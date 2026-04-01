package wss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestPingIsNotBlockedByQueuedWrites(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	client, err := NewWebSocketClient(ctx, wsURL, &http.Client{}, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer client.Close()

	go func() {
		_ = client.ListenIncomeMsg(1024)
	}()

	started := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- client.enqueueWrite(context.Background(), func(ctx context.Context, conn *websocket.Conn) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer pingCancel()
	if err := client.Ping(pingCtx); err != nil {
		t.Fatalf("expected ping to bypass queued writes, got %v", err)
	}

	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("queued write returned unexpected error: %v", err)
	}
}
