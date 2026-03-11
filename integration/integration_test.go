package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
	"github.com/readmedotmd/store.md/sync/message"
	"github.com/readmedotmd/store.md/sync/server"
)

// Factory produces the components needed for a single integration test run.
type Factory struct {
	Name     string
	NewStore func(t *testing.T) storesync.SyncStore
}

var tokens = map[string]string{
	"token-a": "peer-a",
	"token-b": "peer-b",
	"token-c": "peer-c",
}

var factories = []Factory{
	{
		Name: "StoreSync",
		NewStore: func(t *testing.T) storesync.SyncStore {
			return storesync.New(memory.New(), int64(100*time.Millisecond))
		},
	},
	{
		Name: "StoreMessage",
		NewStore: func(t *testing.T) storesync.SyncStore {
			ss := storesync.New(memory.New(), int64(100*time.Millisecond))
			return message.New(ss, fmt.Sprintf("msg-%d", time.Now().UnixNano()))
		},
	},
}

func header(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

func wsURL(ts *httptest.Server) string {
	return "ws" + ts.URL[4:]
}

// newServer creates a server.Server for any SyncStore.
func newServer(store storesync.SyncStore) *server.Server {
	srv := server.New(store, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})
	return srv
}

// newClient creates a client.Client for any SyncStore.
func newClient(store storesync.SyncStore, interval time.Duration) *client.Client {
	return client.New(store, client.WithInterval(interval))
}

// startServer creates an httptest.Server, registers cleanup for both the HTTP
// server and the sync server adapter goroutines.
func startServer(t *testing.T, store storesync.SyncStore) *httptest.Server {
	t.Helper()
	srv := newServer(store)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts
}

// pollFor polls until condition returns true or timeout.
func pollFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

// --- Tests ---

func TestFullRoundTrip(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			if err := serverStore.SetItem("app", "key1", "val1"); err != nil {
				t.Fatal(err)
			}
			if err := serverStore.SetItem("app", "key2", "val2"); err != nil {
				t.Fatal(err)
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Client pulls server's data.
			pollFor(t, 3*time.Second, "client gets key1", func() bool {
				v, err := clientStore.Get("key1")
				return err == nil && v == "val1"
			})
			pollFor(t, 3*time.Second, "client gets key2", func() bool {
				v, err := clientStore.Get("key2")
				return err == nil && v == "val2"
			})

			// Client writes, pushes to server.
			if err := clientStore.SetItem("app", "client-key", "client-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets client-key", func() bool {
				v, err := serverStore.Get("client-key")
				return err == nil && v == "client-val"
			})
		})
	}
}

func TestTwoClientBroadcast(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			storeA := f.NewStore(t)
			cA := newClient(storeA, 10*time.Second)
			defer cA.Close()
			if err := cA.Connect("peer-a", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			storeB := f.NewStore(t)
			cB := newClient(storeB, 10*time.Second)
			defer cB.Close()
			if err := cB.Connect("peer-b", wsURL(ts), header("token-b")); err != nil {
				t.Fatal(err)
			}

			time.Sleep(200 * time.Millisecond)

			if err := storeA.SetItem("app", "from-a", "hello"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets from-a", func() bool {
				v, err := serverStore.Get("from-a")
				return err == nil && v == "hello"
			})

			pollFor(t, 3*time.Second, "client-b gets from-a", func() bool {
				v, err := storeB.Get("from-a")
				return err == nil && v == "hello"
			})
		})
	}
}

func TestConflictResolution(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			// Server writes first (older timestamp).
			serverStore.SetItem("app", "conflict", "server-val")
			time.Sleep(5 * time.Millisecond)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets server-val", func() bool {
				v, err := clientStore.Get("conflict")
				return err == nil && v == "server-val"
			})

			// Client writes newer value.
			clientStore.SetItem("app", "conflict", "client-wins")

			pollFor(t, 3*time.Second, "server accepts client-wins", func() bool {
				v, err := serverStore.Get("conflict")
				return err == nil && v == "client-wins"
			})
		})
	}
}

func TestDeleteSync(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			serverStore.SetItem("app", "to-delete", "exists")

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets to-delete", func() bool {
				v, err := clientStore.Get("to-delete")
				return err == nil && v == "exists"
			})

			if err := clientStore.Delete("to-delete"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server sees delete", func() bool {
				_, err := serverStore.Get("to-delete")
				return err != nil
			})
		})
	}
}

func TestManyItems(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			for i := range 50 {
				if err := serverStore.SetItem("app", fmt.Sprintf("item-%03d", i), fmt.Sprintf("value-%d", i)); err != nil {
					t.Fatal(err)
				}
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "client gets all 50 items", func() bool {
				for i := range 50 {
					v, err := clientStore.Get(fmt.Sprintf("item-%03d", i))
					if err != nil || v != fmt.Sprintf("value-%d", i) {
						return false
					}
				}
				return true
			})
		})
	}
}

