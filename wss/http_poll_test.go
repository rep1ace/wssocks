package wss

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/ksuid"
)

func TestNewWebSocketClientFallsBackToHTTPPollAfterUpgradeRequired(t *testing.T) {
	outer := newHTTPPollFallbackTestServer(WebsocksServerConfig{})
	defer outer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, err := NewWebSocketClient(ctx, "ws"+strings.TrimPrefix(outer.URL, "http"), &http.Client{}, nil)
	if err != nil {
		t.Fatalf("connect with http poll fallback: %v", err)
	}
	defer client.Close()

	version, err := ExchangeVersion(ctx, client.WsConn)
	if err != nil {
		t.Fatalf("exchange version over http poll: %v", err)
	}
	if version.VersionCode != VersionCode {
		t.Fatalf("unexpected version code: got %d want %d", version.VersionCode, VersionCode)
	}
}

func TestHTTPPollURLAddsTrailingSlashBeforeQuery(t *testing.T) {
	got, err := httpPollURL("wss://webvpn.example/wss-1088/encrypted-host")
	if err != nil {
		t.Fatalf("http poll url: %v", err)
	}
	want := "https://webvpn.example/wss-1088/encrypted-host/"
	if got != want {
		t.Fatalf("unexpected http poll url: got %q want %q", got, want)
	}
}

func TestHTTPPollFallbackRejectsNonTunnelResponse(t *testing.T) {
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHTTPPollRequest(r) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>not a tunnel</html>"))
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	defer outer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := NewWebSocketClient(ctx, "ws"+strings.TrimPrefix(outer.URL, "http"), &http.Client{}, nil)
	if err == nil || !strings.Contains(err.Error(), "non-tunnel response") {
		t.Fatalf("expected non-tunnel fallback error, got %v", err)
	}
}

func TestHTTPPollReadSkipsEmptyOKResponse(t *testing.T) {
	recvCount := 0
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHTTPPollRequest(r) {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}

		w.Header().Set(httpPollResponseHeader, httpPollTransportName)
		switch r.URL.Query().Get(httpPollActionKey) {
		case httpPollActionOpen:
			w.WriteHeader(http.StatusNoContent)
		case httpPollActionRecv:
			recvCount++
			if recvCount == 1 {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("message-after-empty-ok"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer outer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	conn, err := newHTTPPollClientConn(ctx, "ws"+strings.TrimPrefix(outer.URL, "http"), outer.Client(), nil)
	if err != nil {
		t.Fatalf("new http poll conn: %v", err)
	}
	_, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read after empty ok: %v", err)
	}
	if string(body) != "message-after-empty-ok" {
		t.Fatalf("unexpected body %q", string(body))
	}
}

func TestHTTPPollFallbackCarriesProxyTraffic(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	outer := newHTTPPollFallbackTestServer(WebsocksServerConfig{EnableHttp: true})
	defer outer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := NewWebSocketClient(ctx, "ws"+strings.TrimPrefix(outer.URL, "http"), &http.Client{}, nil)
	if err != nil {
		t.Fatalf("connect with http poll fallback: %v", err)
	}
	defer client.Close()
	if _, err := ExchangeVersion(ctx, client.WsConn); err != nil {
		t.Fatalf("exchange version over http poll: %v", err)
	}

	dataCh := make(chan []byte, 4)
	errCh := make(chan error, 2)
	proxy := client.NewProxy(func(id ksuid.KSUID, data ServerData) {
		dataCh <- data.Data
	}, func(ksuid.KSUID, bool) {}, func(_ ksuid.KSUID, err error) {
		errCh <- err
	})

	listenDone := make(chan error, 1)
	go func() {
		listenDone <- client.ListenIncomeMsg(1024 * 1024)
	}()

	if err := proxy.Establish(client, nil, ProxyTypeSocks5, echoAddr); err != nil {
		t.Fatalf("establish proxy: %v", err)
	}
	waitForPayload(t, dataCh, []byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

	if err := client.WriteProxyMessage(ctx, proxy.Id, TagData, []byte("ping-over-http-poll")); err != nil {
		t.Fatalf("write proxy message: %v", err)
	}
	waitForPayload(t, dataCh, []byte("ping-over-http-poll"))

	largePayload := []byte(strings.Repeat("chunked-http-poll-payload-", 200))
	if err := client.WriteProxyMessage(ctx, proxy.Id, TagData, largePayload); err != nil {
		t.Fatalf("write chunked proxy message: %v", err)
	}
	waitForPayload(t, dataCh, largePayload)

	select {
	case err := <-errCh:
		t.Fatalf("proxy callback error: %v", err)
	default:
	}
	_ = client.Close()
	select {
	case <-listenDone:
	case <-time.After(time.Second):
		t.Fatal("listener did not stop after client close")
	}
}

func newHTTPPollFallbackTestServer(config WebsocksServerConfig) *httptest.Server {
	backend := NewServeWS(NewHubCollection(), config)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHTTPPollRequest(r) {
			backend.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
}

func startTCPEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp echo: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String(), func() {
		_ = listener.Close()
		<-done
	}
}

func waitForPayload(t *testing.T, ch <-chan []byte, want []byte) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case got := <-ch:
			if string(got) == string(want) {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for payload %q", string(want))
		}
	}
}
