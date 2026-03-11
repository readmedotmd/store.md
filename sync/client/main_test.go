package client_test

import (
	"net/http"
	"net/http/httptest"
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
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem("app", "key1", "val1"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.SetItem("app", "key2", "val2"); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithInterval(50*time.Millisecond))
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	v1, err := clientStore.Get("key1")
	if err != nil {
		t.Fatalf("Get key1: %v", err)
	}
	if v1 != "val1" {
		t.Fatalf("expected val1, got %q", v1)
	}

	v2, err := clientStore.Get("key2")
	if err != nil {
		t.Fatalf("Get key2: %v", err)
	}
	if v2 != "val2" {
		t.Fatalf("expected val2, got %q", v2)
	}
}

func TestClient_PushToServer(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithInterval(10*time.Second))
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for the initial sync exchange to complete before writing,
	// so the push doesn't race with it.
	time.Sleep(200 * time.Millisecond)

	if err := clientStore.SetItem("app", "pushed-key", "pushed-val"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	item, err := serverStore.GetItem("pushed-key")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Value != "pushed-val" {
		t.Fatalf("expected pushed-val, got %q", item.Value)
	}
}

func TestClient_Broadcast(t *testing.T) {
	serverStore := newSyncStore(t)
	ts := startServer(t, serverStore)

	// Client A
	storeA := newSyncStore(t)
	cA := client.New(storeA, client.WithInterval(10*time.Second))
	defer cA.Close()
	if err := cA.Connect("peer-a", wsURL(ts), authHeader("token-a")); err != nil {
		t.Fatal(err)
	}

	// Client B
	storeB := newSyncStore(t)
	cB := client.New(storeB, client.WithInterval(10*time.Second))
	defer cB.Close()
	if err := cB.Connect("peer-b", wsURL(ts), authHeader("token-b")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Client A writes — should push to server, server broadcasts to B, B pulls.
	if err := storeA.SetItem("app", "broadcast-key", "from-a"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	val, err := storeB.Get("broadcast-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "from-a" {
		t.Fatalf("expected from-a, got %q", val)
	}
}

func TestClient_MultipleConnections(t *testing.T) {
	serverStore1 := newSyncStore(t)
	serverStore2 := newSyncStore(t)

	if err := serverStore1.SetItem("app", "s1-key", "s1-val"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore2.SetItem("app", "s2-key", "s2-val"); err != nil {
		t.Fatal(err)
	}

	ts1 := startServer(t, serverStore1)
	ts2 := startServer(t, serverStore2)

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithInterval(50*time.Millisecond))
	defer c.Close()

	if err := c.Connect("peer-for-s1", wsURL(ts1), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect("peer-for-s2", wsURL(ts2), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	v1, err := clientStore.Get("s1-key")
	if err != nil {
		t.Fatalf("Get s1-key: %v", err)
	}
	if v1 != "s1-val" {
		t.Fatalf("expected s1-val, got %q", v1)
	}

	v2, err := clientStore.Get("s2-key")
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
	c := client.New(clientStore, client.WithInterval(50*time.Millisecond))

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
