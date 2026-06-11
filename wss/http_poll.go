package wss

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/ksuid"
	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
)

const (
	httpPollTransportName = "http-poll"
	httpPollQueryKey      = "wssocks_transport"
	httpPollSessionKey    = "wssocks_session"
	httpPollActionKey     = "wssocks_action"
	httpPollMessageKey    = "wssocks_message"
	httpPollPartKey       = "wssocks_part"
	httpPollPartsKey      = "wssocks_parts"
	httpPollDataKey       = "wssocks_data"
	httpPollTypeKey       = "wssocks_type"
	httpPollMessageType   = "X-WSSocks-Message-Type"
	httpPollHeaderMessage = "X-WSSocks-Message"
	httpPollHeaderPart    = "X-WSSocks-Part"
	httpPollHeaderParts   = "X-WSSocks-Parts"
	httpPollHeaderData    = "X-WSSocks-Data"

	httpPollActionOpen  = "open"
	httpPollActionSend  = "send"
	httpPollActionRecv  = "recv"
	httpPollActionClose = "close"

	httpPollResponseHeader = "X-WSSocks-Transport"

	httpPollRecvTimeout = 25 * time.Second
	httpPollMaxMessage  = 1 << 23
	httpPollQueryChunk  = 1400
	httpPollAssemblyTTL = 2 * time.Minute
)

var errHTTPPollClosed = errors.New("http poll connection is closed")

type httpPollMessage struct {
	typ  websocket.MessageType
	data []byte
}

type httpPollClientConn struct {
	client  *http.Client
	baseURL string
	header  http.Header
	session string

	closeOnce sync.Once
	closeCh   chan struct{}
}

func newHTTPPollClientConn(ctx context.Context, rawURL string, client *http.Client, header http.Header) (*httpPollClientConn, error) {
	if client == nil {
		client = http.DefaultClient
	}
	baseURL, err := httpPollURL(rawURL)
	if err != nil {
		return nil, err
	}
	conn := &httpPollClientConn{
		client:  client,
		baseURL: baseURL,
		header:  cloneHeader(header),
		session: ksuid.New().String(),
		closeCh: make(chan struct{}),
	}
	if err := conn.doControl(ctx, httpPollActionOpen); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *httpPollClientConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	var assembly *httpPollIncomingAssembly
	var assemblingID string

	for {
		if err := c.closedErr(); err != nil {
			return 0, nil, err
		}

		reqCtx, cancel := context.WithTimeout(ctx, httpPollRecvTimeout+10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.actionURL(httpPollActionRecv), nil)
		if err != nil {
			cancel()
			return 0, nil, err
		}
		req.Header = c.header.Clone()
		resp, err := c.client.Do(req)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, httpPollMaxMessage+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return 0, nil, readErr
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if resp.Header.Get(httpPollResponseHeader) != httpPollTransportName {
				return 0, nil, fmt.Errorf("http poll recv got non-tunnel response from %s with content-type %q", resp.Request.URL.String(), resp.Header.Get("Content-Type"))
			}
			if resp.Header.Get(httpPollHeaderData) != "" || resp.Header.Get(httpPollHeaderMessage) != "" {
				msg, ok, err := acceptHeaderPart(resp.Header, &assembly, &assemblingID)
				if err != nil {
					return 0, nil, err
				}
				if !ok {
					continue
				}
				return msg.typ, msg.data, nil
			}
			if len(body) == 0 {
				continue
			}
			if len(body) > httpPollMaxMessage {
				return 0, nil, fmt.Errorf("http poll message exceeds %d bytes", httpPollMaxMessage)
			}
			msgType := websocket.MessageText
			if value := resp.Header.Get(httpPollMessageType); value != "" {
				if parsed, err := strconv.Atoi(value); err == nil {
					msgType = websocket.MessageType(parsed)
				}
			}
			return msgType, body, nil
		case http.StatusNoContent:
			if resp.Header.Get(httpPollResponseHeader) != httpPollTransportName {
				return 0, nil, fmt.Errorf("http poll recv got non-tunnel empty response from %s", resp.Request.URL.String())
			}
			continue
		case http.StatusGone, http.StatusNotFound:
			return 0, nil, io.EOF
		default:
			return 0, nil, fmt.Errorf("http poll recv failed with status %d: %s", resp.StatusCode, string(body))
		}
	}
}

