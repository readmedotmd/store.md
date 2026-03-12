package client

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	gosync "sync"

	storesync "github.com/readmedotmd/store.md/sync/core"
)

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
type Client struct {
	store  storesync.SyncStore
	logger *slog.Logger
	dialer Dialer

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

// New creates a Client that syncs the given store over connections.
func New(store storesync.SyncStore, opts ...Option) *Client {
	c := &Client{
		store:    store,
		logger:   slog.Default(),
		dialer:   Dial,
		maxConns: 100,
		done:     make(chan struct{}),
		notifyCh: make(chan struct{}, 1),
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
	conn, err := c.dialer(peerID, url, header)
	if err != nil {
		return err
	}
	return c.addConn(conn, true)
}

// AddConnection adds an existing Connection (e.g. a server-side accepted
// WebSocket) and starts handling sync messages over it. The connection is
// passive: it responds to incoming sync messages but does not initiate.
func (c *Client) AddConnection(conn Connection) error {
	return c.addConn(conn, false)
}

func (c *Client) addConn(conn Connection, active bool) error {
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
	}

	c.connsMu.Lock()
	c.conns = append(c.conns, mc)
	c.connsMu.Unlock()

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

	if remaining == 0 {
		c.Close()
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
// Always sends a message, even if Sync returns nil, so the remote side gets
// a chance to respond with its own data.
func (c *Client) initiateSync(mc *managedConn) {
	ctx := context.Background()
	payload, err := c.store.Sync(ctx, mc.PeerID(), nil)
	if err != nil {
		c.logger.Error("initiate sync failed", "err", err)
		return
	}
	if payload == nil {
		payload = &storesync.SyncPayload{}
	}

	mc.mu.Lock()
	err = mc.WriteMessage(Message{Type: "sync", Payload: payload})
	mc.mu.Unlock()
	if err != nil {
		c.logger.Error("send error during initiate sync", "err", err)
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
	defer c.removeConn(mc)
	ctx := context.Background()
	for {
		msg, err := mc.ReadMessage()
		if err != nil {
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
