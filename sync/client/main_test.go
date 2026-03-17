package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
	"github.com/readmedotmd/store.md/sync/server"
)

func newSyncStore(t *testing.T) storesync.SyncStore {
	t.Helper()
	return storesync.New(memory.New(), int64(100*time.Millisecond))
}

func startServer(t *testing.T, ss storesync.SyncStore) *httptest.Server {
	t.Helper()
	tokens := map[string]string{
		"test-token": "test-peer",
		"token-a":    "peer-a",
		"token-b":    "peer-b",
	}
	srv := server.New(ss, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server) string {
	return "ws" + ts.URL[4:]
}

func authHeader(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

func TestClient_PullFromServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	v1, err := clientStore.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get key1: %v", err)
	}
	if v1 != "val1" {
		t.Fatalf("expected val1, got %q", v1)
	}

	v2, err := clientStore.Get(ctx, "key2")
	if err != nil {
		t.Fatalf("Get key2: %v", err)
	}
	if v2 != "val2" {
		t.Fatalf("expected val2, got %q", v2)
	}
}

func TestClient_PushToServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for the initial sync exchange to complete before writing,
	// so the push doesn't race with it.
	time.Sleep(200 * time.Millisecond)

	if err := clientStore.SetItem(ctx, "app", "pushed-key", "pushed-val"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	item, err := serverStore.GetItem(ctx, "pushed-key")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Value != "pushed-val" {
		t.Fatalf("expected pushed-val, got %q", item.Value)
	}
}

func TestClient_Broadcast(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	// Client A
	storeA := newSyncStore(t)
	cA := client.New(storeA)
	defer cA.Close()
	if err := cA.Connect("peer-a", wsURL(ts), authHeader("token-a")); err != nil {
		t.Fatal(err)
	}

	// Client B
	storeB := newSyncStore(t)
	cB := client.New(storeB)
	defer cB.Close()
	if err := cB.Connect("peer-b", wsURL(ts), authHeader("token-b")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Client A writes — should push to server, server broadcasts to B, B pulls.
	if err := storeA.SetItem(ctx, "app", "broadcast-key", "from-a"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	val, err := storeB.Get(ctx, "broadcast-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "from-a" {
		t.Fatalf("expected from-a, got %q", val)
	}
}

func TestClient_MultipleConnections(t *testing.T) {
	ctx := context.Background()
	serverStore1 := newSyncStore(t)
	serverStore2 := newSyncStore(t)

	if err := serverStore1.SetItem(ctx, "app", "s1-key", "s1-val"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore2.SetItem(ctx, "app", "s2-key", "s2-val"); err != nil {
		t.Fatal(err)
	}

	ts1 := startServer(t, serverStore1)
	ts2 := startServer(t, serverStore2)

	clientStore := newSyncStore(t)
	c := client.New(clientStore)
	defer c.Close()

	if err := c.Connect("peer-for-s1", wsURL(ts1), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect("peer-for-s2", wsURL(ts2), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	v1, err := clientStore.Get(ctx, "s1-key")
	if err != nil {
		t.Fatalf("Get s1-key: %v", err)
	}
	if v1 != "s1-val" {
		t.Fatalf("expected s1-val, got %q", v1)
	}

	v2, err := clientStore.Get(ctx, "s2-key")
	if err != nil {
		t.Fatalf("Get s2-key: %v", err)
	}
	if v2 != "s2-val" {
		t.Fatalf("expected s2-val, got %q", v2)
	}
}

func TestClient_Close(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore)

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func startHTTPServer(t *testing.T, ss storesync.SyncStore) *httptest.Server {
	t.Helper()
	tokens := map[string]string{
		"test-token": "test-peer",
		"token-a":    "peer-a",
		"token-b":    "peer-b",
	}
	srv := server.New(ss, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})
	srv.EnableHTTP()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestClient_HTTPDialer_PullFromServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatal(err)
	}

	ts := startHTTPServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.HTTPDialer(100*time.Millisecond)))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v1, err := clientStore.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get key1: %v", err)
	}
	if v1 != "val1" {
		t.Fatalf("expected val1, got %q", v1)
	}

	v2, err := clientStore.Get(ctx, "key2")
	if err != nil {
		t.Fatalf("Get key2: %v", err)
	}
	if v2 != "val2" {
		t.Fatalf("expected val2, got %q", v2)
	}
}

