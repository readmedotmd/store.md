package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	gosync "sync"
	"time"

	storesync "github.com/readmedotmd/store.md/sync/core"
)

// ErrAuthFailed is returned when a WebSocket connection is closed with a
// close code indicating authentication failure (4401 or 1008). OnConnectError
// hooks can check for this error with errors.Is to distinguish auth failures
// from transient network errors and stop retrying.
var ErrAuthFailed = errors.New("authentication failed")

// Message is the wire format for sync protocol messages.
type Message struct {
	Type    string                 `json:"type"`
	Payload *storesync.SyncPayload `json:"payload,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// Connection is a bidirectional sync protocol transport.
// Both client-side and server-side WebSocket connections implement this.
type Connection interface {
	PeerID() string
	ReadMessage() (Message, error)
	WriteMessage(msg Message) error
	Close() error
}

// managedConn wraps a Connection with lifecycle state.
type managedConn struct {
	Connection
	mu     gosync.Mutex // serializes writes
	done   chan struct{}
	active bool // true for client-side connections that initiate sync

	// dialInfo stores the parameters needed to redial this connection.
	// Non-nil only for active connections created via Connect.
	dialInfo *dialInfo
}

// dialInfo stores the parameters needed to redial a connection.
type dialInfo struct {
	peerID string
	url    string
	header http.Header
}

// Client is a sync adapter that connects a local SyncStore to one or more
// remote peers via Connections. It uses the store's Sync method to drive
// the sync protocol.
//
// When local data changes, the client pushes items directly to all connected
// peers. There is no polling — sync is entirely event-driven.
//
// Protocol messages:
//   - sync: carries a SyncPayload with items the peer hasn't seen
//
// # Hooks
//
// The client supports interceptor hooks that can observe, cancel, or modify
// behavior at key lifecycle points. All hooks are registered via Option
// functions at construction time.
//
//   - [OnConnect]: Called after a connection is established. Return an error
//     to reject and close the connection immediately.
//   - [OnDisconnect]: Called when a connection drops unexpectedly. Return
//     false to suppress automatic reconnection for this disconnect.
//   - [OnConnectError]: Called when a dial/reconnection attempt fails.
//     Return false to stop retrying and give up on reconnection.
//   - [WithHeaderProvider]: Called before every dial (including reconnects)
//     to provide fresh HTTP headers (e.g. refreshed auth tokens).
//
// Multiple hooks of the same type are called in registration order. For
// boolean hooks, any hook returning false will suppress the behavior (even
// if other hooks return true). For error hooks, the first error short-circuits.
type Client struct {
	store   storesync.SyncStore
	logger  *slog.Logger
	dialer  Dialer
	dialers map[string]Dialer // scheme -> dialer

	maxConns int

	connsMu gosync.RWMutex
	conns   []*managedConn

	// syncInIDs tracks item IDs currently being processed via Sync.
	// The OnUpdate callback skips items with these IDs to avoid pushing
	// data back to the peer that just sent it.
	syncInIDs gosync.Map

	// notifyCh coalesces rapid OnUpdate notifications into a single
	// push cycle, avoiding 2 goroutines per update.
	notifyCh chan struct{}

	// Reconnection settings.
	reconnect        bool
	reconnectInitial time.Duration
	reconnectMax     time.Duration
	pendingReconnMu  gosync.Mutex
	pendingReconn    int

	// Hooks
	headerProvider func(peerID, url string) (http.Header, error)
	onConnect      []func(peerID string) error
	onDisconnect   []func(peerID string, err error) bool
	onConnectError []func(peerID string, err error) bool

	wg     gosync.WaitGroup // tracks all background goroutines
	done   chan struct{}
	unsub  func()
	closed bool
	mu     gosync.Mutex
}

// Option configures a Client.
type Option func(*Client)

// WithLogger sets the structured logger for the client.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithMaxConns sets the maximum number of connections. Default is 100.
func WithMaxConns(n int) Option {
	return func(c *Client) { c.maxConns = n }
}

// WithReconnect enables automatic reconnection for client-initiated connections
// (those created via Connect). When a connection drops unexpectedly, the client
// will attempt to redial with exponential backoff starting at initialBackoff and
// capping at maxBackoff. Reconnection continues until the client is closed.
// Defaults: initialBackoff=1s, maxBackoff=30s if zero values are passed.
func WithReconnect(initialBackoff, maxBackoff time.Duration) Option {
	return func(c *Client) {
		c.reconnect = true
		if initialBackoff > 0 {
			c.reconnectInitial = initialBackoff
		}
		if maxBackoff > 0 {
			c.reconnectMax = maxBackoff
		}
	}
}

// WithHeaderProvider sets a function that returns HTTP headers for each dial
// attempt, including reconnections. This enables token refresh — the provider
// is called before every dial so it can return fresh credentials. When set,
// the header parameter passed to Connect is ignored.
func WithHeaderProvider(fn func(peerID, url string) (http.Header, error)) Option {
	return func(c *Client) { c.headerProvider = fn }
}

// OnConnect registers an interceptor invoked after a connection is established,
// including after successful reconnection. Return a non-nil error to reject
// the connection — it will be closed immediately.
func OnConnect(fn func(peerID string) error) Option {
	return func(c *Client) { c.onConnect = append(c.onConnect, fn) }
}

// OnDisconnect registers an interceptor invoked when a connection drops
// unexpectedly (not via explicit Close). The error is the read error that
// caused the disconnect. Return false to suppress automatic reconnection
// for this disconnect event.
func OnDisconnect(fn func(peerID string, err error) bool) Option {
	return func(c *Client) { c.onDisconnect = append(c.onDisconnect, fn) }
}

// OnConnectError registers an interceptor invoked when a dial attempt fails,
// including reconnection attempts. Return false to stop retrying and give up
// on reconnection.
func OnConnectError(fn func(peerID string, err error) bool) Option {
	return func(c *Client) { c.onConnectError = append(c.onConnectError, fn) }
}

// New creates a Client that syncs the given store over connections.
func New(store storesync.SyncStore, opts ...Option) *Client {
	c := &Client{
		store:            store,
		logger:           slog.Default(),
		dialer:           Dial,
		maxConns:         100,
		reconnectInitial: 1 * time.Second,
		reconnectMax:     30 * time.Second,
		done:             make(chan struct{}),
		notifyCh:         make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(c)
	}

	// Start the coalescing notification goroutine.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.notifyLoop()
	}()

	// Subscribe to local updates. On update, send a non-blocking signal
	// to the notify channel which coalesces rapid notifications.
	c.unsub = c.store.OnUpdate(func(item storesync.SyncStoreItem) {
		if _, ok := c.syncInIDs.Load(item.ID); ok {
			return
		}
		// Non-blocking send — if there's already a pending notification, skip.
		select {
		case c.notifyCh <- struct{}{}:
		default:
		}
	})

	return c
}

// notifyLoop reads from the notify channel and coalesces rapid notifications
// into a single push cycle that sends items to all connected peers.
func (c *Client) notifyLoop() {
	for {
		select {
		case <-c.done:
			return
		case <-c.notifyCh:
			// Drain any additional queued notifications to coalesce.
			draining := true
			for draining {
				select {
				case <-c.notifyCh:
				default:
					draining = false
				}
			}
			c.pushToAll(nil)
		}
	}
}

// Connect dials a remote server using the configured Dialer and starts
// actively syncing. The default dialer uses WebSocket; use WithDialer to
// switch to HTTP or a custom transport. Can be called multiple times to
// connect to multiple servers.
func (c *Client) Connect(peerID, url string, header http.Header) error {
	if c.headerProvider != nil {
		h, err := c.headerProvider(peerID, url)
		if err != nil {
			return fmt.Errorf("header provider: %w", err)
		}
		header = h
	}
	dialer, dialURL := c.resolveDialer(url)
	conn, err := dialer(peerID, dialURL, header)
	if err != nil {
		return err
	}
	var di *dialInfo
	if c.reconnect {
		h := make(http.Header)
		for k, v := range header {
			h[k] = v
		}
		di = &dialInfo{peerID: peerID, url: url, header: h}
	}
	return c.addConn(conn, true, di)
}

// AddConnection adds an existing Connection (e.g. a server-side accepted
// WebSocket) and starts handling sync messages over it. The connection is
// passive: it responds to incoming sync messages but does not initiate.
func (c *Client) AddConnection(conn Connection) error {
	return c.addConn(conn, false, nil)
}

func (c *Client) addConn(conn Connection, active bool, di *dialInfo) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client is closed")
	}
	c.mu.Unlock()

	c.connsMu.Lock()
	if len(c.conns) >= c.maxConns {
		c.connsMu.Unlock()
		return fmt.Errorf("maximum connections reached (%d)", c.maxConns)
	}
	c.connsMu.Unlock()

	mc := &managedConn{
		Connection: conn,
		done:       make(chan struct{}),
		active:     active,
		dialInfo:   di,
	}

	c.connsMu.Lock()
	c.conns = append(c.conns, mc)
	c.connsMu.Unlock()

	for _, fn := range c.onConnect {
		if err := fn(conn.PeerID()); err != nil {
			// Hook rejected the connection — remove and close it.
			c.removeConn(mc)
			conn.Close()
			return fmt.Errorf("onConnect rejected: %w", err)
		}
	}

	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.readLoop(mc) }()

	// Initiate first sync immediately to exchange any existing data.
	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.initiateSync(mc) }()

	return nil
}

// Done returns a channel that is closed when the client is closed.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// removeConn removes a dead connection from the list and auto-closes the
// client when no connections remain.
func (c *Client) removeConn(mc *managedConn) {
	c.connsMu.Lock()
	for i, conn := range c.conns {
		if conn == mc {
			c.conns = append(c.conns[:i], c.conns[i+1:]...)
			break
		}
	}
	remaining := len(c.conns)
	c.connsMu.Unlock()

	c.pendingReconnMu.Lock()
	pending := c.pendingReconn
	c.pendingReconnMu.Unlock()

	if remaining == 0 && pending == 0 {
		c.Close()
	}
}

// reconnectLoop attempts to re-establish a dropped connection with
// exponential backoff. It retries until the client is closed.
func (c *Client) reconnectLoop(di *dialInfo) {
	defer func() {
		c.pendingReconnMu.Lock()
		c.pendingReconn--
		pending := c.pendingReconn
		c.pendingReconnMu.Unlock()

		// If we gave up (client closing) and nothing else is left, close.
		c.connsMu.RLock()
		remaining := len(c.conns)
		c.connsMu.RUnlock()
		if remaining == 0 && pending == 0 {
			c.Close()
		}
	}()

	backoff := c.reconnectInitial
	attempt := 0
	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.logger.Info("reconnecting", "peer", di.peerID, "attempt", attempt+1, "backoff", backoff)

		header := di.header
		if c.headerProvider != nil {
			h, err := c.headerProvider(di.peerID, di.url)
			if err != nil {
				c.logger.Error("header provider failed during reconnect", "peer", di.peerID, "err", err)
				keepRetrying := true
				for _, fn := range c.onConnectError {
					if !fn(di.peerID, err) {
						keepRetrying = false
					}
				}
				if !keepRetrying {
					return
				}
			} else {
				header = h
			}
		}

		dialer, dialURL := c.resolveDialer(di.url)
		conn, err := dialer(di.peerID, dialURL, header)
		if err != nil {
			c.logger.Error("reconnect dial failed", "peer", di.peerID, "err", err)
			keepRetrying := true
			for _, fn := range c.onConnectError {
				if !fn(di.peerID, err) {
					keepRetrying = false
				}
			}
			if !keepRetrying {
				return
			}
			attempt++

			select {
			case <-c.done:
				return
			case <-time.After(backoff):
			}

			// Exponential backoff with cap.
			backoff = time.Duration(float64(c.reconnectInitial) * math.Pow(2, float64(attempt)))
			if backoff > c.reconnectMax {
				backoff = c.reconnectMax
			}
			continue
		}

		c.logger.Info("reconnected", "peer", di.peerID, "attempts", attempt+1)
		if err := c.addConn(conn, true, di); err != nil {
			c.logger.Error("reconnect addConn failed", "peer", di.peerID, "err", err)
			conn.Close()
			return
		}
		return
	}
}

// Closed returns true if the client has been closed.
func (c *Client) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close stops all goroutines and closes all connections.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	close(c.done)

	if c.unsub != nil {
		c.unsub()
	}

	c.connsMu.RLock()
	conns := make([]*managedConn, len(c.conns))
	copy(conns, c.conns)
	c.connsMu.RUnlock()

	var firstErr error
	for _, mc := range conns {
		close(mc.done)
		if err := mc.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	c.wg.Wait()
	return firstErr
}

// initiateSync starts a sync exchange on a connection by calling Sync(nil).
// Only sends a message when there are items to sync. Sending empty payloads
// would trigger unnecessary responses from the remote side, contributing to
// sync loops.
func (c *Client) initiateSync(mc *managedConn) {
	ctx := context.Background()
	payload, err := c.store.Sync(ctx, mc.PeerID(), nil)
	if err != nil {
		c.logger.Error("initiate sync failed", "err", err)
		return
	}
	if payload == nil {
		return // nothing to send
	}

	mc.mu.Lock()
	err = mc.WriteMessage(Message{Type: "sync", Payload: payload})
	mc.mu.Unlock()
	if err != nil {
		c.logger.Error("send error during initiate sync", "err", err)
		return
	}

	// Advance cursor only after successful write to peer.
	if err := c.store.AckSyncOut(ctx, mc.PeerID(), payload); err != nil {
		c.logger.Error("ack sync out error", "err", err)
	}
}

// pushToAll pushes items to all connections except the sender by calling
// initiateSync on each. This sends the actual items each peer hasn't seen,
// rather than just a notification.
func (c *Client) pushToAll(sender *managedConn) {
	c.connsMu.RLock()
	conns := make([]*managedConn, 0, len(c.conns))
	for _, mc := range c.conns {
		if mc != sender {
			conns = append(conns, mc)
		}
	}
	c.connsMu.RUnlock()

	for _, mc := range conns {
		c.initiateSync(mc)
	}
}

// readLoop reads messages from a connection and handles them.
func (c *Client) readLoop(mc *managedConn) {
	var readErr error
	defer func() {
		unexpectedDrop := false
		select {
		case <-c.done:
		case <-mc.done:
		default:
			unexpectedDrop = true
		}

		allowReconnect := true
		if unexpectedDrop {
			for _, fn := range c.onDisconnect {
				if !fn(mc.PeerID(), readErr) {
					allowReconnect = false
				}
			}
		}

		shouldReconnect := unexpectedDrop && mc.dialInfo != nil && allowReconnect
		if shouldReconnect {
			// Increment before removeConn so auto-close doesn't fire.
			c.pendingReconnMu.Lock()
			c.pendingReconn++
			c.pendingReconnMu.Unlock()
		}
		c.removeConn(mc)
		if shouldReconnect {
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.reconnectLoop(mc.dialInfo)
			}()
		}
	}()
	ctx := context.Background()
	for {
		msg, err := mc.ReadMessage()
		if err != nil {
			readErr = err
			select {
			case <-mc.done:
			case <-c.done:
			default:
				c.logger.Error("read error", "err", err)
			}
			return
		}

		switch msg.Type {
		case "sync":
			if msg.Payload == nil {
				continue
			}

			// Track incoming item IDs so the OnUpdate callback can suppress
			// re-broadcasting items back to the peer that sent them. This
			// prevents echo loops: without this, receiving items via Sync
			// would trigger OnUpdate, which would try to push those same
			// items back to the sender.
			for i := range msg.Payload.Items {
				c.syncInIDs.Store(msg.Payload.Items[i].ID, struct{}{})
			}

			response, err := c.store.Sync(ctx, mc.PeerID(), msg.Payload)

			// Clean up tracked IDs after Sync completes, regardless of
			// success or failure.
			for i := range msg.Payload.Items {
				c.syncInIDs.Delete(msg.Payload.Items[i].ID)
			}

			if err != nil {
				c.logger.Error("sync processing error", "err", err)
				// Never send internal error details to peers.
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "internal error"})
				mc.mu.Unlock()
				continue
			}

			// Push items to other connections that new data arrived.
			if len(msg.Payload.Items) > 0 {
				c.wg.Add(1)
				go func() { defer c.wg.Done(); c.pushToAll(mc) }()
			}

			// Send response if there's data to exchange.
			if response != nil {
				mc.mu.Lock()
				err = mc.WriteMessage(Message{Type: "sync", Payload: response})
				mc.mu.Unlock()
				if err != nil {
					c.logger.Error("send error", "err", err)
				} else {
					// Advance cursor only after successful write to peer.
					if ackErr := c.store.AckSyncOut(ctx, mc.PeerID(), response); ackErr != nil {
						c.logger.Error("ack sync out error", "err", ackErr)
					}
				}
			}

		case "sync_update":
			// Backward compatibility: older peers may still send sync_update
			// notifications. Respond by initiating a sync to push our items.
			c.wg.Add(1)
			go func() { defer c.wg.Done(); c.initiateSync(mc) }()

		case "error":
			c.logger.Error("remote error", "msg", msg.Error)
		}
	}
}
