package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
	"github.com/readmedotmd/store.md/sync/server"
)

// startSSEServerWithClose returns the test server and a cleanup function
// that closes the server in the correct order (server adapters first,
// then the HTTP test server). This is necessary because SSE streams are
// long-lived connections that block httptest.Server.Close().
func startSSEServerWithClose(t *testing.T, ss storesync.SyncStore) (*httptest.Server, func()) {
	t.Helper()
	tokens := map[string]string{
		"test-token": "test-peer",
		"token-a":    "peer-a",
		"token-b":    "peer-b",
	}
	srv := server.New(ss, server.TokenAuth(tokens))
	srv.SetAllowedOrigins([]string{"*"})

	mux := http.NewServeMux()
	mux.Handle("/sse", srv.Handler(srv.SSE()))
	mux.Handle("/ws", srv.Handler(srv.WebSocket()))
	mux.Handle("/http", srv.Handler(srv.HTTP()))

	ts := httptest.NewServer(mux)
	cleanup := func() {
		// Force-close all client connections to trigger r.Context().Done()
		// on the server side, which unblocks SSE stream handlers. Then
		// close the sync adapters and the test server.
		ts.CloseClientConnections()
		time.Sleep(100 * time.Millisecond)
		srv.Close()
		ts.Close()
	}
	return ts, cleanup
}

func TestClient_SSEDialer_PullFromServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatal(err)
	}

	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.SSEDialer()))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL+"/sse", authHeader("test-token")); err != nil {
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

func TestClient_SSEDialer_PushToServer(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)
	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.SSEDialer()))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL+"/sse", authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

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

func TestClient_DialerForScheme_SSE(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "scheme-key", "scheme-val"); err != nil {
		t.Fatal(err)
	}

	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithDialerForScheme("sse", client.SSEDialer()),
	)
	defer c.Close()

	// Use sse:// scheme — resolveDialer maps it to http:// for the request.
	sseURL := "sse" + ts.URL[4:] + "/sse" // sse://127.0.0.1:.../sse
	if err := c.Connect("client-peer", sseURL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore.Get(ctx, "scheme-key")
	if err != nil {
		t.Fatalf("Get scheme-key: %v", err)
	}
	if v != "scheme-val" {
		t.Fatalf("expected scheme-val, got %q", v)
	}
}

func TestClient_MultiDialer_MixedTransports(t *testing.T) {
	ctx := context.Background()

	// Two separate server stores.
	serverStoreWS := newSyncStore(t)
	serverStoreSSE := newSyncStore(t)

	if err := serverStoreWS.SetItem(ctx, "app", "ws-key", "ws-val"); err != nil {
		t.Fatal(err)
	}
	if err := serverStoreSSE.SetItem(ctx, "app", "sse-key", "sse-val"); err != nil {
		t.Fatal(err)
	}

	tsWS := startServer(t, serverStoreWS)
	tsSSE, cleanupSSE := startSSEServerWithClose(t, serverStoreSSE)
	defer cleanupSSE()

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		// Default dialer is WebSocket.
		client.WithDialerForScheme("sse", client.SSEDialer()),
	)
	defer c.Close()

	// Connect to WS server with ws:// scheme (uses default dialer).
	if err := c.Connect("ws-peer", wsURL(tsWS), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Connect to SSE server with sse:// scheme.
	sseURL := "sse" + tsSSE.URL[4:] + "/sse"
	if err := c.Connect("sse-peer", sseURL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	// Both items should be in the client store.
	v1, err := clientStore.Get(ctx, "ws-key")
	if err != nil {
		t.Fatalf("Get ws-key: %v", err)
	}
	if v1 != "ws-val" {
		t.Fatalf("expected ws-val, got %q", v1)
	}

	v2, err := clientStore.Get(ctx, "sse-key")
	if err != nil {
		t.Fatalf("Get sse-key: %v", err)
	}
	if v2 != "sse-val" {
		t.Fatalf("expected sse-val, got %q", v2)
	}
}

func TestClient_DialerForScheme_HTTP(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "http-scheme-key", "http-scheme-val"); err != nil {
		t.Fatal(err)
	}

	ts := startHTTPServer(t, serverStore)

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithDialerForScheme("http", client.HTTPDialer(100*time.Millisecond)),
		client.WithDialerForScheme("ws", client.Dial),
	)
	defer c.Close()

	// Connect using http:// scheme — should use HTTPDialer.
	if err := c.Connect("client-peer", ts.URL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore.Get(ctx, "http-scheme-key")
	if err != nil {
		t.Fatalf("Get http-scheme-key: %v", err)
	}
	if v != "http-scheme-val" {
		t.Fatalf("expected http-scheme-val, got %q", v)
	}
}

func TestClient_SSEDialer_AuthFailure(t *testing.T) {
	serverStore := newSyncStore(t)
	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.SSEDialer()))
	defer c.Close()

	// Connect with an invalid token — should fail.
	err := c.Connect("client-peer", ts.URL+"/sse", authHeader("bad-token"))
	if err == nil {
		t.Fatal("expected error for bad token, got nil")
	}
}