func TestClient_HTTPDialer_PushToServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)
	ts := startHTTPServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.HTTPDialer(100*time.Millisecond)))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for initial sync exchange to complete before writing.
	time.Sleep(300 * time.Millisecond)

	if err := clientStore.SetItem(ctx, "app", "pushed-key", "pushed-val"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	item, err := serverStore.GetItem(ctx, "pushed-key")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Value != "pushed-val" {
		t.Fatalf("expected pushed-val, got %q", item.Value)
	}
}

func TestClient_CustomDialer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "custom-key", "custom-val"); err != nil {
		t.Fatal(err)
	}

	ts := startHTTPServer(t, serverStore)

	dialerCalled := false
	customDialer := func(peerID, url string, header http.Header) (client.Connection, error) {
		dialerCalled = true
		return client.HTTPDialer(100 * time.Millisecond)(peerID, url, header)
	}

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(customDialer))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	if !dialerCalled {
		t.Fatal("custom dialer was not called")
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore.Get(ctx, "custom-key")
	if err != nil {
		t.Fatalf("Get custom-key: %v", err)
	}
	if v != "custom-val" {
		t.Fatalf("expected custom-val, got %q", v)
	}
}

// countingConn wraps a Connection and counts write operations.
type countingConn struct {
	client.Connection
	writes *atomic.Int64
}

func (c *countingConn) WriteMessage(msg client.Message) error {
	c.writes.Add(1)
	return c.Connection.WriteMessage(msg)
}

func TestNoSyncLoop_TwoClients(t *testing.T) {
	ctx := context.Background()
	// Use the default 10-second time offset to reproduce real-world behavior.
	// With small offsets (like 100ms in other tests), items' WriteTimestamps
	// become past-cursor quickly and the bug is masked.
	serverStore := storesync.New(memory.New())
	ts := startServer(t, serverStore)

	// Client A with message counting
	storeA := storesync.New(memory.New())
	var writesA atomic.Int64
	clientA := client.New(storeA, client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
		conn, err := client.Dial(peerID, url, header)
		if err != nil {
			return nil, err
		}
		return &countingConn{Connection: conn, writes: &writesA}, nil
	}))
	t.Cleanup(func() { clientA.Close() })

	// Client B with message counting
	storeB := storesync.New(memory.New())
	var writesB atomic.Int64
	clientB := client.New(storeB, client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
		conn, err := client.Dial(peerID, url, header)
		if err != nil {
			return nil, err
		}
		return &countingConn{Connection: conn, writes: &writesB}, nil
	}))
	t.Cleanup(func() { clientB.Close() })

	if err := clientA.Connect("server", wsURL(ts), authHeader("token-a")); err != nil {
		t.Fatal(err)
	}
	if err := clientB.Connect("server", wsURL(ts), authHeader("token-b")); err != nil {
		t.Fatal(err)
	}

	// Wait for initial sync handshake to settle.
	time.Sleep(500 * time.Millisecond)

	// Write one item on Client A.
	if err := storeA.SetItem(ctx, "app", "key1", "value1"); err != nil {
		t.Fatal(err)
	}

	// Wait for propagation to Client B.
	deadline := time.After(3 * time.Second)
	for {
		item, err := storeB.GetItem(ctx, "key1")
		if err == nil && item.Value == "value1" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for item to propagate to Client B")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Let the system settle, then snapshot message counts.
	time.Sleep(500 * time.Millisecond)
	snapshotA := writesA.Load()
	snapshotB := writesB.Load()

	// Wait and check that no more messages are being exchanged (quiescence).
	time.Sleep(1 * time.Second)
	finalA := writesA.Load()
	finalB := writesB.Load()

	// Allow at most 2 extra messages per client for stragglers.
	if finalA-snapshotA > 2 {
		t.Errorf("sync loop detected on Client A: %d messages sent after settling (snapshot=%d, final=%d)",
			finalA-snapshotA, snapshotA, finalA)
	}
	if finalB-snapshotB > 2 {
		t.Errorf("sync loop detected on Client B: %d messages sent after settling (snapshot=%d, final=%d)",
			finalB-snapshotB, snapshotB, finalB)
	}
}

