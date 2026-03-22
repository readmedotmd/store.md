//go:build !js || !wasm

package client

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	gosync "sync"
	"time"

	storesync "github.com/readmedotmd/store.md/sync/core"
)

// InboxClient connects to one or more InboxServers to exchange raw sync
// items without a local SyncStore. Received items are delivered to the
// onReceive callback; outgoing items are sent via [InboxClient.Send].
//
// InboxClient uses the same transport layer as [Client] — all three
// built-in transports (WebSocket, HTTP, SSE) work interchangeably.
// Choose the transport by passing a different Dialer:
//
//   - WebSocket (default): persistent bidirectional connection
//   - HTTP polling: stateless POST-based, works everywhere
//   - SSE: server-push via event stream, writes via POST
//
// # Example
//
//	ic := client.NewInbox(
//	    func(peerID string, items []core.SyncStoreItem) {
//	        for _, item := range items {
//	            fmt.Printf("received %s=%s from %s\n", item.Key, item.Value, peerID)
//	        }
//	    },
//	)
//	defer ic.Close()
//	ic.Connect("server", "ws://host/sync", headers)
//	ic.Send([]core.SyncStoreItem{{App: "chat", Key: "msg1", Value: "hello"}})
type InboxClient struct {
	logger  *slog.Logger
	dialer  Dialer
	dialers map[string]Dialer

	maxConns int

	connsMu gosync.RWMutex
	conns   []*managedConn

	onReceive func(peerID string, items []storesync.SyncStoreItem)

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

	wg     gosync.WaitGroup
	done   chan struct{}
	closed bool
	mu     gosync.Mutex
}

// InboxOption configures an InboxClient.
type InboxOption func(*InboxClient)

// WithInboxLogger sets the structured logger.
func WithInboxLogger(l *slog.Logger) InboxOption {
	return func(c *InboxClient) { c.logger = l }
}

// WithInboxMaxConns sets the maximum number of connections. Default is 100.
func WithInboxMaxConns(n int) InboxOption {
	return func(c *InboxClient) { c.maxConns = n }
}

// WithInboxDialer sets a custom dialer for creating outbound connections.
// By default, the client uses the WebSocket [Dial] function.
func WithInboxDialer(d Dialer) InboxOption {
	return func(c *InboxClient) { c.dialer = d }
}

// WithInboxDialerForScheme registers a dialer for a specific URL scheme,
// enabling a single client to connect to multiple servers using different
// transports. See [WithDialerForScheme] for details.
func WithInboxDialerForScheme(scheme string, d Dialer) InboxOption {
	return func(c *InboxClient) {
		if c.dialers == nil {
			c.dialers = make(map[string]Dialer)
		}
		c.dialers[scheme] = d
	}
}

// WithInboxReconnect enables automatic reconnection with exponential
// backoff. See [WithReconnect] for details.
func WithInboxReconnect(initialBackoff, maxBackoff time.Duration) InboxOption {
	return func(c *InboxClient) {
		c.reconnect = true
		if initialBackoff > 0 {
			c.reconnectInitial = initialBackoff
		}
		if maxBackoff > 0 {
			c.reconnectMax = maxBackoff
		}
	}
}

// WithInboxHeaderProvider sets a function that returns HTTP headers for
// each dial attempt, enabling token refresh on reconnection.
func WithInboxHeaderProvider(fn func(peerID, url string) (http.Header, error)) InboxOption {
	return func(c *InboxClient) { c.headerProvider = fn }
}

// OnInboxConnect registers an interceptor invoked after a connection is
// established. Return a non-nil error to reject the connection.
func OnInboxConnect(fn func(peerID string) error) InboxOption {
	return func(c *InboxClient) { c.onConnect = append(c.onConnect, fn) }
}

// OnInboxDisconnect registers an interceptor invoked when a connection
// drops unexpectedly. Return false to suppress automatic reconnection.
func OnInboxDisconnect(fn func(peerID string, err error) bool) InboxOption {
	return func(c *InboxClient) { c.onDisconnect = append(c.onDisconnect, fn) }
}

// OnInboxConnectError registers an interceptor invoked when a dial attempt
// fails. Return false to stop retrying.
func OnInboxConnectError(fn func(peerID string, err error) bool) InboxOption {
	return func(c *InboxClient) { c.onConnectError = append(c.onConnectError, fn) }
}

