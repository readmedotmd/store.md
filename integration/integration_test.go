package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
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
			s := storesync.New(memory.New(), int64(100*time.Millisecond))
			t.Cleanup(func() { s.Close() })
			return s
		},
	},
	{
		Name: "StoreMessage",
		NewStore: func(t *testing.T) storesync.SyncStore {
			ss := storesync.New(memory.New(), int64(100*time.Millisecond))
			m := message.New(ss, fmt.Sprintf("msg-%d", time.Now().UnixNano()))
			t.Cleanup(func() {
				m.Close()
				ss.Close()
			})
			return m
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
func newClient(store storesync.SyncStore) *client.Client {
	return client.New(store)
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
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			if err := serverStore.SetItem(ctx, "app", "key1", "val1"); err != nil {
				t.Fatal(err)
			}
			if err := serverStore.SetItem(ctx, "app", "key2", "val2"); err != nil {
				t.Fatal(err)
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Client pulls server's data.
			pollFor(t, 3*time.Second, "client gets key1", func() bool {
				v, err := clientStore.Get(ctx, "key1")
				return err == nil && v == "val1"
			})
			pollFor(t, 3*time.Second, "client gets key2", func() bool {
				v, err := clientStore.Get(ctx, "key2")
				return err == nil && v == "val2"
			})

			// Client writes, pushes to server.
			if err := clientStore.SetItem(ctx, "app", "client-key", "client-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets client-key", func() bool {
				v, err := serverStore.Get(ctx, "client-key")
				return err == nil && v == "client-val"
			})
		})
	}
}

func TestTwoClientBroadcast(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			storeA := f.NewStore(t)
			cA := newClient(storeA)
			defer cA.Close()
			if err := cA.Connect("peer-a", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			storeB := f.NewStore(t)
			cB := newClient(storeB)
			defer cB.Close()
			if err := cB.Connect("peer-b", wsURL(ts), header("token-b")); err != nil {
				t.Fatal(err)
			}

			time.Sleep(50 * time.Millisecond)

			if err := storeA.SetItem(ctx, "app", "from-a", "hello"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets from-a", func() bool {
				v, err := serverStore.Get(ctx, "from-a")
				return err == nil && v == "hello"
			})

			pollFor(t, 3*time.Second, "client-b gets from-a", func() bool {
				v, err := storeB.Get(ctx, "from-a")
				return err == nil && v == "hello"
			})
		})
	}
}

func TestConflictResolution(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			// Server writes first (older timestamp).
			serverStore.SetItem(ctx, "app", "conflict", "server-val")
			time.Sleep(5 * time.Millisecond)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets server-val", func() bool {
				v, err := clientStore.Get(ctx, "conflict")
				return err == nil && v == "server-val"
			})

			// Client writes newer value.
			clientStore.SetItem(ctx, "app", "conflict", "client-wins")

			pollFor(t, 3*time.Second, "server accepts client-wins", func() bool {
				v, err := serverStore.Get(ctx, "conflict")
				return err == nil && v == "client-wins"
			})
		})
	}
}

func TestDeleteSync(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			serverStore.SetItem(ctx, "app", "to-delete", "exists")

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets to-delete", func() bool {
				v, err := clientStore.Get(ctx, "to-delete")
				return err == nil && v == "exists"
			})

			if err := clientStore.Delete(ctx, "to-delete"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server sees delete", func() bool {
				_, err := serverStore.Get(ctx, "to-delete")
				return errors.Is(err, storemd.ErrNotFound)
			})
		})
	}
}

