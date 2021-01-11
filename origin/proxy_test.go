package origin

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/cloudflare/cloudflared/logger"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/h2mux"
	"github.com/cloudflare/cloudflared/hello"
	"github.com/cloudflare/cloudflared/ingress"
	tunnelpogs "github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/cloudflare/cloudflared/websocket"
	"github.com/urfave/cli/v2"

	"github.com/gobwas/ws/wsutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testTags = []tunnelpogs.Tag(nil)
)

type mockHTTPRespWriter struct {
	*httptest.ResponseRecorder
}

func newMockHTTPRespWriter() *mockHTTPRespWriter {
	return &mockHTTPRespWriter{
		httptest.NewRecorder(),
	}
}

func (w *mockHTTPRespWriter) WriteRespHeaders(status int, header http.Header) error {
	w.WriteHeader(status)
	for header, val := range header {
		w.Header()[header] = val
	}
	return nil
}

func (w *mockHTTPRespWriter) WriteErrorResponse() {
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("http response error"))
}

func (w *mockHTTPRespWriter) Read(data []byte) (int, error) {
	return 0, fmt.Errorf("mockHTTPRespWriter doesn't implement io.Reader")
}

type mockWSRespWriter struct {
	*mockHTTPRespWriter
	writeNotification chan []byte
	reader            io.Reader
}

func newMockWSRespWriter(reader io.Reader) *mockWSRespWriter {
	return &mockWSRespWriter{
		newMockHTTPRespWriter(),
		make(chan []byte),
		reader,
	}
}

func (w *mockWSRespWriter) Write(data []byte) (int, error) {
	w.writeNotification <- data
	return len(data), nil
}

func (w *mockWSRespWriter) respBody() io.ReadWriter {
	data := <-w.writeNotification
	return bytes.NewBuffer(data)
}

func (w *mockWSRespWriter) Read(data []byte) (int, error) {
	return w.reader.Read(data)
}

type mockSSERespWriter struct {
	*mockHTTPRespWriter
	writeNotification chan []byte
}

func newMockSSERespWriter() *mockSSERespWriter {
	return &mockSSERespWriter{
		newMockHTTPRespWriter(),
		make(chan []byte),
	}
}

func (w *mockSSERespWriter) Write(data []byte) (int, error) {
	w.writeNotification <- data
	return len(data), nil
}

func (w *mockSSERespWriter) ReadBytes() []byte {
	return <-w.writeNotification
}

func TestProxySingleOrigin(t *testing.T) {
	log := zerolog.Nop()

	ctx, cancel := context.WithCancel(context.Background())

	flagSet := flag.NewFlagSet(t.Name(), flag.PanicOnError)
	flagSet.Bool("hello-world", true, "")

	cliCtx := cli.NewContext(cli.NewApp(), flagSet, nil)
	err := cliCtx.Set("hello-world", "true")
	require.NoError(t, err)

	allowURLFromArgs := false
	ingressRule, err := ingress.NewSingleOrigin(cliCtx, allowURLFromArgs)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errC := make(chan error)
	require.NoError(t, ingressRule.StartOrigins(&wg, &log, ctx.Done(), errC))

	proxy := NewOriginProxy(ingressRule, testTags, &log)
	t.Run("testProxyHTTP", testProxyHTTP(t, proxy))
	t.Run("testProxyWebsocket", testProxyWebsocket(t, proxy))
	t.Run("testProxySSE", testProxySSE(t, proxy))
	cancel()
	wg.Wait()
}

func testProxyHTTP(t *testing.T, proxy connection.OriginProxy) func(t *testing.T) {
	return func(t *testing.T) {
		respWriter := newMockHTTPRespWriter()
		req, err := http.NewRequest(http.MethodGet, "http://localhost:8080", nil)
		require.NoError(t, err)

		err = proxy.Proxy(respWriter, req, connection.TypeHTTP)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, respWriter.Code)
	}
}

