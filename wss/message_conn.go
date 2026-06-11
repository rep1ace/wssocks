package wss

import (
	"context"
	"encoding/json"
	"fmt"

	"nhooyr.io/websocket"
)

type messageConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
	Ping(ctx context.Context) error
}

func writeJSONMessage(ctx context.Context, conn messageConn, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

func readJSONMessage(ctx context.Context, conn messageConn, data interface{}) error {
	msgType, payload, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	if msgType != websocket.MessageText {
		return fmt.Errorf("expected websocket text message, got message type %d", msgType)
	}
	return json.Unmarshal(payload, data)
}