// NewInbox creates an InboxClient that delivers received items to the
// onReceive callback. The callback is called from the read goroutine of
// the connection that received the items.
func NewInbox(onReceive func(peerID string, items []storesync.SyncStoreItem), opts ...InboxOption) *InboxClient {
	c := &InboxClient{
		logger:           slog.Default(),
		dialer:           Dial,
		maxConns:         100,
		reconnectInitial: 1 * time.Second,
		reconnectMax:     30 * time.Second,
		done:             make(chan struct{}),
		onReceive:        onReceive,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Connect dials a remote InboxServer and starts receiving items. The
// default dialer uses WebSocket; use [WithInboxDialer] or
// [WithInboxDialerForScheme] to switch transports.
func (c *InboxClient) Connect(peerID, rawURL string, header http.Header) error {
	if c.headerProvider != nil {
		h, err := c.headerProvider(peerID, rawURL)
		if err != nil {
			return fmt.Errorf("header provider: %w", err)
		}
		header = h
	}
	dialer, dialURL := c.resolveDialer(rawURL)
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
		di = &dialInfo{peerID: peerID, url: rawURL, header: h}
	}
	return c.addConn(conn, di)
}

// Send sends items to all connected servers.
func (c *InboxClient) Send(items []storesync.SyncStoreItem) error {
	msg := Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: items},
	}
	c.connsMu.RLock()
	conns := make([]*managedConn, len(c.conns))
	copy(conns, c.conns)
	c.connsMu.RUnlock()

	var firstErr error
	for _, mc := range conns {
		mc.mu.Lock()
		if err := mc.WriteMessage(msg); err != nil && firstErr == nil {
			firstErr = err
		}
		mc.mu.Unlock()
	}
	return firstErr
}

// Done returns a channel that is closed when the client is closed.
func (c *InboxClient) Done() <-chan struct{} { return c.done }

// Closed returns true if the client has been closed.
func (c *InboxClient) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close stops all goroutines and closes all connections.
func (c *InboxClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	close(c.done)

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

func (c *InboxClient) addConn(conn Connection, di *dialInfo) error {
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
		active:     true,
		dialInfo:   di,
	}

	c.connsMu.Lock()
	c.conns = append(c.conns, mc)
	c.connsMu.Unlock()

	for _, fn := range c.onConnect {
		if err := fn(conn.PeerID()); err != nil {
			c.removeConn(mc)
			conn.Close()
			return fmt.Errorf("onConnect rejected: %w", err)
		}
	}

	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.readLoop(mc) }()

	// Send empty sync to fetch any pending items from the server.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		mc.mu.Lock()
		mc.WriteMessage(Message{Type: "sync", Payload: &storesync.SyncPayload{}})
		mc.mu.Unlock()
	}()

	return nil
}

func (c *InboxClient) removeConn(mc *managedConn) {
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

// readLoop reads messages from a connection and delivers items to onReceive.
func (c *InboxClient) readLoop(mc *managedConn) {
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
			if msg.Payload != nil && len(msg.Payload.Items) > 0 && c.onReceive != nil {
				c.onReceive(mc.PeerID(), msg.Payload.Items)
			}
		case "error":
			c.logger.Error("remote error", "msg", msg.Error)
		}
	}
}

func (c *InboxClient) reconnectLoop(di *dialInfo) {
	defer func() {
		c.pendingReconnMu.Lock()
		c.pendingReconn--
		pending := c.pendingReconn
		c.pendingReconnMu.Unlock()

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

			backoff = time.Duration(float64(c.reconnectInitial) * math.Pow(2, float64(attempt)))
			if backoff > c.reconnectMax {
				backoff = c.reconnectMax
			}
			continue
		}

		c.logger.Info("reconnected", "peer", di.peerID, "attempts", attempt+1)
		if err := c.addConn(conn, di); err != nil {
			c.logger.Error("reconnect addConn failed", "peer", di.peerID, "err", err)
			conn.Close()
			return
		}
		return
	}
}

func (c *InboxClient) resolveDialer(rawURL string) (Dialer, string) {
	if len(c.dialers) == 0 {
		return c.dialer, rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return c.dialer, rawURL
	}

	scheme := u.Scheme
	if d, ok := c.dialers[scheme]; ok {
		if realScheme, mapped := schemeMapping[scheme]; mapped {
			u.Scheme = realScheme
			return d, u.String()
		}
		return d, rawURL
	}

	return c.dialer, rawURL
}
