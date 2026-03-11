package fingerprintsync

import (
	"fmt"
	"log"
	"net/http"
	gosync "sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/sync"
)

// Message is the wire format for the fingerprint sync protocol.
// It is a superset of client.Message — existing queue-based fields are
// preserved and new fingerprint fields are added.
type Message struct {
	Type    string             `json:"type"`
	Payload *sync.SyncPayload `json:"payload,omitempty"`
	Limit   string            `json:"limit,omitempty"`
	Error   string            `json:"error,omitempty"`

	// Fingerprint reconciliation fields.
	FPBuckets *[NumBuckets]uint64  `json:"fpBuckets,omitempty"`
	FPItems   []sync.SyncStoreItem `json:"fpItems,omitempty"`
}

// Connection is a bidirectional transport for the fingerprint sync protocol.
type Connection interface {
	PeerID() string
	ReadMessage() (Message, error)
	WriteMessage(msg Message) error
	Close() error
}

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
	mu     gosync.Mutex
	done   chan struct{}
	active bool
}

// FingerprintClient manages connections with both queue-based sync and
// fingerprint reconciliation.
type FingerprintClient struct {
	store      *StoreFingerprint
	interval   time.Duration // queue-based pull interval
	fpInterval time.Duration // fingerprint reconciliation interval
	pageSize   int
	limit      int

	connsMu   gosync.RWMutex
	conns     []*managedConn
	syncInIDs gosync.Map

	done   chan struct{}
	unsub  func()
	closed bool
	mu     gosync.Mutex
}

// Option configures a FingerprintClient.
type Option func(*FingerprintClient)

// WithInterval sets the queue-based sync polling interval.
func WithInterval(d time.Duration) Option {
	return func(c *FingerprintClient) { c.interval = d }
}

// WithFingerprintInterval sets the fingerprint reconciliation interval.
func WithFingerprintInterval(d time.Duration) Option {
	return func(c *FingerprintClient) { c.fpInterval = d }
}

// WithLimit sets the max items per sync request.
func WithLimit(limit int) Option {
	return func(c *FingerprintClient) { c.limit = limit }
}

// WithPageSize sets the number of items per SyncOut page.
func WithPageSize(size int) Option {
	return func(c *FingerprintClient) { c.pageSize = size }
}

// NewClient creates a FingerprintClient.
func NewClient(store *StoreFingerprint, opts ...Option) *FingerprintClient {
	c := &FingerprintClient{
		store:      store,
		interval:   5 * time.Second,
		fpInterval: 30 * time.Second,
		pageSize:   100,
		done:       make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}

	c.unsub = c.store.OnUpdate(func(item sync.SyncStoreItem) {
		if _, ok := c.syncInIDs.Load(item.ID); ok {
			return
		}
		go c.pushActive()
		go c.broadcast(nil)
	})

	return c
}

// Connect dials a remote peer and starts syncing.
func (c *FingerprintClient) Connect(peerID, url string, header http.Header) error {
	conn, err := Dial(peerID, url, header)
	if err != nil {
		return err
	}
	return c.addConn(conn, true)
}

// AddConnection adds a server-side connection.
func (c *FingerprintClient) AddConnection(conn Connection) error {
	return c.addConn(conn, false)
}

func (c *FingerprintClient) addConn(conn Connection, active bool) error {
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
		go c.fpReconcileLoop(mc)

		if err := c.sendSyncRequest(mc); err != nil {
			log.Printf("fingerprint client: initial pull error: %v", err)
		}
	}

	return nil
}

// Done returns a channel closed when the client shuts down.
func (c *FingerprintClient) Done() <-chan struct{} {
	return c.done
}

