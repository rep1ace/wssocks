package wss

import "sync"

type WebSocketClientProvider interface {
	Current() *WebSocketClient
}

type StaticWebSocketClientProvider struct {
	client *WebSocketClient
}

func NewStaticWebSocketClientProvider(client *WebSocketClient) *StaticWebSocketClientProvider {
	return &StaticWebSocketClientProvider{client: client}
}

func (p *StaticWebSocketClientProvider) Current() *WebSocketClient {
	return p.client
}

type SwappableWebSocketClientProvider struct {
	mu      sync.RWMutex
	current *WebSocketClient
}

func NewSwappableWebSocketClientProvider() *SwappableWebSocketClientProvider {
	return &SwappableWebSocketClientProvider{}
}

func (p *SwappableWebSocketClientProvider) Current() *WebSocketClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current
}

func (p *SwappableWebSocketClientProvider) Set(client *WebSocketClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = client
}