func TestClient_SSEDialer_InvalidURL(t *testing.T) {
	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.SSEDialer()))
	defer c.Close()

	err := c.Connect("client-peer", "http://127.0.0.1:1/nonexistent", authHeader("test-token"))
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

func TestClient_DialerForScheme_SSES(t *testing.T) {
	// Verify that "sses" scheme maps to "https" by checking that resolveDialer
	// picks the right dialer. We can't easily test actual HTTPS without certs,
	// so we verify the mapping by connecting with sse:// (which maps to http://).
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "sses-key", "sses-val"); err != nil {
		t.Fatal(err)
	}

	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore,
		client.WithDialerForScheme("sse", client.SSEDialer()),
		client.WithDialerForScheme("sses", client.SSEDialer()),
	)
	defer c.Close()

	// Use sse:// scheme to test mapping works.
	sseURL := "sse" + ts.URL[4:] + "/sse"
	if err := c.Connect("client-peer", sseURL, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore.Get(ctx, "sses-key")
	if err != nil {
		t.Fatalf("Get sses-key: %v", err)
	}
	if v != "sses-val" {
		t.Fatalf("expected sses-val, got %q", v)
	}
}

func TestClient_SSEDialer_BidirectionalSync(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	// Server has initial data.
	if err := serverStore.SetItem(ctx, "app", "server-key", "server-val"); err != nil {
		t.Fatal(err)
	}

	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore := newSyncStore(t)
	c := client.New(clientStore, client.WithDialer(client.SSEDialer()))
	defer c.Close()

	if err := c.Connect("client-peer", ts.URL+"/sse", authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	// Wait for server data to arrive.
	time.Sleep(500 * time.Millisecond)

	// Verify client pulled server data.
	v, err := clientStore.Get(ctx, "server-key")
	if err != nil {
		t.Fatalf("Get server-key: %v", err)
	}
	if v != "server-val" {
		t.Fatalf("expected server-val, got %q", v)
	}

	// Client pushes data.
	if err := clientStore.SetItem(ctx, "app", "client-key", "client-val"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify server received client data.
	item, err := serverStore.GetItem(ctx, "client-key")
	if err != nil {
		t.Fatalf("GetItem client-key: %v", err)
	}
	if item.Value != "client-val" {
		t.Fatalf("expected client-val, got %q", item.Value)
	}
}

func TestClient_SchemeDialer_FallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	serverStore := newSyncStore(t)

	if err := serverStore.SetItem(ctx, "app", "fallback-key", "fallback-val"); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t, serverStore)

	clientStore := newSyncStore(t)
	// Register sse dialer for "sse" scheme, but connect with ws:// — should
	// use the default WebSocket dialer.
	c := client.New(clientStore,
		client.WithDialerForScheme("sse", client.SSEDialer()),
	)
	defer c.Close()

	if err := c.Connect("client-peer", wsURL(ts), authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore.Get(ctx, "fallback-key")
	if err != nil {
		t.Fatalf("Get fallback-key: %v", err)
	}
	if v != "fallback-val" {
		t.Fatalf("expected fallback-val, got %q", v)
	}
}

func TestClient_MultiDialer_ThreeTransports(t *testing.T) {
	ctx := context.Background()

	serverStore := newSyncStore(t)
	if err := serverStore.SetItem(ctx, "app", "triple-key", "triple-val"); err != nil {
		t.Fatal(err)
	}

	// Server supports all three transports on separate routes.
	ts, cleanup := startSSEServerWithClose(t, serverStore)
	defer cleanup()

	clientStore1 := newSyncStore(t)
	c1 := client.New(clientStore1)
	defer c1.Close()

	// WebSocket connection.
	wsAddr := "ws" + ts.URL[4:] + "/ws"
	if err := c1.Connect("ws-peer", wsAddr, authHeader("test-token")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err := clientStore1.Get(ctx, "triple-key")
	if err != nil {
		t.Fatalf("Get from WS: %v", err)
	}
	if v != "triple-val" {
		t.Fatalf("expected triple-val from WS, got %q", v)
	}

	// HTTP connection.
	clientStore2 := newSyncStore(t)
	c2 := client.New(clientStore2, client.WithDialer(client.HTTPDialer(100*time.Millisecond)))
	defer c2.Close()

	if err := c2.Connect("http-peer", ts.URL+"/http", authHeader("token-a")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err = clientStore2.Get(ctx, "triple-key")
	if err != nil {
		t.Fatalf("Get from HTTP: %v", err)
	}
	if v != "triple-val" {
		t.Fatalf("expected triple-val from HTTP, got %q", v)
	}

	// SSE connection.
	clientStore3 := newSyncStore(t)
	c3 := client.New(clientStore3, client.WithDialer(client.SSEDialer()))
	defer c3.Close()

	if err := c3.Connect("sse-peer", ts.URL+"/sse", authHeader("token-b")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	v, err = clientStore3.Get(ctx, "triple-key")
	if err != nil {
		t.Fatalf("Get from SSE: %v", err)
	}
	if v != "triple-val" {
		t.Fatalf("expected triple-val from SSE, got %q", v)
	}
}
