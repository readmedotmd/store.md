package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

// Storage key prefixes for the inbox server.
const (
	inboxMsgPrefix    = "%inbox%msg%"
	inboxCursorPrefix = "%inbox%cur%"
)

// inboxEntry wraps a SyncStoreItem with the sender's peer ID so items
// are not echoed back to the peer that sent them.
type inboxEntry struct {
	Sender string                  `json:"sender"`
	Item   storesync.SyncStoreItem `json:"item"`
}

// InboxServer is a store-and-forward relay that receives sync messages,
// stores items opaquely (no decryption or conflict resolution), and
// forwards them to other peers when they connect.
//
// Unlike [Server], which uses a [core.SyncStore] and participates in
// full bidirectional sync with conflict resolution, InboxServer uses a
// plain [storemd.Store] as a message queue. The server never reads or
// modifies item payloads — it simply holds them until delivery.
//
// This is ideal for relay servers handling end-to-end encrypted data
// where the server cannot (and should not) decrypt the payloads.
//
// # Storage Model
//
// Items are stored in a global queue ordered by receipt time. Each peer
// has a read cursor tracking the last item delivered to it. Items sent
// by a peer are never echoed back to that peer.
//
// # Transports
//
// InboxServer uses the same pluggable transport system as [Server].
// WebSocket is enabled by default. Call [InboxServer.EnableHTTP] or
// [InboxServer.EnableSSE] to add more transports, or mount individual
// transports on routes using [InboxServer.Handler].
//
// # Example
//
//	store := memory.New()
//	tokens := map[string]string{"token-a": "alice", "token-b": "bob"}
//	inbox := server.NewInbox(store, server.TokenAuth(tokens))
//	http.ListenAndServe(":8080", inbox)
type InboxServer struct {
	store  storemd.Store
	auth   Authorizer
	logger *slog.Logger

	transports  []Transport
	wsTransport *WebSocketTransport

	maxConnsPerPeer int
	maxTotalConns   int

	connsMu    gosync.Mutex
	totalConns int
	peerConns  map[string]int

	// Active connections for push notifications, keyed by peer ID.
	activeMu gosync.RWMutex
	active   map[string][]*inboxPeer

	// Rate limiting for auth failures (per IP).
	authFailMu     gosync.Mutex
	authFailCount  map[string]*authFailEntry
	trustedProxies map[string]struct{}

	// Hooks
	onPeerConnect    []func(peerID string, r *http.Request) error
	onPeerDisconnect []func(peerID string)
}

// inboxPeer tracks an active persistent connection for push notifications.
type inboxPeer struct {
	peerID string
	conn   client.Connection
	notify chan struct{}
	done   chan struct{}
	once   gosync.Once
	mu     gosync.Mutex // serializes writes to conn
}

var _ http.Handler = (*InboxServer)(nil)

// NewInbox creates a store-and-forward inbox server backed by a plain
// key-value store. The store holds items temporarily until delivery.
// WebSocket transport is enabled by default.
func NewInbox(store storemd.Store, auth Authorizer) *InboxServer {
	wst := &WebSocketTransport{Upgrader: websocket.Upgrader{}}
	return &InboxServer{
		store:           store,
		auth:            auth,
		logger:          slog.Default(),
		transports:      []Transport{wst},
		wsTransport:     wst,
		maxConnsPerPeer: 10,
		maxTotalConns:   1000,
		peerConns:       make(map[string]int),
		active:          make(map[string][]*inboxPeer),
		authFailCount:   make(map[string]*authFailEntry),
	}
}

// SetLogger sets the structured logger for the inbox server.
func (s *InboxServer) SetLogger(l *slog.Logger) {
	s.logger = l
}

// SetMaxConnsPerPeer sets the maximum connections per peer.
func (s *InboxServer) SetMaxConnsPerPeer(n int) {
	s.maxConnsPerPeer = n
}

// SetMaxTotalConns sets the maximum total connections.
func (s *InboxServer) SetMaxTotalConns(n int) {
	s.maxTotalConns = n
}

// SetTrustedProxies configures IP addresses trusted to set X-Forwarded-For.
func (s *InboxServer) SetTrustedProxies(ips []string) {
	s.trustedProxies = make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		s.trustedProxies[ip] = struct{}{}
	}
}

// SetAllowedOrigins configures which origins are allowed for WebSocket.
func (s *InboxServer) SetAllowedOrigins(origins []string) {
	if s.wsTransport == nil {
		return
	}
	s.wsTransport.Upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		for _, o := range origins {
			if o == "*" || o == origin {
				return true
			}
		}
		return false
	}
}

