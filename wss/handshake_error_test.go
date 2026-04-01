package wss

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewWebSocketClientReturnsHandshakeErrorForHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := NewWebSocketClient(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &http.Client{}, nil)
	if err == nil {
		t.Fatal("expected websocket dial to fail")
	}

	var handshakeErr *HandshakeError
	if !errors.As(err, &handshakeErr) {
		t.Fatalf("expected HandshakeError, got %T", err)
	}
	if handshakeErr.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status code: got %d want %d", handshakeErr.StatusCode, http.StatusForbidden)
	}
	if handshakeErr.FinalPath != "/" {
		t.Fatalf("unexpected final path: got %q want %q", handshakeErr.FinalPath, "/")
	}
}

func TestNewWebSocketClientCapturesRedirectedLoginPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("login"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := NewWebSocketClient(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &http.Client{}, nil)
	if err == nil {
		t.Fatal("expected websocket dial to fail")
	}

	var handshakeErr *HandshakeError
	if !errors.As(err, &handshakeErr) {
		t.Fatalf("expected HandshakeError, got %T", err)
	}
	if handshakeErr.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", handshakeErr.StatusCode, http.StatusOK)
	}
	if handshakeErr.FinalPath != "/login" {
		t.Fatalf("unexpected final path: got %q want %q", handshakeErr.FinalPath, "/login")
	}
}
