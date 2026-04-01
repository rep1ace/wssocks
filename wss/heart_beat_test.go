package wss

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/ksuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestHeartBeatKeepsRunningWhileReaderCallbackBlocks(t *testing.T) {
	proxyID := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		go func() {
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		}()

		msg := WebSocketMessage{
			Id:   <-proxyID,
			Type: WsTpData,
			Data: ProxyData{
				Tag:        TagData,
				DataBase64: base64.StdEncoding.EncodeToString([]byte("payload")),
			},
		}
		if err := wsjson.Write(r.Context(), conn, &msg); err != nil {
			t.Errorf("write websocket frame: %v", err)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, err := NewWebSocketClient(ctx, wsURL, &http.Client{}, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	blocked := make(chan struct{})
	callbackStarted := make(chan struct{})
	proxy := client.NewProxy(func(id ksuid.KSUID, data ServerData) {
		close(callbackStarted)
		<-blocked
	}, func(ksuid.KSUID, bool) {}, func(ksuid.KSUID, error) {})
	proxyID <- proxy.Id.String()

	go func() {
		_ = client.ListenIncomeMsg(1024)
	}()

	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("proxy callback did not block the reader loop")
	}

	hb, hbCtx := NewHeartBeat(client)
	hbErr := make(chan error, 1)
	go func() {
		hbErr <- hb.StartWithInterval(hbCtx, 10*time.Millisecond, 20*time.Millisecond)
	}()

	select {
	case err := <-hbErr:
		t.Fatalf("heartbeat stopped while reader callback was blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(blocked)
	hb.Close()
	select {
	case err := <-hbErr:
		if err != nil {
			t.Fatalf("heartbeat returned unexpected error on shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop after Close")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
}

func TestHeartBeatFailsWhenConnectionCloses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		time.AfterFunc(30*time.Millisecond, func() {
			_ = conn.Close(websocket.StatusGoingAway, "")
		})

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

	hb, hbCtx := NewHeartBeat(client)
	err = hb.StartWithInterval(hbCtx, 10*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected heartbeat error after websocket close")
	}
}