func TestManyItems(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			for i := range 50 {
				if err := serverStore.SetItem(ctx, "app", fmt.Sprintf("item-%03d", i), fmt.Sprintf("value-%d", i)); err != nil {
					t.Fatal(err)
				}
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "client gets all 50 items", func() bool {
				for i := range 50 {
					v, err := clientStore.Get(ctx, fmt.Sprintf("item-%03d", i))
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
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			time.Sleep(50 * time.Millisecond)

			for i := range 10 {
				serverStore.SetItem(ctx, "app", fmt.Sprintf("srv-%d", i), fmt.Sprintf("srv-val-%d", i))
			}
			for i := range 10 {
				clientStore.SetItem(ctx, "app", fmt.Sprintf("cli-%d", i), fmt.Sprintf("cli-val-%d", i))
			}

			pollFor(t, 5*time.Second, "both stores converge", func() bool {
				for i := range 10 {
					v, err := clientStore.Get(ctx, fmt.Sprintf("srv-%d", i))
					if err != nil || v != fmt.Sprintf("srv-val-%d", i) {
						return false
					}
					v, err = serverStore.Get(ctx, fmt.Sprintf("cli-%d", i))
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
	ctx := context.Background()
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
			cA := newClient(storeA)
			defer cA.Close()
			if err := cA.Connect("peer-b", wsURL(tsB), header("token-a")); err != nil {
				t.Fatal(err)
			}

			cB := newClient(storeB)
			defer cB.Close()
			if err := cB.Connect("peer-c", wsURL(tsC), header("token-b")); err != nil {
				t.Fatal(err)
			}

			cC := newClient(storeC)
			defer cC.Close()
			if err := cC.Connect("peer-a", wsURL(tsA), header("token-c")); err != nil {
				t.Fatal(err)
			}

			// Let initial sync settle.
			time.Sleep(50 * time.Millisecond)

			// A writes — should propagate A→B→C→A (full ring).
			if err := storeA.SetItem(ctx, "app", "from-a", "a-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "B gets from-a", func() bool {
				v, err := storeB.Get(ctx, "from-a")
				return err == nil && v == "a-val"
			})
			pollFor(t, 5*time.Second, "C gets from-a", func() bool {
				v, err := storeC.Get(ctx, "from-a")
				return err == nil && v == "a-val"
			})

			// B writes — should reach C and A.
			if err := storeB.SetItem(ctx, "app", "from-b", "b-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "C gets from-b", func() bool {
				v, err := storeC.Get(ctx, "from-b")
				return err == nil && v == "b-val"
			})
			pollFor(t, 5*time.Second, "A gets from-b", func() bool {
				v, err := storeA.Get(ctx, "from-b")
				return err == nil && v == "b-val"
			})

			// C writes — should reach A and B.
			if err := storeC.SetItem(ctx, "app", "from-c", "c-val"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 5*time.Second, "A gets from-c", func() bool {
				v, err := storeA.Get(ctx, "from-c")
				return err == nil && v == "c-val"
			})
			pollFor(t, 5*time.Second, "B gets from-c", func() bool {
				v, err := storeB.Get(ctx, "from-c")
				return err == nil && v == "c-val"
			})

			// Verify all three stores have all three keys.
			for name, store := range map[string]storesync.SyncStore{"A": storeA, "B": storeB, "C": storeC} {
				for _, key := range []string{"from-a", "from-b", "from-c"} {
					v, err := store.Get(ctx, key)
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
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore1 := f.NewStore(t)
			serverStore2 := f.NewStore(t)

			serverStore1.SetItem(ctx, "app", "s1-key", "s1-val")
			serverStore2.SetItem(ctx, "app", "s2-key", "s2-val")

			ts1 := startServer(t, serverStore1)
			ts2 := startServer(t, serverStore2)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("peer-for-s1", wsURL(ts1), header("token-a")); err != nil {
				t.Fatal(err)
			}
			if err := c.Connect("peer-for-s2", wsURL(ts2), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets s1-key", func() bool {
				v, err := clientStore.Get(ctx, "s1-key")
				return err == nil && v == "s1-val"
			})
			pollFor(t, 3*time.Second, "client gets s2-key", func() bool {
				v, err := clientStore.Get(ctx, "s2-key")
				return err == nil && v == "s2-val"
			})
		})
	}
}

// --- New Tests ---

func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Wait for connection to establish.
			time.Sleep(50 * time.Millisecond)

			// Multiple goroutines writing to the server store simultaneously.
			const numWriters = 10
			const writesPerWriter = 5
			var wg sync.WaitGroup
			wg.Add(numWriters)
			for w := range numWriters {
				go func(writer int) {
					defer wg.Done()
					for i := range writesPerWriter {
						key := fmt.Sprintf("w%d-k%d", writer, i)
						val := fmt.Sprintf("v%d-%d", writer, i)
						if err := serverStore.SetItem(ctx, "app", key, val); err != nil {
							t.Errorf("writer %d failed on key %d: %v", writer, i, err)
						}
					}
				}(w)
			}
			wg.Wait()

			// Verify all writes are visible on the server.
			for w := range numWriters {
				for i := range writesPerWriter {
					key := fmt.Sprintf("w%d-k%d", w, i)
					expected := fmt.Sprintf("v%d-%d", w, i)
					v, err := serverStore.Get(ctx, key)
					if err != nil {
						t.Fatalf("server missing %s: %v", key, err)
					}
					if v != expected {
						t.Fatalf("server has %s=%q, want %q", key, v, expected)
					}
				}
			}

			// Verify all writes sync to client.
			totalKeys := numWriters * writesPerWriter
			pollFor(t, 10*time.Second, fmt.Sprintf("client gets all %d keys", totalKeys), func() bool {
				for w := range numWriters {
					for i := range writesPerWriter {
						key := fmt.Sprintf("w%d-k%d", w, i)
						expected := fmt.Sprintf("v%d-%d", w, i)
						v, err := clientStore.Get(ctx, key)
						if err != nil || v != expected {
							return false
						}
					}
				}
				return true
			})
		})
	}
}