func (c *httpPollClientConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	if err := c.closedErr(); err != nil {
		return err
	}
	if len(p) > httpPollMaxMessage {
		return fmt.Errorf("http poll message exceeds %d bytes", httpPollMaxMessage)
	}

	encoded := base64.RawURLEncoding.EncodeToString(p)
	parts := (len(encoded) + httpPollQueryChunk - 1) / httpPollQueryChunk
	if parts == 0 {
		parts = 1
	}
	msgID := ksuid.New().String()
	for part := 0; part < parts; part++ {
		start := part * httpPollQueryChunk
		end := start + httpPollQueryChunk
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := ""
		if start < len(encoded) {
			chunk = encoded[start:end]
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sendURL(msgID, typ, part, parts, chunk), nil)
		if err != nil {
			return err
		}
		req.Header = c.header.Clone()
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("http poll send failed with status %d: %s", resp.StatusCode, string(body))
		}
		if resp.Header.Get(httpPollResponseHeader) != httpPollTransportName {
			return fmt.Errorf("http poll send got non-tunnel response from %s with status %d and content-type %q", resp.Request.URL.String(), resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	return nil
}

func (c *httpPollClientConn) Close(code websocket.StatusCode, reason string) error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeCh)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = c.doControl(ctx, httpPollActionClose)
	})
	return err
}

func (c *httpPollClientConn) Ping(ctx context.Context) error {
	return c.closedErr()
}

func (c *httpPollClientConn) actionURL(action string) string {
	u, _ := url.Parse(c.baseURL)
	q := u.Query()
	q.Set(httpPollQueryKey, httpPollTransportName)
	q.Set(httpPollSessionKey, c.session)
	q.Set(httpPollActionKey, action)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *httpPollClientConn) sendURL(msgID string, typ websocket.MessageType, part, parts int, chunk string) string {
	u, _ := url.Parse(c.actionURL(httpPollActionSend))
	q := u.Query()
	q.Set(httpPollMessageKey, msgID)
	q.Set(httpPollTypeKey, strconv.Itoa(int(typ)))
	q.Set(httpPollPartKey, strconv.Itoa(part))
	q.Set(httpPollPartsKey, strconv.Itoa(parts))
	q.Set(httpPollDataKey, chunk)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *httpPollClientConn) doControl(ctx context.Context, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.actionURL(action), nil)
	if err != nil {
		return err
	}
	req.Header = c.header.Clone()
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("http poll %s failed with status %d: %s", action, resp.StatusCode, string(body))
	}
	if resp.Header.Get(httpPollResponseHeader) != httpPollTransportName {
		return fmt.Errorf("http poll %s got non-tunnel response from %s with status %d and content-type %q", action, resp.Request.URL.String(), resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return nil
}

func (c *httpPollClientConn) closedErr() error {
	select {
	case <-c.closeCh:
		return errHTTPPollClosed
	default:
		return nil
	}
}

func httpPollURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "https", "http":
	default:
		return "", fmt.Errorf("unsupported http poll scheme %q", u.Scheme)
	}
	if u.Path != "" && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String(), nil
}

type httpPollServerConn struct {
	incoming chan httpPollMessage
	outgoing chan httpPollMessage

	incomingMu    sync.Mutex
	incomingParts map[string]*httpPollIncomingAssembly
	outgoingMu    sync.Mutex
	outgoingParts *httpPollOutgoingAssembly

	closeOnce sync.Once
	closeCh   chan struct{}
}

type httpPollIncomingAssembly struct {
	typ     websocket.MessageType
	parts   []string
	present []bool
	seen    int
	size    int
	created time.Time
	updated time.Time
}

type httpPollOutgoingAssembly struct {
	messageID string
	typ       websocket.MessageType
	parts     []string
	next      int
}

type httpPollOutgoingPart struct {
	messageID string
	typ       websocket.MessageType
	part      int
	parts     int
	data      string
}

func newHTTPPollServerConn() *httpPollServerConn {
	return &httpPollServerConn{
		incoming:      make(chan httpPollMessage, 256),
		outgoing:      make(chan httpPollMessage, 256),
		incomingParts: make(map[string]*httpPollIncomingAssembly),
		closeCh:       make(chan struct{}),
	}
}

func (c *httpPollServerConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.closeCh:
		return 0, nil, io.EOF
	case msg := <-c.incoming:
		return msg.typ, msg.data, nil
	}
}

func (c *httpPollServerConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	if len(p) > httpPollMaxMessage {
		return fmt.Errorf("http poll message exceeds %d bytes", httpPollMaxMessage)
	}
	msg := httpPollMessage{typ: typ, data: cloneBytes(p)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return errHTTPPollClosed
	case c.outgoing <- msg:
		return nil
	}
}

