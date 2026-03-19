package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"

	"github.com/readmedotmd/store.md/sync/client"
)

// SSETransport handles sync over Server-Sent Events.
//
// The transport uses two HTTP request types working in tandem:
//
//   - GET with Accept: text/event-stream opens a persistent event stream.
//     The server pushes sync messages to the client as SSE events with
//     event type "sync" and JSON-encoded [client.Message] data.
//
//   - POST with X-Transport: sse carries client-to-server sync messages.
//     These are correlated with an active SSE stream by peer ID. When no
//     SSE stream is active, POST requests fall back to stateless HTTP
//     behavior (identical to [HTTPTransport]).
//
// SSETransport implements [SessionTransport]: POST requests to an active
// session are treated as secondary requests that share the stream's
// connection slot, avoiding double-counting in the server's connection
// limits.
//
// # Wire format
//
// Each SSE event uses the "sync" event type with a single data line
// containing a JSON-encoded [client.Message]:
//
//	event: sync
//	data: {"type":"sync","payload":{...}}
//
// # Usage
//
// Register with a server using EnableSSE or mount on a specific
// route using Handler:
//
//	srv.EnableSSE()                            // auto-dispatch via CanHandle
//	mux.Handle("/sse", srv.Handler(srv.SSE())) // route-based dispatch
type SSETransport struct {
	sessions gosync.Map // peerID -> *sseSession
}

// sseSession tracks an active SSE event stream for a single peer.
// It is created when a GET request opens the stream and removed when
// the client disconnects or the session is closed.
type sseSession struct {
	peerID  string
	msgCh   chan client.Message // POST handler feeds messages here
	done    chan struct{}
	once    gosync.Once
	writer  http.ResponseWriter
	flusher http.Flusher
	writeMu gosync.Mutex // serializes writes to the event stream
}

// sseServerConn implements [client.Connection] so the SSE stream can be
// used with [PeerHandler.ServeConnection]. It bridges the SSE event stream
// (write) and the POST message channel (read) into the bidirectional
// Connection interface.
type sseServerConn struct {
	sess *sseSession
}

func (c *sseServerConn) PeerID() string { return c.sess.peerID }

// ReadMessage blocks until a message arrives from a POST request or the
// session closes.
func (c *sseServerConn) ReadMessage() (client.Message, error) {
	select {
	case msg := <-c.sess.msgCh:
		return msg, nil
	case <-c.sess.done:
		return client.Message{}, fmt.Errorf("SSE session closed")
	}
}

// WriteMessage serializes the message as an SSE "sync" event and flushes
// it to the client's event stream. Writes are serialized by writeMu to
// prevent interleaved SSE frames from concurrent goroutines.
func (c *sseServerConn) WriteMessage(msg client.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}

	c.sess.writeMu.Lock()
	defer c.sess.writeMu.Unlock()

	select {
	case <-c.sess.done:
		return fmt.Errorf("SSE session closed")
	default:
	}

	if _, err := fmt.Fprintf(c.sess.writer, "event: sync\ndata: %s\n\n", data); err != nil {
		return err
	}
	c.sess.flusher.Flush()
	return nil
}

func (c *sseServerConn) Close() error {
	c.sess.once.Do(func() { close(c.sess.done) })
	return nil
}

// CanHandle returns true for GET requests with Accept: text/event-stream
// (the SSE stream) or POST requests with X-Transport: sse (sync messages
// routed to an active SSE session).
//
// When the SSE transport is mounted on its own route via Handler,
// CanHandle is not called — all requests go to Serve directly.
func (t *SSETransport) CanHandle(r *http.Request) bool {
	if r.Method == http.MethodGet {
		return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	}
	return r.Method == http.MethodPost && r.Header.Get("X-Transport") == "sse"
}

// IsSessionRequest reports whether the request is a POST to an existing
// SSE session. Session requests piggyback on the stream's connection
// slot and should not acquire/release their own.
func (t *SSETransport) IsSessionRequest(r *http.Request, peerID string) bool {
	if r.Method != http.MethodPost {
		return false
	}
	_, ok := t.sessions.Load(peerID)
	return ok
}

// Serve handles both GET (event stream) and POST (sync message) requests.
func (t *SSETransport) Serve(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error {
	if r.Method == http.MethodPost {
		return t.handlePost(w, r, peerID, handler, logger)
	}
	return t.handleStream(w, r, peerID, handler, logger)
}

// handleStream opens a persistent SSE event stream for the peer.
//
// The handler sets Content-Type, Cache-Control, and Connection headers,
// then flushes them immediately so the client can start reading events.
// It blocks until the client disconnects (r.Context().Done()) or the
// session is explicitly closed.
func (t *SSETransport) handleStream(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return fmt.Errorf("ResponseWriter does not implement http.Flusher")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Flush headers immediately so the client's HTTP request completes
	// and it can start reading the event stream.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sess := &sseSession{
		peerID:  peerID,
		msgCh:   make(chan client.Message, 64),
		done:    make(chan struct{}),
		writer:  w,
		flusher: flusher,
	}

	t.sessions.Store(peerID, sess)
	defer t.sessions.Delete(peerID)

	conn := &sseServerConn{sess: sess}

	// Ensure session closes when client disconnects.
	go func() {
		<-r.Context().Done()
		conn.Close()
	}()

	if err := handler.ServeConnection(peerID, conn); err != nil {
		logger.Debug("SSE connection ended", "peer", peerID, "err", err)
	}
	return nil
}

// handlePost processes a sync message POSTed alongside an active SSE stream.
// The message is fed into the session's msgCh channel where the handler's
// ServeConnection picks it up for processing.
//
// If no SSE stream is active for the peer, the request falls back to
// stateless HTTP behavior via [HTTPTransport.Serve], returning a sync
// response in the HTTP response body.
//
// Returns 202 Accepted on success, 400 on malformed body, or 410 Gone
// if the session closed between lookup and send.
func (t *SSETransport) handlePost(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error {
	val, ok := t.sessions.Load(peerID)
	if !ok {
		// No active SSE stream — handle as stateless HTTP.
		return (&HTTPTransport{}).Serve(w, r, peerID, handler, logger)
	}
	sess := val.(*sseSession)

	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB limit
	var msg client.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return nil
	}

	select {
	case sess.msgCh <- msg:
	case <-sess.done:
		http.Error(w, "session closed", http.StatusGone)
		return nil
	}

	w.WriteHeader(http.StatusAccepted)
	return nil
}
