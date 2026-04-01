package wss

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConcurrentWebSocketSerializesWrites(t *testing.T) {
	wsc := NewConcurrentWebSocket(nil)
	wsc.start()
	defer wsc.close()

	var mu sync.Mutex
	active := 0
	maxActive := 0
	order := make([]int, 0, 8)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := wsc.enqueueWrite(context.Background(), func(ctx context.Context, conn *websocket.Conn) error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				order = append(order, i)
				mu.Unlock()

				time.Sleep(10 * time.Millisecond)

				mu.Lock()
				active--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("unexpected write error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if maxActive != 1 {
		t.Fatalf("expected serialized writes, max concurrent writers = %d", maxActive)
	}
	if len(order) != 8 {
		t.Fatalf("expected 8 writes, got %d", len(order))
	}
}

func TestConcurrentWebSocketRejectsWritesAfterClose(t *testing.T) {
	wsc := NewConcurrentWebSocket(nil)
	wsc.start()
	wsc.setWriteErr(ErrWebSocketClosed)
	wsc.close()

	if err := wsc.Ping(context.Background()); !errors.Is(err, ErrWebSocketClosed) {
		t.Fatalf("expected ErrWebSocketClosed after close, got %v", err)
	}
}

func TestConcurrentWebSocketDrainsPendingWritesOnClose(t *testing.T) {
	wsc := NewConcurrentWebSocket(nil)
	wsc.start()
	defer wsc.close()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- wsc.enqueueWrite(context.Background(), func(ctx context.Context, conn *websocket.Conn) error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted

	secondExecuted := make(chan struct{}, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- wsc.enqueueWrite(context.Background(), func(ctx context.Context, conn *websocket.Conn) error {
			secondExecuted <- struct{}{}
			return nil
		})
	}()

	deadline := time.Now().Add(time.Second)
	for len(wsc.writeQueue) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(wsc.writeQueue) == 0 {
		t.Fatal("expected a pending write to be queued")
	}

	wsc.setWriteErr(ErrWebSocketClosed)
	wsc.close()
	close(releaseFirst)

	if err := <-firstDone; err != nil {
		t.Fatalf("first write returned unexpected error: %v", err)
	}

	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrWebSocketClosed) {
			t.Fatalf("expected pending write to return ErrWebSocketClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending write did not complete after close")
	}

	select {
	case <-secondExecuted:
		t.Fatal("pending write executed after close")
	default:
	}
}
