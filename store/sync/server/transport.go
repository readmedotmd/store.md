package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/sync/client"
)

// PeerHandler processes sync protocol interactions with peers.
// Both [Server] (full sync) and [InboxServer] (store-and-forward) implement
// this interface through handler types, allowing the same set of transports
// to work with either server architecture.
//
// For example, a WebSocket transport calls ServeConnection to run the sync
// protocol over a persistent connection, while an HTTP transport calls
// HandleMessage for a single request-response exchange. The handler
// implementation determines the sync semantics (full sync vs relay).
type PeerHandler interface {
	// HandleMessage processes a single sync exchange (request-response).
	// It stores incoming items, prepares outgoing items, and returns the
	// response message. The ack function, if non-nil, must be called after
	// the response has been successfully delivered to advance the sync
	// cursor. Skipping ack causes items to be re-sent on the next exchange.
	//
	// Used by stateless transports like [HTTPTransport].
	HandleMessage(ctx context.Context, peerID string, msg client.Message) (resp client.Message, ack func(), err error)

	// ServeConnection runs the sync protocol over a persistent, bidirectional
	// connection. It reads incoming messages, processes them, and pushes new
	// items as they arrive from other peers. Blocks until the connection
	// closes or an error occurs.
	//
	// Used by stream transports like [WebSocketTransport] and [SSETransport].
	ServeConnection(peerID string, conn client.Connection) error
}

// Transport handles the sync protocol over a specific network transport.
//
// Implementations are registered with [Server.AddTransport] or mounted on
// individual routes via [Server.Handler]. The server calls CanHandle to
// select a transport during auto-dispatch (ServeHTTP), or skips it entirely
// for route-based dispatch.
//
// Transports are decoupled from sync semantics via [PeerHandler], so the
// same transport works with both [Server] and [InboxServer].
//
// Built-in transports:
//   - [WebSocketTransport]: persistent bidirectional (default)
//   - [HTTPTransport]: stateless request-response
//   - [SSETransport]: server-push events with POST for client-to-server
type Transport interface {
	// CanHandle reports whether this transport can handle the given request.
	// Used by ServeHTTP for auto-dispatch. Not called when the
	// transport is mounted via Handler.
	CanHandle(r *http.Request) bool

	// Serve handles the sync exchange for an authenticated peer.
	// For stream transports (WebSocket, SSE), this blocks until the
	// connection closes. For request-response transports (HTTP), this
	// handles a single exchange and returns immediately.
	Serve(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error
}

// SessionTransport is an optional interface for transports that multiplex
// multiple HTTP requests over a single logical connection.
//
// For example, [SSETransport] uses a long-lived GET request for the event
// stream (which holds the connection slot) and short-lived POST requests
// to feed client messages into the same session. Without SessionTransport,
// each POST would acquire its own connection slot, counting against
// per-peer and total limits and triggering connect/disconnect hooks.
//
// When IsSessionRequest returns true for a request, the server skips:
//   - Connection slot acquisition/release
//   - OnPeerConnect hooks
//   - OnPeerDisconnect hooks
type SessionTransport interface {
	Transport
	// IsSessionRequest reports whether the request belongs to an
	// existing session for the given peer.
	IsSessionRequest(r *http.Request, peerID string) bool
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

// Serve upgrades the connection to WebSocket and delegates to the
// handler's ServeConnection for sync protocol management.
func (t *WebSocketTransport) Serve(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error {
	ws, err := t.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("websocket upgrade: %w", err)
	}

	conn := client.NewConn(ws, peerID)
	if err := handler.ServeConnection(peerID, conn); err != nil {
		logger.Debug("connection ended", "peer", peerID, "err", err)
	}
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

// Serve reads a sync message from the request body, delegates to the
// handler's HandleMessage, and writes the response.
func (t *HTTPTransport) Serve(w http.ResponseWriter, r *http.Request, peerID string, handler PeerHandler, logger *slog.Logger) error {
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

	resp, ack, err := handler.HandleMessage(ctx, peerID, msg)
	if err != nil {
		logger.Error("sync error", "peer", peerID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return err
	}

	// Advance cursor only after successful response write.
	if ack != nil {
		ack()
	}
	return nil
}
