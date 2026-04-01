package wss

import (
	"context"
	"time"

	"github.com/segmentio/ksuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type HeartBeat struct {
	wsc      *WebSocketClient
	cancel   context.CancelFunc
	isClosed bool
}

func NewHeartBeat(wsc *WebSocketClient) (*HeartBeat, context.Context) {
	hb := HeartBeat{wsc: wsc, isClosed: false}
	ctx, can := context.WithCancel(context.Background())

	hb.cancel = can
	return &hb, ctx
}

// close heartbeat sending
func (hb *HeartBeat) Close() {
	if hb.isClosed {
		return
	}
	hb.isClosed = true
	hb.cancel()
}

// start sending heart beat to server.
func (hb *HeartBeat) Start(ctx context.Context, writeTimeout time.Duration) error {
	return hb.StartWithInterval(ctx, time.Second*15, writeTimeout)
}

func (hb *HeartBeat) StartWithInterval(ctx context.Context, interval, writeTimeout time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			heartBeats := WebSocketMessage{
				Id:   ksuid.KSUID{}.String(),
				Type: WsTpBeats,
				Data: nil,
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := hb.wsc.enqueueWrite(writeCtx, func(ctx context.Context, conn *websocket.Conn) error {
				return wsjson.Write(ctx, conn, &heartBeats)
			})
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}