func (c *httpPollServerConn) Close(code websocket.StatusCode, reason string) error {
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
	return nil
}

func (c *httpPollServerConn) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return errHTTPPollClosed
	default:
		return nil
	}
}

func (c *httpPollServerConn) pushIncoming(ctx context.Context, msg httpPollMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return errHTTPPollClosed
	case c.incoming <- msg:
		return nil
	}
}

func (c *httpPollServerConn) pushIncomingRequest(ctx context.Context, r *http.Request) error {
	if r.URL.Query().Get(httpPollMessageKey) != "" || r.URL.Query().Has(httpPollDataKey) {
		msg, ok, err := c.acceptIncomingPart(r.URL.Query())
		if err != nil || !ok {
			return err
		}
		return c.pushIncoming(ctx, msg)
	}

	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, httpPollMaxMessage+1))
	if err != nil {
		return err
	}
	if len(body) > httpPollMaxMessage {
		return fmt.Errorf("http poll message too large")
	}
	msgType := websocket.MessageText
	if value := r.Header.Get(httpPollMessageType); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			msgType = websocket.MessageType(parsed)
		}
	}
	return c.pushIncoming(ctx, httpPollMessage{typ: msgType, data: body})
}

func (c *httpPollServerConn) acceptIncomingPart(q url.Values) (httpPollMessage, bool, error) {
	msgID := q.Get(httpPollMessageKey)
	if msgID == "" {
		return httpPollMessage{}, false, fmt.Errorf("missing http poll message id")
	}
	part, err := strconv.Atoi(q.Get(httpPollPartKey))
	if err != nil {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll part: %w", err)
	}
	parts, err := strconv.Atoi(q.Get(httpPollPartsKey))
	if err != nil {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll parts: %w", err)
	}
	if parts <= 0 || part < 0 || part >= parts {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll part index %d of %d", part, parts)
	}
	if parts*httpPollQueryChunk > base64.RawURLEncoding.EncodedLen(httpPollMaxMessage)+httpPollQueryChunk {
		return httpPollMessage{}, false, fmt.Errorf("http poll message too large")
	}

	msgType := websocket.MessageText
	if value := q.Get(httpPollTypeKey); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			msgType = websocket.MessageType(parsed)
		}
	}
	chunk := q.Get(httpPollDataKey)
	now := time.Now()

	c.incomingMu.Lock()
	defer c.incomingMu.Unlock()
	c.pruneIncomingPartsLocked(now)

	assembly := c.incomingParts[msgID]
	if assembly == nil {
		assembly = &httpPollIncomingAssembly{
			typ:     msgType,
			parts:   make([]string, parts),
			present: make([]bool, parts),
			created: now,
		}
		c.incomingParts[msgID] = assembly
	}
	if len(assembly.parts) != parts {
		return httpPollMessage{}, false, fmt.Errorf("http poll message part count changed")
	}
	if assembly.typ != msgType {
		return httpPollMessage{}, false, fmt.Errorf("http poll message type changed")
	}
	assembly.updated = now

	if !assembly.present[part] {
		assembly.parts[part] = chunk
		assembly.present[part] = true
		assembly.seen++
		assembly.size += len(chunk)
	} else if assembly.parts[part] != chunk {
		return httpPollMessage{}, false, fmt.Errorf("http poll duplicate part mismatch")
	}
	if assembly.size > base64.RawURLEncoding.EncodedLen(httpPollMaxMessage) {
		delete(c.incomingParts, msgID)
		return httpPollMessage{}, false, fmt.Errorf("http poll message too large")
	}
	if assembly.seen < len(assembly.parts) {
		log.WithFields(log.Fields{
			"message":   msgID,
			"part":      part,
			"parts":     parts,
			"chunk_len": len(chunk),
			"seen":      assembly.seen,
		}).Debug("http poll send part accepted")
		return httpPollMessage{}, false, nil
	}

	var encoded strings.Builder
	encoded.Grow(assembly.size)
	for _, item := range assembly.parts {
		encoded.WriteString(item)
	}
	delete(c.incomingParts, msgID)

	data, err := base64.RawURLEncoding.DecodeString(encoded.String())
	if err != nil {
		return httpPollMessage{}, false, err
	}
	if len(data) > httpPollMaxMessage {
		return httpPollMessage{}, false, fmt.Errorf("http poll message too large")
	}
	log.WithFields(log.Fields{
		"message": msgID,
		"parts":   parts,
		"bytes":   len(data),
	}).Debug("http poll message reassembled")
	return httpPollMessage{typ: assembly.typ, data: data}, true, nil
}