// breakableConn wraps a Connection and allows force-closing it to simulate
// a network failure. This is needed because httptest.Server.Close() does not
// immediately close hijacked WebSocket connections.
type breakableConn struct {
	client.Connection
	done chan struct{}
	once gosync.Once
}

func newBreakableConn(c client.Connection) *breakableConn {
	return &breakableConn{Connection: c, done: make(chan struct{})}
}

func (b *breakableConn) Break() {
	b.once.Do(func() { close(b.done) })
	b.Connection.Close()
}

func (b *breakableConn) ReadMessage() (client.Message, error) {
	select {
	case <-b.done:
		return client.Message{}, fmt.Errorf("connection broken")
	default:
		return b.Connection.ReadMessage()
	}
}

func TestClient_Reconnect_StaysAlive(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatal(err)
	}

	var activeConn atomic.Pointer[breakableConn]
	var dialCount atomic.Int64

	tokens := map[string]string{"test-token": "test-peer"}
	srv := server.New(serverStore, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			n := dialCount.Add(1)
			conn, err := client.Dial(peerID, url, header)
			if err != nil {
				return nil, err
			}
			bc := newBreakableConn(conn)
			if n == 1 {
				activeConn.Store(bc)
			}
			return bc, nil
		}),
	)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for initial sync.
	time.Sleep(300 * time.Millisecond)

	v, err := clientStore.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get key1: %v", err)
	}
	if v != "val1" {
		t.Fatalf("expected val1, got %q", v)
	}

	// Break the connection.
	activeConn.Load().Break()

	// Wait for reconnection.
	deadline := time.After(3 * time.Second)
	for dialCount.Load() <= 1 {
		select {
		case <-deadline:
			t.Fatalf("expected reconnection, got %d dials", dialCount.Load())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Client should still be alive.
	if c.Closed() {
		t.Fatal("client should not be closed while reconnecting")
	}
}

func TestClient_Reconnect_BackoffRetries(t *testing.T) {
	var dialCount atomic.Int64

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			n := dialCount.Add(1)
			if n == 1 {
				// First dial succeeds with a connection that immediately errors.
				conn, err := client.Dial(peerID, url, header)
				if err != nil {
					return nil, err
				}
				bc := newBreakableConn(conn)
				// Break immediately to trigger reconnect.
				go func() {
					time.Sleep(50 * time.Millisecond)
					bc.Break()
				}()
				return bc, nil
			}
			// Subsequent dials fail.
			return nil, fmt.Errorf("simulated dial failure")
		}),
	)
	defer c.Close()

	// Start a real server just for the first connection.
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)
	defer ts.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for multiple reconnection attempts.
	deadline := time.After(3 * time.Second)
	for dialCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected at least 3 dial attempts, got %d", dialCount.Load())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Client should still be alive, retrying.
	if c.Closed() {
		t.Fatal("client should not be closed while reconnecting")
	}

	// Explicit close should stop reconnection.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.Closed() {
		t.Fatal("client should be closed after Close()")
	}
}