func TestLargePayloadSync(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)

			const numItems = 200
			for i := range numItems {
				key := fmt.Sprintf("large-%04d", i)
				val := fmt.Sprintf("payload-%d", i)
				if err := serverStore.SetItem(ctx, "app", key, val); err != nil {
					t.Fatal(err)
				}
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			if err := c.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 15*time.Second, fmt.Sprintf("client gets all %d items", numItems), func() bool {
				for i := range numItems {
					key := fmt.Sprintf("large-%04d", i)
					expected := fmt.Sprintf("payload-%d", i)
					v, err := clientStore.Get(ctx, key)
					if err != nil || v != expected {
						return false
					}
				}
				return true
			})
		})
	}
}

// recordingConn wraps a client.Connection and records all sent/received messages.
type recordingConn struct {
	inner client.Connection
	mu    sync.Mutex
	sent  []client.Message
	recv  []client.Message
}

func newRecordingConn(inner client.Connection) *recordingConn {
	return &recordingConn{inner: inner}
}

func (r *recordingConn) PeerID() string { return r.inner.PeerID() }

func (r *recordingConn) ReadMessage() (client.Message, error) {
	msg, err := r.inner.ReadMessage()
	if err == nil {
		r.mu.Lock()
		r.recv = append(r.recv, msg)
		r.mu.Unlock()
	}
	return msg, err
}

func (r *recordingConn) WriteMessage(msg client.Message) error {
	err := r.inner.WriteMessage(msg)
	if err == nil {
		r.mu.Lock()
		r.sent = append(r.sent, msg)
		r.mu.Unlock()
	}
	return err
}

func (r *recordingConn) Close() error { return r.inner.Close() }

func (r *recordingConn) Sent() []client.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]client.Message, len(r.sent))
	copy(out, r.sent)
	return out
}

func (r *recordingConn) Recv() []client.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]client.Message, len(r.recv))
	copy(out, r.recv)
	return out
}

// resetRecording clears recorded messages (useful between test phases).
func (r *recordingConn) resetRecording() {
	r.mu.Lock()
	r.sent = nil
	r.recv = nil
	r.mu.Unlock()
}

// uniqueItemKeys returns the set of unique item keys across all sync messages.
func uniqueItemKeys(msgs []client.Message) map[string]bool {
	keys := make(map[string]bool)
	for _, msg := range msgs {
		if msg.Type == "sync" && msg.Payload != nil {
			for _, item := range msg.Payload.Items {
				keys[item.Key] = true
			}
		}
	}
	return keys
}

