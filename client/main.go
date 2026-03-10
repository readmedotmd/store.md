package client

import (
	"fmt"
	"log"
	"net/http"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	storesync "github.com/readmedotmd/store.md/sync"
)

// Message is the wire format for sync protocol messages.
type Message struct {
	Type    string                  `json:"type"`
	Payload *storesync.SyncPayload `json:"payload,omitempty"`
	Limit   string                 `json:"limit,omitempty"`
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

// wsConn wraps a gorilla/websocket.Conn as a Connection.
type wsConn struct {
	ws     *websocket.Conn
	peerID string
}

func (c *wsConn) PeerID() string { return c.peerID }

func (c *wsConn) ReadMessage() (Message, error) {
	var msg Message
	err := c.ws.ReadJSON(&msg)
	return msg, err
}

func (c *wsConn) WriteMessage(msg Message) error {
	return c.ws.WriteJSON(msg)
}

func (c *wsConn) Close() error {
	return c.ws.Close()
}

// Dial creates a client-side Connection by dialing a WebSocket server.
func Dial(peerID, url string, header http.Header) (Connection, error) {
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	return &wsConn{ws: ws, peerID: peerID}, nil
}

// NewConn wraps an already-upgraded WebSocket as a server-side Connection.
func NewConn(ws *websocket.Conn, peerID string) Connection {
	ws.SetReadLimit(1 << 20)
	return &wsConn{ws: ws, peerID: peerID}
}

// managedConn wraps a Connection with lifecycle state.
type managedConn struct {
	Connection
	mu     gosync.Mutex // serializes writes
	done   chan struct{}
	active bool // true for client-side connections that initiate pulls
}

// Client is a sync adapter that connects a local SyncStore to one or more
// remote peers via Connections. It handles both client-side and server-side
// sync protocol roles:
//   - Receives sync_request -> responds with sync_response (server role)
//   - Receives sync_push -> SyncIn + responds with sync_ack (server role)
//   - Receives sync_response -> SyncIn (client role)
//   - Receives sync_ack -> no-op (client role)
//   - Receives sync_update -> sends sync_request (client role)
type Client struct {
	store    storesync.SyncStore
	interval time.Duration
	limit    int

	connsMu gosync.RWMutex
	conns   []*managedConn

	// inSyncIn tracks how many readLoop goroutines are currently processing
	// a sync_push (calling SyncIn). When > 0, the OnUpdate callback skips
	// pushing to avoid feedback loops.
	inSyncIn atomic.Int32

	done   chan struct{}
	unsub  func()
	closed bool
	mu     gosync.Mutex
}

// Option configures a Client.
type Option func(*Client)

// WithInterval sets the sync polling interval. Default is 5 seconds.
func WithInterval(d time.Duration) Option {
	return func(c *Client) { c.interval = d }
}

// WithLimit sets the max items per sync request. Default is 0 (no limit).
func WithLimit(limit int) Option {
	return func(c *Client) { c.limit = limit }
}

// New creates a Client that syncs the given store over connections.
func New(store storesync.SyncStore, opts ...Option) *Client {
	c := &Client{
		store:    store,
		interval: 5 * time.Second,
		done:     make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}

	// Subscribe to local updates and push to active (client-side) connections.
	// Skip when the update originated from a connection's SyncIn to avoid
	// sending data back to the peer that just pushed it.
	c.unsub = c.store.OnUpdate(func(item storesync.SyncStoreItem) {
		if c.inSyncIn.Load() > 0 {
			return
		}
		go c.pushActive()
	})

	return c
}

// Connect dials a remote WebSocket server and starts actively syncing.
// Can be called multiple times to connect to multiple servers.
// Active connections send an initial sync_request and run a periodic pull loop.
func (c *Client) Connect(peerID, url string, header http.Header) error {
	conn, err := Dial(peerID, url, header)
	if err != nil {
		return err
	}
	return c.addConn(conn, true)
}

// AddConnection adds an existing Connection (e.g. a server-side accepted
// WebSocket) and starts handling sync messages over it. The connection is
// passive: it responds to incoming sync_request/sync_push but does not
// initiate pulls. Use Connect for active client-side connections.
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

	mc := &managedConn{
		Connection: conn,
		done:       make(chan struct{}),
		active:     active,
	}

	c.connsMu.Lock()
	c.conns = append(c.conns, mc)
	c.connsMu.Unlock()

	go c.readLoop(mc)

	if active {
		go c.pullLoop(mc)

		if err := c.sendSyncRequest(mc); err != nil {
			log.Printf("sync client: initial pull error: %v", err)
		}
	}

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

// Close stops all sync loops and closes all connections.
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
	return firstErr
}