func TestClient_NoReconnect_WithoutOption(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)

	var activeConn atomic.Pointer[breakableConn]
	// No WithReconnect option — should auto-close when connection drops.
	c := client.New(clientStore, client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
		conn, err := client.Dial(peerID, url, header)
		if err != nil {
			return nil, err
		}
		bc := newBreakableConn(conn)
		activeConn.Store(bc)
		return bc, nil
	}))

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Break the connection.
	activeConn.Load().Break()

	// Wait for client to detect disconnect and auto-close.
	deadline := time.After(3 * time.Second)
	for !c.Closed() {
		select {
		case <-deadline:
			t.Fatal("client should auto-close without WithReconnect")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestClient_Reconnect_ServerSideConnNoReconnect(t *testing.T) {
	// Server-side connections (via AddConnection) should NOT reconnect.
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithReconnect(50*time.Millisecond, 200*time.Millisecond))

	conn, err := client.Dial("test-peer", wsURL(ts), authHeader("test-token"))
	if err != nil {
		t.Fatal(err)
	}
	bc := newBreakableConn(conn)
	if err := c.AddConnection(bc); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Break the connection.
	bc.Break()

	// Server-side connections should not reconnect, so client should auto-close.
	deadline := time.After(3 * time.Second)
	for !c.Closed() {
		select {
		case <-deadline:
			t.Fatal("client should auto-close for server-side connections even with WithReconnect")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestClient_OnConnect_Hook(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	var connected atomic.Int64
	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.OnConnect(func(peerID string) error {
		connected.Add(1)
		return nil
	}))
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if connected.Load() != 1 {
		t.Fatalf("expected OnConnect called once, got %d", connected.Load())
	}
}

func TestClient_OnDisconnect_Hook(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	var disconnected atomic.Int64
	var activeConn atomic.Pointer[breakableConn]

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.OnDisconnect(func(peerID string, err error) bool {
			disconnected.Add(1)
			return true
		}),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			conn, err := client.Dial(peerID, url, header)
			if err != nil {
				return nil, err
			}
			bc := newBreakableConn(conn)
			activeConn.Store(bc)
			return bc, nil
		}),
	)

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	activeConn.Load().Break()

	deadline := time.After(3 * time.Second)
	for disconnected.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("OnDisconnect was never called")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestClient_OnConnectError_Hook(t *testing.T) {
	var connectErrors atomic.Int64
	var dialCount atomic.Int64

	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.OnConnectError(func(peerID string, err error) bool {
			connectErrors.Add(1)
			return true
		}),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			n := dialCount.Add(1)
			if n == 1 {
				conn, err := client.Dial(peerID, url, header)
				if err != nil {
					return nil, err
				}
				bc := newBreakableConn(conn)
				go func() {
					time.Sleep(50 * time.Millisecond)
					bc.Break()
				}()
				return bc, nil
			}
			return nil, fmt.Errorf("simulated dial failure")
		}),
	)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for connectErrors.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("OnConnectError was never called")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestClient_HeaderProvider(t *testing.T) {
	var headerCalls atomic.Int64
	var activeConn atomic.Pointer[breakableConn]
	var dialCount atomic.Int64

	serverStore := newSyncStore(t)
	tokens := map[string]string{"token-v1": "test-peer", "token-v2": "test-peer"}
	srv := server.New(serverStore, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.WithHeaderProvider(func(peerID, url string) (http.Header, error) {
			n := headerCalls.Add(1)
			h := http.Header{}
			if n == 1 {
				h.Set("Authorization", "Bearer token-v1")
			} else {
				h.Set("Authorization", "Bearer token-v2")
			}
			return h, nil
		}),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			n := dialCount.Add(1)
			conn, err := client.Dial(peerID, url, header)
			if err != nil {
				return nil, err
			}
			bc := newBreakableConn(conn)
			if n == 1 {
				activeConn.Store(bc)
			}
			return bc, nil
		}),
	)
	defer c.Close()

	// Connect — headerProvider should be called for initial connect.
	if err := c.Connect("client-peer", wsURL(ts), nil); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	if headerCalls.Load() < 1 {
		t.Fatal("HeaderProvider was not called on initial connect")
	}

	// Break connection to trigger reconnect — headerProvider called again.
	activeConn.Load().Break()

	deadline := time.After(3 * time.Second)
	for headerCalls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("HeaderProvider not called on reconnect, calls=%d", headerCalls.Load())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// --- Interceptor behavior tests ---