// TestNetworkEfficiency_WriteAfterSync verifies that after an initial sync,
// writes on client and server only produce messages carrying the new items —
// no empty payloads, and only the expected item keys appear in the traffic.
func TestNetworkEfficiency_WriteAfterSync(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)
			serverStore.SetItem(ctx, "app", "initial", "value")

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c := newClient(clientStore)
			defer c.Close()

			// Dial manually so we can wrap with recording conn.
			conn, err := client.Dial("server-peer", wsURL(ts), header("token-a"))
			if err != nil {
				t.Fatal(err)
			}
			rec := newRecordingConn(conn)
			if err := c.AddConnection(rec); err != nil {
				t.Fatal(err)
			}

			// Wait for initial sync to complete and time offset window to pass.
			pollFor(t, 3*time.Second, "client gets initial", func() bool {
				v, err := clientStore.Get(ctx, "initial")
				return err == nil && v == "value"
			})
			// Wait for the time offset window (100ms) to pass so cursors settle.
			time.Sleep(300 * time.Millisecond)
			rec.resetRecording()

			// --- Phase 1: Client writes a new item ---
			if err := clientStore.SetItem(ctx, "app", "client-write", "cval"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets client-write", func() bool {
				v, err := serverStore.Get(ctx, "client-write")
				return err == nil && v == "cval"
			})
			time.Sleep(300 * time.Millisecond)

			sent := rec.Sent()
			if len(sent) == 0 {
				t.Error("expected at least one sent message after client write")
			}
			// Every sent sync message must carry items (no empty payloads).
			for i, msg := range sent {
				if msg.Type == "sync" && (msg.Payload == nil || len(msg.Payload.Items) == 0) {
					t.Errorf("sent message %d is an empty sync payload (no items)", i)
				}
			}
			// The new item must appear in sent messages.
			sentKeys := uniqueItemKeys(sent)
			if !sentKeys["client-write"] {
				t.Error("client-write not found in sent messages")
			}
			t.Logf("client write phase: %d sent, %d recv, sent keys: %v",
				len(sent), len(rec.Recv()), sentKeys)

			// --- Phase 2: Server writes a new item ---
			rec.resetRecording()

			if err := serverStore.SetItem(ctx, "app", "server-write", "sval"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "client gets server-write", func() bool {
				v, err := clientStore.Get(ctx, "server-write")
				return err == nil && v == "sval"
			})
			time.Sleep(300 * time.Millisecond)

			recv := rec.Recv()
			if len(recv) == 0 {
				t.Error("expected at least one received message after server write")
			}
			// The received messages should contain the server-write item.
			recvKeys := uniqueItemKeys(recv)
			if !recvKeys["server-write"] {
				t.Error("server-write not found in received messages")
			}
			t.Logf("server write phase: %d sent, %d recv, recv keys: %v",
				len(rec.Sent()), len(recv), recvKeys)
		})
	}
}

