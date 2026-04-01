package wss

import (
	"errors"
	"testing"
	"time"

	"github.com/segmentio/ksuid"
)

func TestHubCloseStopsConcurrentWebSocketWriter(t *testing.T) {
	hub := &Hub{
		id:                  ksuid.New(),
		ConcurrentWebSocket: NewConcurrentWebSocket(nil),
		connPool:            make(map[ksuid.KSUID]*ProxyServer),
	}
	hub.ConcurrentWebSocket.start()

	hub.Close()

	select {
	case <-hub.closeCh:
	case <-time.After(time.Second):
		t.Fatal("expected hub close to stop websocket writer loop")
	}

	if err := hub.WriteWSJSON(struct{}{}); !errors.Is(err, ErrWebSocketClosed) {
		t.Fatalf("expected ErrWebSocketClosed after hub close, got %v", err)
	}
}