// sendSyncRequest asks the remote peer for its latest changes.
func (c *Client) sendSyncRequest(mc *managedConn) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	req := Message{Type: "sync_request"}
	if c.limit > 0 {
		req.Limit = fmt.Sprintf("%d", c.limit)
	}
	return mc.WriteMessage(req)
}

// sendPush pushes local changes to a single connection.
func (c *Client) sendPush(mc *managedConn) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	payload, err := c.store.SyncOut(mc.PeerID(), c.limit)
	if err != nil {
		return fmt.Errorf("SyncOut: %w", err)
	}
	if len(payload.Items) == 0 {
		return nil
	}

	return mc.WriteMessage(Message{Type: "sync_push", Payload: payload})
}

// pushActive pushes local changes to all active (client-side) connections.
func (c *Client) pushActive() {
	c.connsMu.RLock()
	conns := make([]*managedConn, 0, len(c.conns))
	for _, mc := range c.conns {
		if mc.active {
			conns = append(conns, mc)
		}
	}
	c.connsMu.RUnlock()

	for _, mc := range conns {
		if err := c.sendPush(mc); err != nil {
			log.Printf("sync client: push error: %v", err)
		}
	}
}

// broadcast sends a sync_update notification to all connections except the sender.
func (c *Client) broadcast(sender *managedConn) {
	c.connsMu.RLock()
	conns := make([]*managedConn, 0, len(c.conns))
	for _, mc := range c.conns {
		if mc != sender {
			conns = append(conns, mc)
		}
	}
	c.connsMu.RUnlock()

	msg := Message{Type: "sync_update"}
	for _, mc := range conns {
		mc.mu.Lock()
		err := mc.WriteMessage(msg)
		mc.mu.Unlock()
		if err != nil {
			log.Printf("sync client: broadcast error: %v", err)
		}
	}
}

// readLoop reads messages from a connection and handles both client and server roles.
func (c *Client) readLoop(mc *managedConn) {
	defer c.removeConn(mc)
	for {
		msg, err := mc.ReadMessage()
		if err != nil {
			select {
			case <-mc.done:
			case <-c.done:
			default:
				log.Printf("sync client: read error: %v", err)
			}
			return
		}

		switch msg.Type {
		// --- Server role: respond to remote peer's requests ---

		case "sync_request":
			limit := 0
			if msg.Limit != "" {
				fmt.Sscanf(msg.Limit, "%d", &limit)
			}
			payload, err := c.store.SyncOut(mc.PeerID(), limit)
			if err != nil {
				log.Printf("sync client: SyncOut error: %v", err)
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "internal error"})
				mc.mu.Unlock()
				continue
			}
			mc.mu.Lock()
			mc.WriteMessage(Message{Type: "sync_response", Payload: payload})
			mc.mu.Unlock()

		case "sync_push":
			if msg.Payload == nil {
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "missing payload"})
				mc.mu.Unlock()
				continue
			}
			c.inSyncIn.Add(1)
			err := c.store.SyncIn(mc.PeerID(), *msg.Payload)
			c.inSyncIn.Add(-1)
			if err != nil {
				log.Printf("sync client: SyncIn error: %v", err)
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "internal error"})
				mc.mu.Unlock()
				continue
			}
			mc.mu.Lock()
			mc.WriteMessage(Message{Type: "sync_ack"})
			mc.mu.Unlock()
			// Notify other connections that new data is available.
			c.broadcast(mc)

		// --- Client role: handle responses from remote server ---

		case "sync_response":
			if msg.Payload != nil {
				c.inSyncIn.Add(1)
				err := c.store.SyncIn(mc.PeerID(), *msg.Payload)
				c.inSyncIn.Add(-1)
				if err != nil {
					log.Printf("sync client: SyncIn error: %v", err)
				}
			}

		case "sync_ack":
			// Push acknowledged, nothing to do.

		case "sync_update":
			// Remote peer has new data — pull it.
			if err := c.sendSyncRequest(mc); err != nil {
				log.Printf("sync client: pull on update error: %v", err)
			}

		case "error":
			log.Printf("sync client: remote error: %s", msg.Error)
		}
	}
}

func (c *Client) pullLoop(mc *managedConn) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-mc.done:
			return
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.sendSyncRequest(mc); err != nil {
				log.Printf("sync client: pull error: %v", err)
			}
		}
	}
}