// OnPeerConnect registers a hook invoked after authentication. Return a
// non-nil error to reject the connection with 403 Forbidden.
func (s *InboxServer) OnPeerConnect(fn func(peerID string, r *http.Request) error) {
	s.onPeerConnect = append(s.onPeerConnect, fn)
}

// OnPeerDisconnect registers a hook invoked when a peer disconnects.
func (s *InboxServer) OnPeerDisconnect(fn func(peerID string)) {
	s.onPeerDisconnect = append(s.onPeerDisconnect, fn)
}

// AddTransport registers a transport with the inbox server.
func (s *InboxServer) AddTransport(t Transport) {
	s.transports = append(s.transports, t)
}

// EnableHTTP adds an HTTP POST transport.
func (s *InboxServer) EnableHTTP() {
	s.AddTransport(&HTTPTransport{})
}

// EnableSSE adds a Server-Sent Events transport.
func (s *InboxServer) EnableSSE() {
	s.AddTransport(&SSETransport{})
}

// WebSocket returns the server's WebSocket transport instance.
func (s *InboxServer) WebSocket() *WebSocketTransport {
	return s.wsTransport
}

// HTTP returns a new HTTPTransport for use with Handler.
func (s *InboxServer) HTTP() *HTTPTransport {
	return &HTTPTransport{}
}

// SSE returns a new SSETransport for use with Handler.
func (s *InboxServer) SSE() *SSETransport {
	return &SSETransport{}
}

// Handler returns an http.Handler for a specific transport, allowing
// route-based dispatch:
//
//	mux.Handle("/ws", inbox.Handler(inbox.WebSocket()))
//	mux.Handle("/sse", inbox.Handler(inbox.SSE()))
//	mux.Handle("/http", inbox.Handler(inbox.HTTP()))
func (s *InboxServer) Handler(t Transport) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveWithTransport(t, w, r)
	})
}