func (c *FingerprintClient) removeConn(mc *managedConn) {
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
func (c *FingerprintClient) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close stops all loops and closes all connections.
func (c *FingerprintClient) Close() error {
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

// --- Queue-based sync ---

func (c *FingerprintClient) sendSyncRequest(mc *managedConn) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	msg := Message{Type: "sync_request"}
	if c.limit > 0 {
		msg.Limit = fmt.Sprintf("%d", c.limit)
	}
	return mc.WriteMessage(msg)
}

func (c *FingerprintClient) sendPush(mc *managedConn) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	for {
		payload, err := c.store.SyncOut(mc.PeerID(), c.pageSize)
		if err != nil {
			return fmt.Errorf("SyncOut: %w", err)
		}
		if len(payload.Items) == 0 {
			return nil
		}
		if err := mc.WriteMessage(Message{Type: "sync_push", Payload: payload}); err != nil {
			return err
		}
		if len(payload.Items) < c.pageSize {
			return nil
		}
	}
}

func (c *FingerprintClient) pushActive() {
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
			log.Printf("fingerprint client: push error: %v", err)
		}
	}
}

func (c *FingerprintClient) broadcast(sender *managedConn) {
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
			log.Printf("fingerprint client: broadcast error: %v", err)
		}
	}
}

// --- Fingerprint reconciliation ---