func TestClient_OnConnect_RejectsConnection(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.OnConnect(func(peerID string) error {
		return fmt.Errorf("rejected: %s not allowed", peerID)
	}))
	defer c.Close()

	err := c.Connect("client-peer", wsURL(ts), authHeader("test-token"))
	if err == nil {
		t.Fatal("expected Connect to fail when OnConnect rejects")
	}
	if err.Error() != "onConnect rejected: rejected: client-peer not allowed" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_OnDisconnect_SuppressesReconnect(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	var dialCount atomic.Int64
	var activeConn atomic.Pointer[breakableConn]

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.OnDisconnect(func(peerID string, err error) bool {
			return false // suppress reconnection
		}),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			dialCount.Add(1)
			conn, err := client.Dial(peerID, url, header)
			if err != nil {
				return nil, err
			}
			bc := newBreakableConn(conn)
			activeConn.Store(bc)
			return bc, nil
		}),
	)

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Break the connection — reconnect should be suppressed.
	activeConn.Load().Break()

	// Client should auto-close because OnDisconnect returned false,
	// which suppresses reconnection.
	deadline := time.After(3 * time.Second)
	for !c.Closed() {
		select {
		case <-deadline:
			t.Fatal("client should auto-close when OnDisconnect suppresses reconnect")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Should have only dialed once — no reconnection attempt.
	if dialCount.Load() != 1 {
		t.Fatalf("expected 1 dial (no reconnect), got %d", dialCount.Load())
	}
}

func TestClient_OnConnectError_StopsRetrying(t *testing.T) {
	var dialCount atomic.Int64
	var errorCount atomic.Int64

	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithReconnect(50*time.Millisecond, 200*time.Millisecond),
		client.OnConnectError(func(peerID string, err error) bool {
			n := errorCount.Add(1)
			// Allow 2 retries, then give up.
			return n < 3
		}),
		client.WithDialer(func(peerID, url string, header http.Header) (client.Connection, error) {
			n := dialCount.Add(1)
			if n == 1 {
				conn, err := client.Dial(peerID, url, header)
				if err != nil {
					return nil, err
				}
				bc := newBreakableConn(conn)
				go func() {
					time.Sleep(50 * time.Millisecond)
					bc.Break()
				}()
				return bc, nil
			}
			return nil, fmt.Errorf("simulated dial failure")
		}),
	)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for reconnection to give up.
	deadline := time.After(5 * time.Second)
	for errorCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected 3 connect errors, got %d", errorCount.Load())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// After the hook returns false, retries should stop. Wait a bit and
	// verify the dial count hasn't grown beyond the initial + 3 retries.
	time.Sleep(500 * time.Millisecond)
	// 1 initial + ~3 reconnect attempts (the 3rd error stops retries)
	if dialCount.Load() > 5 {
		t.Fatalf("expected retries to stop, but got %d dials", dialCount.Load())
	}
}

func TestClient_OnConnect_MultipleHooks_FirstErrorWins(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	var hook1Called, hook2Called bool
	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.OnConnect(func(peerID string) error {
			hook1Called = true
			return fmt.Errorf("hook1 rejects")
		}),
		client.OnConnect(func(peerID string) error {
			hook2Called = true
			return nil
		}),
	)
	defer c.Close()

	err := c.Connect("client-peer", wsURL(ts), authHeader("test-token"))
	if err == nil {
		t.Fatal("expected error from first hook")
	}

	if !hook1Called {
		t.Fatal("first hook should have been called")
	}
	if hook2Called {
		t.Fatal("second hook should NOT be called after first rejects")
	}
}
