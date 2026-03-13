package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

// Transport handles the sync protocol over a specific network transport.
type Transport interface {
	// CanHandle reports whether this transport can handle the given request.
	CanHandle(r *http.Request) bool

	// Serve handles the sync exchange for an authenticated peer.
	// For stream transports (WebSocket), this blocks until the connection closes.
	// For request-response transports (HTTP), this handles a single exchange.
	Serve(w http.ResponseWriter, r *http.Request, peerID string, store storesync.SyncStore, adapter *client.Client, logger *slog.Logger) error
}

// WebSocketTransport upgrades HTTP connections to WebSocket for persistent,
// bidirectional sync communication.
type WebSocketTransport struct {
	Upgrader websocket.Upgrader
}

// CanHandle returns true for WebSocket upgrade requests.
func (t *WebSocketTransport) CanHandle(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

// Serve upgrades the connection to WebSocket, adds it to the adapter, and
// blocks until the connection closes.
func (t *WebSocketTransport) Serve(w http.ResponseWriter, r *http.Request, peerID string, store storesync.SyncStore, adapter *client.Client, logger *slog.Logger) error {
	ws, err := t.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("websocket upgrade: %w", err)
	}

	conn := &notifyConn{
		Connection: client.NewConn(ws, peerID),
		done:       make(chan struct{}),
	}

	if err := adapter.AddConnection(conn); err != nil {
		logger.Error("AddConnection error", "err", err)
		ws.Close()
		return err
	}

	<-conn.done
	return nil
}

// HTTPTransport handles sync over standard HTTP POST requests.
// Each request carries a single sync message and receives a single response.
// This is suitable for environments where WebSocket connections are not available.
type HTTPTransport struct{}

// CanHandle returns true for POST requests.
func (t *HTTPTransport) CanHandle(r *http.Request) bool {
	return r.Method == http.MethodPost
}

// Serve reads a sync message from the request body, processes it against
// the store, and writes the response. It does not use the adapter since
// HTTP requests are stateless — each request is an independent sync exchange.
func (t *HTTPTransport) Serve(w http.ResponseWriter, r *http.Request, peerID string, store storesync.SyncStore, _ *client.Client, logger *slog.Logger) error {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB limit
	var msg client.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return nil
	}

	if msg.Type != "sync" {
		http.Error(w, "unsupported message type", http.StatusBadRequest)
		return nil
	}

	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	payload := msg.Payload
	if payload == nil {
		payload = &storesync.SyncPayload{}
	}

	response, err := store.Sync(ctx, peerID, payload)
	if err != nil {
		logger.Error("sync error", "peer", peerID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil
	}

	if response == nil {
		response = &storesync.SyncPayload{}
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(client.Message{Type: "sync", Payload: response})
}