func (c *httpPollServerConn) pruneIncomingPartsLocked(now time.Time) {
	for id, assembly := range c.incomingParts {
		if now.Sub(assembly.updated) > httpPollAssemblyTTL && now.Sub(assembly.created) > httpPollAssemblyTTL {
			delete(c.incomingParts, id)
		}
	}
}

func acceptHeaderPart(header http.Header, assembly **httpPollIncomingAssembly, assemblingID *string) (httpPollMessage, bool, error) {
	msgID := header.Get(httpPollHeaderMessage)
	if msgID == "" {
		return httpPollMessage{}, false, fmt.Errorf("missing http poll response message id")
	}
	part, err := strconv.Atoi(header.Get(httpPollHeaderPart))
	if err != nil {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll response part: %w", err)
	}
	parts, err := strconv.Atoi(header.Get(httpPollHeaderParts))
	if err != nil {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll response parts: %w", err)
	}
	if parts <= 0 || part < 0 || part >= parts {
		return httpPollMessage{}, false, fmt.Errorf("invalid http poll response part index %d of %d", part, parts)
	}

	msgType := websocket.MessageText
	if value := header.Get(httpPollMessageType); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			msgType = websocket.MessageType(parsed)
		}
	}
	chunk := header.Get(httpPollHeaderData)

	if *assembly == nil {
		*assembly = &httpPollIncomingAssembly{
			typ:     msgType,
			parts:   make([]string, parts),
			present: make([]bool, parts),
			created: time.Now(),
		}
		*assemblingID = msgID
	}
	if *assemblingID != msgID {
		return httpPollMessage{}, false, fmt.Errorf("http poll response message changed before completion")
	}
	current := *assembly
	if len(current.parts) != parts {
		return httpPollMessage{}, false, fmt.Errorf("http poll response part count changed")
	}
	if current.typ != msgType {
		return httpPollMessage{}, false, fmt.Errorf("http poll response message type changed")
	}
	current.updated = time.Now()
	if !current.present[part] {
		current.parts[part] = chunk
		current.present[part] = true
		current.seen++
		current.size += len(chunk)
	} else if current.parts[part] != chunk {
		return httpPollMessage{}, false, fmt.Errorf("http poll response duplicate part mismatch")
	}
	if current.size > base64.RawURLEncoding.EncodedLen(httpPollMaxMessage) {
		*assembly = nil
		*assemblingID = ""
		return httpPollMessage{}, false, fmt.Errorf("http poll response message too large")
	}
	if current.seen < len(current.parts) {
		return httpPollMessage{}, false, nil
	}

	var encoded strings.Builder
	encoded.Grow(current.size)
	for _, item := range current.parts {
		encoded.WriteString(item)
	}
	*assembly = nil
	*assemblingID = ""

	data, err := base64.RawURLEncoding.DecodeString(encoded.String())
	if err != nil {
		return httpPollMessage{}, false, err
	}
	if len(data) > httpPollMaxMessage {
		return httpPollMessage{}, false, fmt.Errorf("http poll response message too large")
	}
	return httpPollMessage{typ: current.typ, data: data}, true, nil
}

func splitHTTPPollChunks(encoded string) []string {
	parts := (len(encoded) + httpPollQueryChunk - 1) / httpPollQueryChunk
	if parts == 0 {
		return []string{""}
	}
	out := make([]string, 0, parts)
	for start := 0; start < len(encoded); start += httpPollQueryChunk {
		end := start + httpPollQueryChunk
		if end > len(encoded) {
			end = len(encoded)
		}
		out = append(out, encoded[start:end])
	}
	return out
}

func (c *httpPollServerConn) popOutgoing(ctx context.Context) (httpPollMessage, bool, error) {
	timer := time.NewTimer(httpPollRecvTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return httpPollMessage{}, false, ctx.Err()
	case <-c.closeCh:
		return httpPollMessage{}, false, errHTTPPollClosed
	case msg := <-c.outgoing:
		return msg, true, nil
	case <-timer.C:
		return httpPollMessage{}, false, nil
	}
}