// ServeHTTP implements http.Handler with auto-dispatch. It authenticates
// the request and routes to the first transport whose CanHandle returns true.
// For explicit routing, use [InboxServer.Handler] instead.
func (s *InboxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.checkAuthRateLimit(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	peerID, err := s.auth(r)
	if err != nil {
		s.recordAuthFailure(ip)
		if websocket.IsWebSocketUpgrade(r) && s.wsTransport != nil {
			ws, upgradeErr := s.wsTransport.Upgrader.Upgrade(w, r, nil)
			if upgradeErr != nil {
				return
			}
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseCodeAuthFailed, "Unauthorized"))
			ws.Close()
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var transport Transport
	for _, t := range s.transports {
		if t.CanHandle(r) {
			transport = t
			break
		}
	}
	if transport == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	s.serveTransport(transport, w, r, peerID)
}

// serveWithTransport handles auth + transport dispatch for route-based handlers.
func (s *InboxServer) serveWithTransport(t Transport, w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.checkAuthRateLimit(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	peerID, err := s.auth(r)
	if err != nil {
		s.recordAuthFailure(ip)
		if websocket.IsWebSocketUpgrade(r) && s.wsTransport != nil {
			ws, upgradeErr := s.wsTransport.Upgrader.Upgrade(w, r, nil)
			if upgradeErr != nil {
				return
			}
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseCodeAuthFailed, "Unauthorized"))
			ws.Close()
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	s.serveTransport(t, w, r, peerID)
}

// serveTransport runs connection tracking, hooks, and the transport.
func (s *InboxServer) serveTransport(transport Transport, w http.ResponseWriter, r *http.Request, peerID string) {
	// Check if this is a secondary request on an existing session.
	if st, ok := transport.(SessionTransport); ok && st.IsSessionRequest(r, peerID) {
		if err := transport.Serve(w, r, peerID, s, s.logger); err != nil {
			s.logger.Error("transport error", "err", err)
		}
		return
	}

	if err := s.acquireConn(peerID); err != nil {
		s.logger.Warn("connection limit reached", "peer", peerID, "err", err)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	for _, fn := range s.onPeerConnect {
		if err := fn(peerID, r); err != nil {
			s.releaseConn(peerID)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	if err := transport.Serve(w, r, peerID, s, s.logger); err != nil {
		s.logger.Error("transport error", "err", err)
	}
	s.releaseConn(peerID)

	for _, fn := range s.onPeerDisconnect {
		fn(peerID)
	}
}

// --- PeerHandler implementation ---

// HandleMessage processes a single sync exchange for stateless transports.
func (s *InboxServer) HandleMessage(ctx context.Context, peerID string, msg client.Message) (client.Message, func(), error) {
	// Store incoming items.
	if msg.Payload != nil && len(msg.Payload.Items) > 0 {
		s.storeItems(peerID, msg.Payload.Items)
		s.notifyPeers(peerID)
	}

	// Read pending items for this peer.
	items, cursor := s.readPending(peerID)
	resp := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: items},
	}

	ack := func() { s.advanceCursor(peerID, cursor) }
	return resp, ack, nil
}

// ServeConnection runs the sync protocol over a persistent connection,
// handling incoming messages and pushing new items as they arrive.
func (s *InboxServer) ServeConnection(peerID string, conn client.Connection) error {
	peer := &inboxPeer{
		peerID: peerID,
		conn:   conn,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}

	s.registerPeer(peer)
	defer s.unregisterPeer(peer)

	// Send any pending items immediately on connect.
	s.sendPendingToConn(peer)

	// Push goroutine: sends items when notified by other peers.
	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		for {
			select {
			case <-peer.notify:
				s.sendPendingToConn(peer)
			case <-peer.done:
				return
			}
		}
	}()

	// Read loop: handle incoming sync messages.
	defer func() {
		peer.once.Do(func() { close(peer.done) })
		<-pushDone
	}()

	ctx := context.Background()
	for {
		msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		if msg.Type != "sync" || msg.Payload == nil {
			continue
		}

		// Process via HandleMessage.
		resp, ack, _ := s.HandleMessage(ctx, peerID, msg)

		peer.mu.Lock()
		writeErr := conn.WriteMessage(resp)
		peer.mu.Unlock()

		if writeErr != nil {
			return writeErr
		}

		if ack != nil {
			ack()
		}
	}
}

// --- Inbox storage operations ---

// storeItems writes items to the global inbox queue.
func (s *InboxServer) storeItems(sender string, items []storesync.SyncStoreItem) {
	ctx := context.Background()
	for _, item := range items {
		entry := inboxEntry{Sender: sender, Item: item}
		data, err := json.Marshal(entry)
		if err != nil {
			s.logger.Error("marshal inbox entry", "err", err)
			continue
		}
		key := fmt.Sprintf("%s%019d%%%s", inboxMsgPrefix, time.Now().UnixNano(), uuid.New().String())
		if err := s.store.Set(ctx, key, string(data)); err != nil {
			s.logger.Error("store inbox item", "err", err)
		}
	}
}

// readPending returns items after the peer's cursor, excluding items the
// peer sent. Returns the items and the last store key (for cursor advance).
func (s *InboxServer) readPending(peerID string) ([]storesync.SyncStoreItem, string) {
	ctx := context.Background()

	cursor, err := s.store.Get(ctx, inboxCursorPrefix+peerID)
	if err != nil {
		cursor = "" // start from beginning
	}

	list, err := s.store.List(ctx, storemd.ListArgs{
		Prefix:     inboxMsgPrefix,
		StartAfter: cursor,
		Limit:      1000,
	})
	if err != nil {
		s.logger.Error("list inbox items", "peer", peerID, "err", err)
		return nil, ""
	}

	if len(list) == 0 {
		return nil, ""
	}

	var items []storesync.SyncStoreItem
	var lastKey string
	for _, kv := range list {
		lastKey = kv.Key
		var entry inboxEntry
		if err := json.Unmarshal([]byte(kv.Value), &entry); err != nil {
			s.logger.Error("unmarshal inbox entry", "err", err)
			continue
		}
		if entry.Sender == peerID {
			continue // don't echo back
		}
		items = append(items, entry.Item)
	}

	return items, lastKey
}

// sendPendingToConn reads and sends pending items to a connected peer.
func (s *InboxServer) sendPendingToConn(peer *inboxPeer) {
	items, cursor := s.readPending(peer.peerID)
	if len(items) == 0 {
		// Ensure the peer has a cursor entry even when no items are pending.
		// This lets Cleanup know the peer exists and hasn't read past "".
		s.advanceCursor(peer.peerID, cursor)
		return
	}

	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: items},
	}

	peer.mu.Lock()
	err := peer.conn.WriteMessage(msg)
	peer.mu.Unlock()

	if err != nil {
		s.logger.Error("push to peer", "peer", peer.peerID, "err", err)
		return
	}

	s.advanceCursor(peer.peerID, cursor)
}

// advanceCursor updates the read cursor for a peer. The cursor only moves
// forward — if the peer already has a cursor beyond the given value, this
// is a no-op. Setting cursor to "" on a peer with no existing cursor
// establishes the peer's presence for [InboxServer.Cleanup].
func (s *InboxServer) advanceCursor(peerID, cursor string) {
	ctx := context.Background()
	existing, err := s.store.Get(ctx, inboxCursorPrefix+peerID)
	if err == nil && existing >= cursor {
		return // don't regress
	}
	if err := s.store.Set(ctx, inboxCursorPrefix+peerID, cursor); err != nil {
		s.logger.Error("advance cursor", "peer", peerID, "err", err)
	}
}

