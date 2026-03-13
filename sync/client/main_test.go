package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