func (c *httpPollServerConn) popOutgoingPart(ctx context.Context) (httpPollOutgoingPart, bool, error) {
	c.outgoingMu.Lock()
	if c.outgoingParts != nil {
		part := c.nextOutgoingPartLocked()
		c.outgoingMu.Unlock()
		return part, true, nil
	}
	c.outgoingMu.Unlock()

	msg, ok, err := c.popOutgoing(ctx)
	if err != nil || !ok {
		return httpPollOutgoingPart{}, ok, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(msg.data)
	parts := splitHTTPPollChunks(encoded)
	if len(parts) == 0 {
		parts = []string{""}
	}

	c.outgoingMu.Lock()
	c.outgoingParts = &httpPollOutgoingAssembly{
		messageID: ksuid.New().String(),
		typ:       msg.typ,
		parts:     parts,
	}
	part := c.nextOutgoingPartLocked()
	c.outgoingMu.Unlock()
	return part, true, nil
}

func (c *httpPollServerConn) nextOutgoingPartLocked() httpPollOutgoingPart {
	out := c.outgoingParts
	part := httpPollOutgoingPart{
		messageID: out.messageID,
		typ:       out.typ,
		part:      out.next,
		parts:     len(out.parts),
		data:      out.parts[out.next],
	}
	out.next++
	if out.next >= len(out.parts) {
		c.outgoingParts = nil
	}
	return part
}

type httpPollSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*httpPollServerConn
}

func newHTTPPollSessionStore() *httpPollSessionStore {
	return &httpPollSessionStore{sessions: make(map[string]*httpPollServerConn)}
}

func (s *httpPollSessionStore) create(id string) (*httpPollServerConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.sessions[id]; old != nil {
		_ = old.Close(websocket.StatusNormalClosure, "replaced")
	}
	conn := newHTTPPollServerConn()
	s.sessions[id] = conn
	return conn, nil
}

func (s *httpPollSessionStore) get(id string) *httpPollServerConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *httpPollSessionStore) remove(id string, conn *httpPollServerConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[id] == conn {
		delete(s.sessions, id)
	}
}

func isHTTPPollRequest(r *http.Request) bool {
	return r.URL.Query().Get(httpPollQueryKey) == httpPollTransportName
}

func (s *ServerWS) serveHTTPPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(httpPollResponseHeader, httpPollTransportName)
	q := r.URL.Query()
	sessionID := q.Get(httpPollSessionKey)
	if sessionID == "" {
		http.Error(w, "missing http poll session", http.StatusBadRequest)
		return
	}
	action := q.Get(httpPollActionKey)
	log.WithFields(log.Fields{
		"action":    action,
		"method":    r.Method,
		"session":   sessionID,
		"query_len": len(r.URL.RawQuery),
		"has_msg":   q.Get(httpPollMessageKey) != "",
		"part":      q.Get(httpPollPartKey),
		"parts":     q.Get(httpPollPartsKey),
	}).Debug("http poll request")

	switch action {
	case httpPollActionOpen:
		conn, err := s.httpSessions.create(sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		go func() {
			defer s.httpSessions.remove(sessionID, conn)
			defer conn.Close(websocket.StatusNormalClosure, "done")
			s.serveConn(context.Background(), conn)
		}()
	case httpPollActionSend:
		conn := s.httpSessions.get(sessionID)
		if conn == nil {
			http.Error(w, "unknown http poll session", http.StatusGone)
			return
		}
		if err := conn.pushIncomingRequest(r.Context(), r); err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case httpPollActionRecv:
		conn := s.httpSessions.get(sessionID)
		if conn == nil {
			http.Error(w, "unknown http poll session", http.StatusGone)
			return
		}
		part, ok, err := conn.popOutgoingPart(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		if !ok {
			log.WithFields(log.Fields{
				"session": sessionID,
			}).Debug("http poll recv no message")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.WithFields(log.Fields{
			"session": sessionID,
			"bytes":   len(part.data),
			"part":    part.part,
			"parts":   part.parts,
		}).Debug("http poll recv message")
		w.Header().Set(httpPollMessageType, strconv.Itoa(int(part.typ)))
		w.Header().Set(httpPollHeaderMessage, part.messageID)
		w.Header().Set(httpPollHeaderPart, strconv.Itoa(part.part))
		w.Header().Set(httpPollHeaderParts, strconv.Itoa(part.parts))
		w.Header().Set(httpPollHeaderData, part.data)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	case httpPollActionClose:
		conn := s.httpSessions.get(sessionID)
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "client closed")
			s.httpSessions.remove(sessionID, conn)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unknown http poll action", http.StatusBadRequest)
	}
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return http.Header{}
	}
	out := make(http.Header, len(header))
	for key, values := range header {
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	return out
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