// --- Peer tracking for push notifications ---

// notifyPeers signals all active connections except the sender.
func (s *InboxServer) notifyPeers(senderID string) {
	s.activeMu.RLock()
	defer s.activeMu.RUnlock()

	for peerID, peers := range s.active {
		if peerID == senderID {
			continue
		}
		for _, p := range peers {
			select {
			case p.notify <- struct{}{}:
			default: // already notified
			}
		}
	}
}

func (s *InboxServer) registerPeer(peer *inboxPeer) {
	s.activeMu.Lock()
	s.active[peer.peerID] = append(s.active[peer.peerID], peer)
	s.activeMu.Unlock()
}

func (s *InboxServer) unregisterPeer(peer *inboxPeer) {
	s.activeMu.Lock()
	peers := s.active[peer.peerID]
	for i, p := range peers {
		if p == peer {
			s.active[peer.peerID] = append(peers[:i], peers[i+1:]...)
			break
		}
	}
	if len(s.active[peer.peerID]) == 0 {
		delete(s.active, peer.peerID)
	}
	s.activeMu.Unlock()
}

// --- Cleanup ---

// Cleanup removes delivered items from the store. An item is considered
// delivered when all known peers (those with cursors) have read past it.
// Call this periodically to reclaim storage.
func (s *InboxServer) Cleanup(ctx context.Context) error {
	cursors, err := s.store.List(ctx, storemd.ListArgs{
		Prefix: inboxCursorPrefix,
	})
	if err != nil {
		return fmt.Errorf("list cursors: %w", err)
	}
	if len(cursors) == 0 {
		return nil
	}

	minCursor := ""
	for _, kv := range cursors {
		if minCursor == "" || kv.Value < minCursor {
			minCursor = kv.Value
		}
	}
	if minCursor == "" {
		return nil
	}

	for {
		list, err := s.store.List(ctx, storemd.ListArgs{
			Prefix: inboxMsgPrefix,
			Limit:  100,
		})
		if err != nil {
			return fmt.Errorf("list items: %w", err)
		}
		if len(list) == 0 {
			break
		}

		deleted := 0
		for _, kv := range list {
			if kv.Key > minCursor {
				return nil
			}
			if err := s.store.Delete(ctx, kv.Key); err != nil {
				s.logger.Error("delete inbox item", "key", kv.Key, "err", err)
			}
			deleted++
		}
		if deleted == 0 {
			break
		}
	}

	return nil
}

// --- Connection tracking (mirrors Server methods) ---

func (s *InboxServer) acquireConn(peerID string) error {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.totalConns >= s.maxTotalConns {
		return fmt.Errorf("maximum total connections reached (%d)", s.maxTotalConns)
	}
	if s.peerConns[peerID] >= s.maxConnsPerPeer {
		return fmt.Errorf("maximum connections per peer reached (%d)", s.maxConnsPerPeer)
	}
	s.totalConns++
	s.peerConns[peerID]++
	return nil
}

func (s *InboxServer) releaseConn(peerID string) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.totalConns > 0 {
		s.totalConns--
	}
	s.peerConns[peerID]--
	if s.peerConns[peerID] <= 0 {
		delete(s.peerConns, peerID)
	}
}

func (s *InboxServer) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx >= 0 {
		ip = ip[:idx]
	}
	if len(s.trustedProxies) > 0 {
		if _, trusted := s.trustedProxies[ip]; trusted {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				if clientIP, _, ok := strings.Cut(xff, ","); ok {
					return strings.TrimSpace(clientIP)
				}
				return strings.TrimSpace(xff)
			}
		}
	}
	return ip
}

func (s *InboxServer) checkAuthRateLimit(ip string) bool {
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	entry, ok := s.authFailCount[ip]
	if !ok {
		return false
	}
	if time.Now().After(entry.resetAt) {
		delete(s.authFailCount, ip)
		return false
	}
	return entry.count >= 10
}

func (s *InboxServer) recordAuthFailure(ip string) {
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	if len(s.authFailCount) >= 10000 {
		return
	}
	entry, ok := s.authFailCount[ip]
	if !ok || time.Now().After(entry.resetAt) {
		s.authFailCount[ip] = &authFailEntry{
			count:   1,
			resetAt: time.Now().Add(1 * time.Minute),
		}
		return
	}
	entry.count++
}