func TestBidirectionalSimultaneous(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			time.Sleep(200 * time.Millisecond)

			for i := range 10 {
				serverStore.SetItem("app", fmt.Sprintf("srv-%d", i), fmt.Sprintf("srv-val-%d", i))
			}
			for i := range 10 {
				clientStore.SetItem("app", fmt.Sprintf("cli-%d", i), fmt.Sprintf("cli-val-%d", i))
			}

			pollFor(t, 5*time.Second, "both stores converge", func() bool {
				for i := range 10 {
					v, err := clientStore.Get(fmt.Sprintf("srv-%d", i))
					if err != nil || v != fmt.Sprintf("srv-val-%d", i) {
						return false
					}
					v, err = serverStore.Get(fmt.Sprintf("cli-%d", i))
					if err != nil || v != fmt.Sprintf("cli-val-%d", i) {
						return false
					}
				}
				return true
			})
		})
	}
}

// TestTriangleSync sets up A → B → C → A: three servers, each with a client
// connecting to the next in a ring. A write on any node should propagate to all.
func TestTriangleSync(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			// Create three stores + servers.
			storeA := f.NewStore(t)
			storeB := f.NewStore(t)
			storeC := f.NewStore(t)

			tsA := startServer(t, storeA)
			tsB := startServer(t, storeB)
			tsC := startServer(t, storeC)

			// A connects to B, B connects to C, C connects to A.
			cA := newClient(storeA, 50*time.Millisecond)
			defer cA.Close()
			if err := cA.Connect("peer-b", wsURL(tsB), header("token-a")); err != nil {
				t.Fatal(err)
			}

			cB := newClient(storeB, 50*time.Millisecond)
			defer cB.Close()
			if err := cB.Connect("peer-c", wsURL(tsC), header("token-b")); err != nil {
				t.Fatal(err)
			}

			cC := newClient(storeC, 50*time.Millisecond)
			defer cC.Close()
			if err := cC.Connect("peer-a", wsURL(tsA), header("token-c")); err != nil {
				t.Fatal(err)
			}

			// Let initial sync settle.
			time.Sleep(300 * time.Millisecond)

			// A writes — should propagate A→B→C→A (full ring).
			if err := storeA.SetItem("app", "from-a", "a-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "B gets from-a", func() bool {
				v, err := storeB.Get("from-a")
				return err == nil && v == "a-val"
			})
			pollFor(t, 5*time.Second, "C gets from-a", func() bool {
				v, err := storeC.Get("from-a")
				return err == nil && v == "a-val"
			})

			// B writes — should reach C and A.
			if err := storeB.SetItem("app", "from-b", "b-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "C gets from-b", func() bool {
				v, err := storeC.Get("from-b")
				return err == nil && v == "b-val"
			})
			pollFor(t, 5*time.Second, "A gets from-b", func() bool {
				v, err := storeA.Get("from-b")
				return err == nil && v == "b-val"
			})

			// C writes — should reach A and B.
			if err := storeC.SetItem("app", "from-c", "c-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "A gets from-c", func() bool {
				v, err := storeA.Get("from-c")
				return err == nil && v == "c-val"
			})
			pollFor(t, 5*time.Second, "B gets from-c", func() bool {
				v, err := storeB.Get("from-c")
				return err == nil && v == "c-val"
			})

			// Verify all three stores have all three keys.
			for name, store := range map[string]storesync.SyncStore{"A": storeA, "B": storeB, "C": storeC} {
				for _, key := range []string{"from-a", "from-b", "from-c"} {
					v, err := store.Get(key)
					if err != nil {
						t.Fatalf("%s missing %s: %v", name, key, err)
					}
					expected := key[len("from-"):] + "-val"
					if v != expected {
						t.Fatalf("%s has %s=%q, want %q", name, key, v, expected)
					}
				}
			}
		})
	}
}

func TestMultipleConnections(t *testing.T) {
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore1 := f.NewStore(t)
			serverStore2 := f.NewStore(t)

			serverStore1.SetItem("app", "s1-key", "s1-val")
			serverStore2.SetItem("app", "s2-key", "s2-val")

			ts1 := startServer(t, serverStore1)
			ts2 := startServer(t, serverStore2)

			clientStore := f.NewStore(t)
			c := newClient(clientStore, 50*time.Millisecond)
			defer c.Close()

			if err := c.Connect("peer-for-s1", wsURL(ts1), header("token-a")); err != nil {
				t.Fatal(err)
			}
			if err := c.Connect("peer-for-s2", wsURL(ts2), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets s1-key", func() bool {
				v, err := clientStore.Get("s1-key")
				return err == nil && v == "s1-val"
			})
			pollFor(t, 3*time.Second, "client gets s2-key", func() bool {
				v, err := clientStore.Get("s2-key")
				return err == nil && v == "s2-val"
			})
		})
	}
}