func testProxyWebsocket(t *testing.T, proxy connection.OriginProxy) func(t *testing.T) {
	return func(t *testing.T) {
		// WSRoute is a websocket echo handler
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost:8080%s", hello.WSRoute), nil)

		readPipe, writePipe := io.Pipe()
		respWriter := newMockWSRespWriter(readPipe)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = proxy.Proxy(respWriter, req, connection.TypeWebsocket)
			require.NoError(t, err)

			require.Equal(t, http.StatusSwitchingProtocols, respWriter.Code)
		}()

		msg := []byte("test websocket")
		err = wsutil.WriteClientText(writePipe, msg)
		require.NoError(t, err)

		// ReadServerText reads next data message from rw, considering that caller represents proxy side.
		returnedMsg, err := wsutil.ReadServerText(respWriter.respBody())
		require.NoError(t, err)
		require.Equal(t, msg, returnedMsg)

		err = wsutil.WriteClientBinary(writePipe, msg)
		require.NoError(t, err)

		returnedMsg, err = wsutil.ReadServerBinary(respWriter.respBody())
		require.NoError(t, err)
		require.Equal(t, msg, returnedMsg)

		cancel()
		wg.Wait()
	}
}

func testProxySSE(t *testing.T, proxy connection.OriginProxy) func(t *testing.T) {
	return func(t *testing.T) {
		var (
			pushCount = 50
			pushFreq  = time.Millisecond * 10
		)
		respWriter := newMockSSERespWriter()
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost:8080%s?freq=%s", hello.SSERoute, pushFreq), nil)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = proxy.Proxy(respWriter, req, connection.TypeHTTP)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, respWriter.Code)
		}()

		for i := 0; i < pushCount; i++ {
			line := respWriter.ReadBytes()
			expect := fmt.Sprintf("%d\n", i)
			require.Equal(t, []byte(expect), line, fmt.Sprintf("Expect to read %v, got %v", expect, line))

			line = respWriter.ReadBytes()
			require.Equal(t, []byte("\n"), line, fmt.Sprintf("Expect to read '\n', got %v", line))
		}

		cancel()
		wg.Wait()
	}
}

func TestProxyMultipleOrigins(t *testing.T) {
	api := httptest.NewServer(mockAPI{})
	defer api.Close()

	unvalidatedIngress := []config.UnvalidatedIngressRule{
		{
			Hostname: "api.example.com",
			Service:  api.URL,
		},
		{
			Hostname: "hello.example.com",
			Service:  "hello-world",
		},
		{
			Hostname: "health.example.com",
			Path:     "/health",
			Service:  "http_status:200",
		},
		{
			Hostname: "*",
			Service:  "http_status:404",
		},
	}

	ingress, err := ingress.ParseIngress(&config.Configuration{
		TunnelID: t.Name(),
		Ingress:  unvalidatedIngress,
	})
	require.NoError(t, err)

	log := zerolog.Nop()

	ctx, cancel := context.WithCancel(context.Background())
	errC := make(chan error)
	var wg sync.WaitGroup
	require.NoError(t, ingress.StartOrigins(&wg, &log, ctx.Done(), errC))

	proxy := NewOriginProxy(ingress, testTags, &log)

	tests := []struct {
		url            string
		expectedStatus int
		expectedBody   []byte
	}{
		{
			url:            "http://api.example.com",
			expectedStatus: http.StatusCreated,
			expectedBody:   []byte("Created"),
		},
		{
			url:            fmt.Sprintf("http://hello.example.com%s", hello.HealthRoute),
			expectedStatus: http.StatusOK,
			expectedBody:   []byte("ok"),
		},
		{
			url:            "http://health.example.com/health",
			expectedStatus: http.StatusOK,
		},
		{
			url:            "http://health.example.com/",
			expectedStatus: http.StatusNotFound,
		},
		{
			url:            "http://not-found.example.com",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		respWriter := newMockHTTPRespWriter()
		req, err := http.NewRequest(http.MethodGet, test.url, nil)
		require.NoError(t, err)

		err = proxy.Proxy(respWriter, req, connection.TypeHTTP)
		require.NoError(t, err)

		assert.Equal(t, test.expectedStatus, respWriter.Code)
		if test.expectedBody != nil {
			assert.Equal(t, test.expectedBody, respWriter.Body.Bytes())
		} else {
			assert.Equal(t, 0, respWriter.Body.Len())
		}
	}
	cancel()
	wg.Wait()
}

type mockAPI struct{}

func (ma mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte("Created"))
}

type errorOriginTransport struct{}

func (errorOriginTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("Proxy error")
}

func TestProxyError(t *testing.T) {
	ingress := ingress.Ingress{
		Rules: []ingress.Rule{
			{
				Hostname: "*",
				Path:     nil,
				Service: ingress.MockOriginHTTPService{
					Transport: errorOriginTransport{},
				},
			},
		},
	}

	log := zerolog.Nop()

	proxy := NewOriginProxy(ingress, testTags, &log)

	respWriter := newMockHTTPRespWriter()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1", nil)
	assert.NoError(t, err)

	err = proxy.Proxy(respWriter, req, connection.TypeHTTP)
	assert.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, respWriter.Code)
	assert.Equal(t, "http response error", respWriter.Body.String())
}

func TestProxyBastionMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	flagSet := flag.NewFlagSet(t.Name(), flag.PanicOnError)
	flagSet.Bool("bastion", true, "")

	cliCtx := cli.NewContext(cli.NewApp(), flagSet, nil)
	err := cliCtx.Set(config.BastionFlag, "true")
	require.NoError(t, err)

	allowURLFromArgs := false
	ingressRule, err := ingress.NewSingleOrigin(cliCtx, allowURLFromArgs)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errC := make(chan error)

	log := logger.Create(nil)

	ingressRule.StartOrigins(&wg, log, ctx.Done(), errC)

	proxy := NewOriginProxy(ingressRule, testTags, log)

	t.Run("testBastionWebsocket", testBastionWebsocket(proxy))
	cancel()
}

func testBastionWebsocket(proxy connection.OriginProxy) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		readPipe, _ := io.Pipe()
		respWriter := newMockWSRespWriter(readPipe)

		var wg sync.WaitGroup
		msgFromConn := []byte("data from websocket proxy")
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ln.Close()
			conn, err := ln.Accept()
			require.NoError(t, err)
			wsConn := websocket.NewConn(conn, nil)
			wsConn.Write(msgFromConn)
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dummy", nil)
		req.Header.Set(h2mux.CFJumpDestinationHeader, ln.Addr().String())

		wg.Add(1)
		go func() {
			defer wg.Done()
			err = proxy.Proxy(respWriter, req, connection.TypeWebsocket)
			require.NoError(t, err)

			require.Equal(t, http.StatusSwitchingProtocols, respWriter.Code)
		}()

		// ReadServerText reads next data message from rw, considering that caller represents proxy side.
		returnedMsg, err := wsutil.ReadServerText(respWriter.respBody())
		if err != io.EOF {
			require.NoError(t, err)
			require.Equal(t, msgFromConn, returnedMsg)
		}

		cancel()
		wg.Wait()
	}
}

func TestTCPStream(t *testing.T) {
	logger := logger.Create(nil)

	ctx, cancel := context.WithCancel(context.Background())

	ingressConfig := &config.Configuration{
		Ingress: []config.UnvalidatedIngressRule{
			config.UnvalidatedIngressRule{
				Hostname: "*",
				Service:  ingress.ServiceTeamnet,
			},
		},
	}
	ingressRule, err := ingress.ParseIngress(ingressConfig)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errC := make(chan error)
	ingressRule.StartOrigins(&wg, logger, ctx.Done(), errC)

	proxy := NewOriginProxy(ingressRule, testTags, logger)

	t.Run("testTCPStream", testTCPStreamProxy(proxy))
	cancel()
	wg.Wait()
}

type mockTCPRespWriter struct {
	w    io.Writer
	code int
}

func (m *mockTCPRespWriter) Read(p []byte) (n int, err error) {
	return len(p), nil
}

func (m *mockTCPRespWriter) Write(p []byte) (n int, err error) {
	return m.w.Write(p)
}

func (m *mockTCPRespWriter) WriteErrorResponse() {
}

func (m *mockTCPRespWriter) WriteRespHeaders(status int, header http.Header) error {
	m.code = status
	return nil
}

func testTCPStreamProxy(proxy connection.OriginProxy) func(t *testing.T) {
	return func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		readPipe, writePipe := io.Pipe()
		respWriter := &mockTCPRespWriter{
			w: writePipe,
		}
		msgFromConn := []byte("data from tcp proxy")
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		go func() {
			defer ln.Close()
			conn, err := ln.Accept()
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.Write(msgFromConn)
			require.NoError(t, err)
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dummy", nil)
		require.NoError(t, err)

		req.Header.Set("Cf-Cloudflared-Proxy-Src", "non-blank-value")
		req.Host = ln.Addr().String()
		err = proxy.Proxy(respWriter, req, connection.TypeTCP)
		require.NoError(t, err)

		require.Equal(t, http.StatusSwitchingProtocols, respWriter.code)

		returnedMsg := make([]byte, len(msgFromConn))

		_, err = readPipe.Read(returnedMsg)

		require.NoError(t, err)
		require.Equal(t, msgFromConn, returnedMsg)

		cancel()
	}
}
