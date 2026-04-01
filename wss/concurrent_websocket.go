package wss

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/segmentio/ksuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const (
	defaultWebSocketWriteQueueSize = 128
	defaultWebSocketWriteTimeout   = 15 * time.Second
)

var ErrWebSocketClosed = errors.New("websocket is closed")

type ConcurrentWebSocketInterface interface {
	WSClose() error
	WriteWSJSON(data interface{}) error
}

type wsWriteRequest struct {
	ctx  context.Context
	fn   func(context.Context, *websocket.Conn) error
	done chan error
}

// ConcurrentWebSocket serializes every outbound write onto a single goroutine.
type ConcurrentWebSocket struct {
	WsConn *websocket.Conn

	writeQueue chan wsWriteRequest
	closeCh    chan struct{}
	loopDone   chan struct{}

	closeOnce sync.Once

	errMu    sync.RWMutex
	writeErr error
}

func NewConcurrentWebSocket(conn *websocket.Conn) ConcurrentWebSocket {
	return ConcurrentWebSocket{
		WsConn:     conn,
		writeQueue: make(chan wsWriteRequest, defaultWebSocketWriteQueueSize),
		closeCh:    make(chan struct{}),
		loopDone:   make(chan struct{}),
	}
}

func (wsc *ConcurrentWebSocket) start() {
	go wsc.writeLoop()
}

func (wsc *ConcurrentWebSocket) writeLoop() {
	defer close(wsc.loopDone)
	for {
		select {
		case <-wsc.closeCh:
			wsc.drainPendingWrites(wsc.getClosedErr())
			return
		case req := <-wsc.writeQueue:
			if err := wsc.getClosedErr(); err != nil {
				wsc.completeWrite(req, err)
				wsc.drainPendingWrites(err)
				return
			}
			err := req.fn(req.ctx, wsc.WsConn)
			if err != nil {
				wsc.setWriteErr(err)
				wsc.close()
			}
			wsc.completeWrite(req, err)
		}
	}
}

func (wsc *ConcurrentWebSocket) drainPendingWrites(err error) {
	for {
		select {
		case req := <-wsc.writeQueue:
			wsc.completeWrite(req, err)
		default:
			return
		}
	}
}

func (wsc *ConcurrentWebSocket) setWriteErr(err error) {
	if err == nil {
		return
	}
	wsc.errMu.Lock()
	defer wsc.errMu.Unlock()
	if wsc.writeErr == nil {
		wsc.writeErr = err
	}
}

func (wsc *ConcurrentWebSocket) getWriteErr() error {
	wsc.errMu.RLock()
	defer wsc.errMu.RUnlock()
	return wsc.writeErr
}

func (wsc *ConcurrentWebSocket) getClosedErr() error {
	select {
	case <-wsc.closeCh:
		if err := wsc.getWriteErr(); err != nil {
			return err
		}
		return ErrWebSocketClosed
	default:
		return wsc.getWriteErr()
	}
}

func (wsc *ConcurrentWebSocket) completeWrite(req wsWriteRequest, err error) {
	select {
	case req.done <- err:
	default:
	}
}

func (wsc *ConcurrentWebSocket) close() {
	wsc.closeOnce.Do(func() {
		close(wsc.closeCh)
	})
}

// close websocket connection
func (wsc *ConcurrentWebSocket) WSClose() error {
	wsc.setWriteErr(ErrWebSocketClosed)
	wsc.close()
	return wsc.WsConn.Close(websocket.StatusNormalClosure, "")
}

func (wsc *ConcurrentWebSocket) enqueueWrite(ctx context.Context, fn func(context.Context, *websocket.Conn) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := wsc.getClosedErr(); err != nil {
		return err
	}

	req := wsWriteRequest{
		ctx:  ctx,
		fn:   fn,
		done: make(chan error, 1),
	}
	select {
	case <-wsc.closeCh:
		return wsc.getClosedErr()
	case <-ctx.Done():
		return ctx.Err()
	case wsc.writeQueue <- req:
	}

	select {
	case err := <-req.done:
		return err
	case <-wsc.loopDone:
		return wsc.getClosedErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (wsc *ConcurrentWebSocket) enqueueWriteTimeout(fn func(context.Context, *websocket.Conn) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultWebSocketWriteTimeout)
	defer cancel()
	return wsc.enqueueWrite(ctx, fn)
}

func (wsc *ConcurrentWebSocket) WriteWSJSON(data interface{}) error {
	return wsc.enqueueWriteTimeout(func(ctx context.Context, conn *websocket.Conn) error {
		return wsjson.Write(ctx, conn, data)
	})
}

func (wsc *ConcurrentWebSocket) Ping(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := wsc.getClosedErr(); err != nil {
		return err
	}
	return wsc.WsConn.Ping(ctx)
}

// write message to websocket, the data is fixed format @ProxyData
// id: connection id
// data: data to be written
func (wsc *ConcurrentWebSocket) WriteProxyMessage(ctx context.Context, id ksuid.KSUID, tag int, data []byte) error {
	dataBase64 := base64.StdEncoding.EncodeToString(data)
	jsonData := WebSocketMessage{
		Id:   id.String(),
		Type: WsTpData,
		Data: ProxyData{Tag: tag, DataBase64: dataBase64},
	}
	return wsc.enqueueWrite(ctx, func(ctx context.Context, conn *websocket.Conn) error {
		return wsjson.Write(ctx, conn, &jsonData)
	})
}

type webSocketWriter struct {
	WSC  *ConcurrentWebSocket
	Id   ksuid.KSUID // connection id.
	Ctx  context.Context
	Type int // type of trans data.
	Mu   *sync.Mutex
}

func NewWebSocketWriter(wsc *ConcurrentWebSocket, id ksuid.KSUID, ctx context.Context) *webSocketWriter {
	return &webSocketWriter{WSC: wsc, Id: id, Ctx: ctx}
}

func NewWebSocketWriterWithMutex(wsc *ConcurrentWebSocket, id ksuid.KSUID, ctx context.Context) *webSocketWriter {
	return &webSocketWriter{WSC: wsc, Id: id, Ctx: ctx, Mu: &sync.Mutex{}}
}

func (writer *webSocketWriter) CloseWsWriter(cancel context.CancelFunc) {
	if writer.Mu != nil {
		writer.Mu.Lock()
		defer writer.Mu.Unlock()
	}
	cancel()
}

func (writer *webSocketWriter) Write(buffer []byte) (n int, err error) {
	if writer.Mu != nil {
		writer.Mu.Lock()
		defer writer.Mu.Unlock()
	}
	// make sure context is not Canceled/DeadlineExceeded before Write.
	if writer.Ctx.Err() != nil {
		return 0, writer.Ctx.Err()
	}
	if err := writer.WSC.WriteProxyMessage(writer.Ctx, writer.Id, TagData, buffer); err != nil {
		return 0, err
	}
	return len(buffer), nil
}