func (c *FingerprintClient) sendFingerprintRequest(mc *managedConn) error {
	fp, err := c.store.ComputeFingerprints()
	if err != nil {
		return fmt.Errorf("ComputeFingerprints: %w", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	return mc.WriteMessage(Message{
		Type:      "fingerprint_request",
		FPBuckets: &fp.Buckets,
	})
}

func (c *FingerprintClient) handleFingerprintRequest(mc *managedConn, remoteBuckets *[NumBuckets]uint64) {
	localFP, err := c.store.ComputeFingerprints()
	if err != nil {
		log.Printf("fingerprint client: ComputeFingerprints error: %v", err)
		return
	}

	remoteFP := &Fingerprints{Buckets: *remoteBuckets}
	diffBuckets := DiffBuckets(localFP, remoteFP)

	var items []sync.SyncStoreItem
	if len(diffBuckets) > 0 {
		items, err = c.store.ItemsForBuckets(diffBuckets)
		if err != nil {
			log.Printf("fingerprint client: ItemsForBuckets error: %v", err)
			return
		}
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.WriteMessage(Message{
		Type:      "fingerprint_response",
		FPBuckets: &localFP.Buckets,
		FPItems:   items,
	})
}

func (c *FingerprintClient) handleFingerprintResponse(mc *managedConn, remoteBuckets *[NumBuckets]uint64, remoteItems []sync.SyncStoreItem) {
	// Apply remote items.
	if len(remoteItems) > 0 {
		for i := range remoteItems {
			c.syncInIDs.Store(remoteItems[i].ID, struct{}{})
		}
		payload := sync.SyncPayload{Items: remoteItems}
		if err := c.store.SyncIn(mc.PeerID(), payload); err != nil {
			log.Printf("fingerprint client: SyncIn error: %v", err)
		}
		for i := range remoteItems {
			c.syncInIDs.Delete(remoteItems[i].ID)
		}
	}

	// Recompute local fingerprints after applying remote items,
	// then send our items for any still-differing buckets.
	localFP, err := c.store.ComputeFingerprints()
	if err != nil {
		log.Printf("fingerprint client: ComputeFingerprints error: %v", err)
		return
	}

	remoteFP := &Fingerprints{Buckets: *remoteBuckets}
	diffBuckets := DiffBuckets(localFP, remoteFP)
	if len(diffBuckets) == 0 {
		return
	}

	items, err := c.store.ItemsForBuckets(diffBuckets)
	if err != nil {
		log.Printf("fingerprint client: ItemsForBuckets error: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.WriteMessage(Message{
		Type:    "fingerprint_diff",
		FPItems: items,
	})
}

func (c *FingerprintClient) handleFingerprintDiff(mc *managedConn, items []sync.SyncStoreItem) {
	if len(items) == 0 {
		return
	}
	for i := range items {
		c.syncInIDs.Store(items[i].ID, struct{}{})
	}
	payload := sync.SyncPayload{Items: items}
	if err := c.store.SyncIn(mc.PeerID(), payload); err != nil {
		log.Printf("fingerprint client: SyncIn error: %v", err)
	}
	for i := range items {
		c.syncInIDs.Delete(items[i].ID)
	}
}

// --- Read/Pull/Reconcile loops ---

func (c *FingerprintClient) readLoop(mc *managedConn) {
	defer c.removeConn(mc)
	for {
		msg, err := mc.ReadMessage()
		if err != nil {
			select {
			case <-mc.done:
			case <-c.done:
			default:
				log.Printf("fingerprint client: read error: %v", err)
			}
			return
		}

		switch msg.Type {
		// --- Queue-based sync (server role) ---

		case "sync_request":
			pageSize := c.pageSize
			if msg.Limit != "" {
				var requested int
				fmt.Sscanf(msg.Limit, "%d", &requested)
				if requested > 0 && requested < pageSize {
					pageSize = requested
				}
			}
			mc.mu.Lock()
			var syncErr error
			for {
				payload, err := c.store.SyncOut(mc.PeerID(), pageSize)
				if err != nil {
					syncErr = err
					break
				}
				if err := mc.WriteMessage(Message{Type: "sync_response", Payload: payload}); err != nil {
					syncErr = err
					break
				}
				if len(payload.Items) < pageSize {
					break
				}
			}
			mc.mu.Unlock()
			if syncErr != nil {
				log.Printf("fingerprint client: SyncOut error: %v", syncErr)
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "internal error"})
				mc.mu.Unlock()
			}

		case "sync_push":
			if msg.Payload == nil {
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "missing payload"})
				mc.mu.Unlock()
				continue
			}
			for i := range msg.Payload.Items {
				c.syncInIDs.Store(msg.Payload.Items[i].ID, struct{}{})
			}
			err := c.store.SyncIn(mc.PeerID(), *msg.Payload)
			for i := range msg.Payload.Items {
				c.syncInIDs.Delete(msg.Payload.Items[i].ID)
			}
			if err != nil {
				log.Printf("fingerprint client: SyncIn error: %v", err)
				mc.mu.Lock()
				mc.WriteMessage(Message{Type: "error", Error: "internal error"})
				mc.mu.Unlock()
				continue
			}
			mc.mu.Lock()
			mc.WriteMessage(Message{Type: "sync_ack"})
			mc.mu.Unlock()
			c.broadcast(mc)

		// --- Queue-based sync (client role) ---

		case "sync_response":
			if msg.Payload != nil {
				for i := range msg.Payload.Items {
					c.syncInIDs.Store(msg.Payload.Items[i].ID, struct{}{})
				}
				err := c.store.SyncIn(mc.PeerID(), *msg.Payload)
				for i := range msg.Payload.Items {
					c.syncInIDs.Delete(msg.Payload.Items[i].ID)
				}
				if err != nil {
					log.Printf("fingerprint client: SyncIn error: %v", err)
				}
			}

		case "sync_ack":
			// Push acknowledged.

		case "sync_update":
			if err := c.sendSyncRequest(mc); err != nil {
				log.Printf("fingerprint client: pull on update error: %v", err)
			}

		// --- Fingerprint reconciliation ---

		case "fingerprint_request":
			if msg.FPBuckets != nil {
				go c.handleFingerprintRequest(mc, msg.FPBuckets)
			}

		case "fingerprint_response":
			if msg.FPBuckets != nil {
				go c.handleFingerprintResponse(mc, msg.FPBuckets, msg.FPItems)
			}

		case "fingerprint_diff":
			go c.handleFingerprintDiff(mc, msg.FPItems)

		case "error":
			log.Printf("fingerprint client: remote error: %s", msg.Error)
		}
	}
}

func (c *FingerprintClient) pullLoop(mc *managedConn) {
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
				log.Printf("fingerprint client: pull error: %v", err)
			}
		}
	}
}

func (c *FingerprintClient) fpReconcileLoop(mc *managedConn) {
	ticker := time.NewTicker(c.fpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mc.done:
			return
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.sendFingerprintRequest(mc); err != nil {
				log.Printf("fingerprint client: reconcile error: %v", err)
			}
		}
	}
}
