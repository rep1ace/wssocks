package wss

import (
	"context"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"nhooyr.io/websocket"
)

type WebsocksServerConfig struct {
	EnableHttp       bool
	EnableConnKey    bool   // bale connection key
	ConnKey          string // connection key
	EnableStatusPage bool   // enable/disable status page
}

type ServerWS struct {
	config       WebsocksServerConfig
	hc           *HubCollection
	httpSessions *httpPollSessionStore
}

// return a a function handling websocket requests from the peer.
func NewServeWS(hc *HubCollection, config WebsocksServerConfig) *ServerWS {
	return &ServerWS{config: config, hc: hc, httpSessions: newHTTPPollSessionStore()}
}

func (s *ServerWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// check connection key
	if s.config.EnableConnKey && r.Header.Get("Key") != s.config.ConnKey {
		w.WriteHeader(401)
		w.Write([]byte("Access denied!\n"))
		return
	}

	if isHTTPPollRequest(r) {
		s.serveHTTPPoll(w, r)
		return
	}

	wc, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Error(err)
		return
	}
	defer wc.Close(websocket.StatusNormalClosure, "the sky is falling")
	s.serveConn(r.Context(), wc)
}

func (s *ServerWS) serveConn(parent context.Context, conn messageConn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// negotiate version with client.
	if err := NegVersionServer(ctx, conn, s.config.EnableStatusPage); err != nil {
		return
	}

	hub := s.hc.NewHub(conn)
	defer s.hc.RemoveProxy(hub.id)
	defer hub.Close()
	// read messages from webSocket
	if limiter, ok := conn.(interface{ SetReadLimit(int64) }); ok {
		limiter.SetReadLimit(1 << 23) // 8 MiB
	}
	for {
		msgType, p, err := conn.Read(ctx) // fixme context
		// if WebSocket is closed by some reason, then this func will return,
		// and 'done' channel will be set, the outer func will reach to the end.
		if err != nil && err != io.EOF {
			log.Error("error reading webSocket message:", err)
			break
		}
		if err = dispatchMessage(hub, msgType, p, s.config); err != nil {
			log.Error("error proxy:", err)
			// break skip error
		}
	}
}