// TestNetworkEfficiency_Reconnect verifies that reconnecting a store that
// already has data only exchanges the items that changed since disconnect —
// not a full re-sync of everything.
func TestNetworkEfficiency_Reconnect(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)

			// Seed server with initial data.
			for i := range 5 {
				serverStore.SetItem(ctx, "app", fmt.Sprintf("pre-%d", i), fmt.Sprintf("val-%d", i))
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c1 := newClient(clientStore)

			if err := c1.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Wait for initial sync and the time offset window to pass.
			pollFor(t, 3*time.Second, "client gets pre-4", func() bool {
				v, err := clientStore.Get(ctx, "pre-4")
				return err == nil && v == "val-4"
			})
			// Wait for cursors to fully advance past the time offset window.
			time.Sleep(300 * time.Millisecond)

			// Disconnect.
			c1.Close()
			time.Sleep(100 * time.Millisecond)

			// Server writes one new item while client is disconnected.
			if err := serverStore.SetItem(ctx, "app", "new-after-disconnect", "new-val"); err != nil {
				t.Fatal(err)
			}

			// Reconnect with recording connection.
			c2 := newClient(clientStore)
			defer c2.Close()

			conn, err := client.Dial("server-peer", wsURL(ts), header("token-a"))
			if err != nil {
				t.Fatal(err)
			}
			rec := newRecordingConn(conn)
			if err := c2.AddConnection(rec); err != nil {
				t.Fatal(err)
			}

			// Wait for the new item to arrive.
			pollFor(t, 3*time.Second, "client gets new-after-disconnect", func() bool {
				v, err := clientStore.Get(ctx, "new-after-disconnect")
				return err == nil && v == "new-val"
			})
			time.Sleep(300 * time.Millisecond)

			// Collect unique item keys received from server.
			recv := rec.Recv()
			recvKeys := uniqueItemKeys(recv)

			t.Logf("reconnect phase: %d messages recv, %d messages sent, unique keys recv: %v",
				len(recv), len(rec.Sent()), recvKeys)

			// The new item must be present.
			if !recvKeys["new-after-disconnect"] {
				t.Error("new-after-disconnect not found in received messages")
			}

			// Pre-existing items that the client already has should NOT be re-sent.
			resent := 0
			for i := range 5 {
				if recvKeys[fmt.Sprintf("pre-%d", i)] {
					resent++
				}
			}
			if resent > 0 {
				t.Errorf("server re-sent %d of 5 pre-existing items on reconnect (expected 0)", resent)
			}

			// Verify all data is present on the client.
			for i := range 5 {
				v, err := clientStore.Get(ctx, fmt.Sprintf("pre-%d", i))
				if err != nil {
					t.Fatalf("missing pre-%d after reconnect: %v", i, err)
				}
				if v != fmt.Sprintf("val-%d", i) {
					t.Fatalf("pre-%d = %q, want %q", i, v, fmt.Sprintf("val-%d", i))
				}
			}
			v, err := clientStore.Get(ctx, "new-after-disconnect")
			if err != nil {
				t.Fatal(err)
			}
			if v != "new-val" {
				t.Fatalf("new-after-disconnect = %q, want %q", v, "new-val")
			}
		})
	}
}

func TestReconnect(t *testing.T) {
	ctx := context.Background()
	for _, f := range factories {
		t.Run(f.Name, func(t *testing.T) {
			serverStore := f.NewStore(t)

			// Seed server with initial data.
			if err := serverStore.SetItem(ctx, "app", "before-disconnect", "initial"); err != nil {
				t.Fatal(err)
			}

			ts := startServer(t, serverStore)

			clientStore := f.NewStore(t)
			c1 := newClient(clientStore)

			if err := c1.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Wait for initial sync.
			pollFor(t, 3*time.Second, "client gets before-disconnect", func() bool {
				v, err := clientStore.Get(ctx, "before-disconnect")
				return err == nil && v == "initial"
			})

			// Client writes data before disconnecting.
			if err := clientStore.SetItem(ctx, "app", "client-before", "from-client"); err != nil {
				t.Fatal(err)
			}

			pollFor(t, 3*time.Second, "server gets client-before", func() bool {
				v, err := serverStore.Get(ctx, "client-before")
				return err == nil && v == "from-client"
			})

			// Disconnect.
			c1.Close()

			// Server writes while client is disconnected.
			if err := serverStore.SetItem(ctx, "app", "during-disconnect", "missed"); err != nil {
				t.Fatal(err)
			}

			// Reconnect with a new client using the same store.
			c2 := newClient(clientStore)
			defer c2.Close()

			if err := c2.Connect("server-peer", wsURL(ts), header("token-a")); err != nil {
				t.Fatal(err)
			}

			// Verify client picks up data written during disconnect.
			pollFor(t, 3*time.Second, "client gets during-disconnect", func() bool {
				v, err := clientStore.Get(ctx, "during-disconnect")
				return err == nil && v == "missed"
			})

			// Verify old data is still present.
			pollFor(t, 3*time.Second, "client still has before-disconnect", func() bool {
				v, err := clientStore.Get(ctx, "before-disconnect")
				return err == nil && v == "initial"
			})
		})
	}
}
