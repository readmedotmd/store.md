package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
	"github.com/readmedotmd/store.md/sync/server"
)

func startInboxServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := memory.New()
	tokens := map[string]string{
		"token-alice": "alice",
		"token-bob":   "bob",
		"token-carol": "carol",
	}
	inbox := server.NewInbox(store, server.TokenAuth(tokens))
	inbox.SetAllowedOrigins([]string{"*"})
	inbox.EnableHTTP()
	inbox.EnableSSE()

	ts := httptest.NewServer(inbox)
	t.Cleanup(ts.Close)
	return ts
}

func inboxWSURL(ts *httptest.Server) string {
	return "ws" + ts.URL[4:]
}

// collectItems returns a callback and a function to retrieve collected items.
func collectItems() (func(string, []storesync.SyncStoreItem), func() []storesync.SyncStoreItem) {
	var mu gosync.Mutex
	var collected []storesync.SyncStoreItem
	cb := func(_ string, items []storesync.SyncStoreItem) {
		mu.Lock()
		collected = append(collected, items...)
		mu.Unlock()
	}
	get := func() []storesync.SyncStoreItem {
		mu.Lock()
		defer mu.Unlock()
		out := make([]storesync.SyncStoreItem, len(collected))
		copy(out, collected)
		return out
	}
	return cb, get
}

func TestInboxClient_WebSocket_SendAndReceive(t *testing.T) {
	ts := startInboxServer(t)

	// Alice sends, Bob receives via InboxClient.
	bobCb, bobItems := collectItems()
	bob := client.NewInbox(bobCb)
	defer bob.Close()

	if err := bob.Connect("server", inboxWSURL(ts), authHeader("token-bob")); err != nil {
		t.Fatal(err)
	}

	// Alice sends an item via raw WebSocket.
	aliceWS := dialRawWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{App: "chat", Key: "msg1", Value: "hello", ID: "id-1"}
	sendRawSync(t, aliceWS, []storesync.SyncStoreItem{item})

	// Wait for Bob to receive the item.
	waitFor(t, func() bool { return len(bobItems()) >= 1 })

	items := bobItems()
	if items[0].Key != "msg1" || items[0].Value != "hello" {
		t.Fatalf("unexpected item: %+v", items[0])
	}
}

func TestInboxClient_WebSocket_Send(t *testing.T) {
	ts := startInboxServer(t)

	// Bob receives via raw WebSocket; Alice sends via InboxClient.
	bobWS := dialRawWS(t, ts, "token-bob")

	alice := client.NewInbox(func(string, []storesync.SyncStoreItem) {})
	defer alice.Close()

	if err := alice.Connect("server", inboxWSURL(ts), authHeader("token-alice")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond) // let connection settle

	item := storesync.SyncStoreItem{App: "chat", Key: "msg2", Value: "world", ID: "id-2"}
	if err := alice.Send([]storesync.SyncStoreItem{item}); err != nil {
		t.Fatal(err)
	}

	// Bob should receive via push or by sending a sync request.
	var resp client.Message
	bobWS.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := bobWS.ReadJSON(&resp); err != nil {
		// Might need to request items explicitly.
		sendRawSync(t, bobWS, nil)
		bobWS.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := bobWS.ReadJSON(&resp); err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	if resp.Payload == nil || len(resp.Payload.Items) == 0 {
		t.Fatal("expected items in response")
	}
	if resp.Payload.Items[0].Key != "msg2" {
		t.Fatalf("unexpected item: %+v", resp.Payload.Items[0])
	}
}

func TestInboxClient_HTTP(t *testing.T) {
	ts := startInboxServer(t)

	// Alice sends via InboxClient with HTTP dialer.
	aliceCb, _ := collectItems()
	alice := client.NewInbox(aliceCb, client.WithInboxDialer(client.HTTPDialer(100*time.Millisecond)))
	defer alice.Close()

	if err := alice.Connect("server", ts.URL, authHeader("token-alice")); err != nil {
		t.Fatal(err)
	}

	item := storesync.SyncStoreItem{App: "chat", Key: "http1", Value: "data", ID: "http-id-1"}
	if err := alice.Send([]storesync.SyncStoreItem{item}); err != nil {
		t.Fatal(err)
	}

	// Bob receives via InboxClient with HTTP dialer.
	bobCb, bobItems := collectItems()
	bob := client.NewInbox(bobCb, client.WithInboxDialer(client.HTTPDialer(100*time.Millisecond)))
	defer bob.Close()

	if err := bob.Connect("server", ts.URL, authHeader("token-bob")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(bobItems()) >= 1 })

	items := bobItems()
	if items[0].Key != "http1" || items[0].Value != "data" {
		t.Fatalf("unexpected item: %+v", items[0])
	}
}

func TestInboxClient_SSE(t *testing.T) {
	ts := startInboxServer(t)

	// Bob connects via SSE dialer.
	bobCb, bobItems := collectItems()
	bob := client.NewInbox(bobCb, client.WithInboxDialer(client.SSEDialer()))
	defer bob.Close()

	if err := bob.Connect("server", ts.URL, authHeader("token-bob")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond) // let SSE stream establish

	// Alice sends via raw WebSocket.
	aliceWS := dialRawWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{App: "chat", Key: "sse1", Value: "stream", ID: "sse-id-1"}
	sendRawSync(t, aliceWS, []storesync.SyncStoreItem{item})

	waitFor(t, func() bool { return len(bobItems()) >= 1 })

	items := bobItems()
	if items[0].Key != "sse1" || items[0].Value != "stream" {
		t.Fatalf("unexpected item: %+v", items[0])
	}
}

func TestInboxClient_TwoPeers(t *testing.T) {
	ts := startInboxServer(t)

	aliceCb, aliceItems := collectItems()
	bobCb, bobItems := collectItems()

	alice := client.NewInbox(aliceCb)
	defer alice.Close()
	bob := client.NewInbox(bobCb)
	defer bob.Close()

	if err := alice.Connect("server", inboxWSURL(ts), authHeader("token-alice")); err != nil {
		t.Fatal(err)
	}
	if err := bob.Connect("server", inboxWSURL(ts), authHeader("token-bob")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	// Alice sends an item.
	item1 := storesync.SyncStoreItem{App: "chat", Key: "a2b", Value: "from-alice", ID: "a1"}
	if err := alice.Send([]storesync.SyncStoreItem{item1}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(bobItems()) >= 1 })
	if bobItems()[0].Key != "a2b" {
		t.Fatalf("bob got wrong item: %+v", bobItems()[0])
	}

	// Bob sends an item.
	item2 := storesync.SyncStoreItem{App: "chat", Key: "b2a", Value: "from-bob", ID: "b1"}
	if err := bob.Send([]storesync.SyncStoreItem{item2}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(aliceItems()) >= 1 })
	if aliceItems()[0].Key != "b2a" {
		t.Fatalf("alice got wrong item: %+v", aliceItems()[0])
	}
}

func TestInboxClient_SchemeDialer(t *testing.T) {
	ts := startInboxServer(t)

	bobCb, bobItems := collectItems()
	bob := client.NewInbox(bobCb,
		client.WithInboxDialerForScheme("sse", client.SSEDialer()),
	)
	defer bob.Close()

	// sse:// scheme gets rewritten to http:// and uses SSEDialer.
	sseURL := "sse" + ts.URL[4:]
	if err := bob.Connect("server", sseURL, authHeader("token-bob")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Alice sends via WebSocket.
	aliceWS := dialRawWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{App: "chat", Key: "scheme1", Value: "val", ID: "scheme-1"}
	sendRawSync(t, aliceWS, []storesync.SyncStoreItem{item})

	waitFor(t, func() bool { return len(bobItems()) >= 1 })
	if bobItems()[0].Key != "scheme1" {
		t.Fatalf("unexpected: %+v", bobItems()[0])
	}
}

func TestInboxClient_OnConnectHook(t *testing.T) {
	ts := startInboxServer(t)

	var connected bool
	ic := client.NewInbox(
		func(string, []storesync.SyncStoreItem) {},
		client.OnInboxConnect(func(peerID string) error {
			connected = true
			return nil
		}),
	)
	defer ic.Close()

	if err := ic.Connect("server", inboxWSURL(ts), authHeader("token-alice")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	if !connected {
		t.Fatal("onConnect hook was not called")
	}
}

func TestInboxClient_Close(t *testing.T) {
	ts := startInboxServer(t)

	ic := client.NewInbox(func(string, []storesync.SyncStoreItem) {})
	if err := ic.Connect("server", inboxWSURL(ts), authHeader("token-alice")); err != nil {
		t.Fatal(err)
	}

	if err := ic.Close(); err != nil {
		t.Fatal(err)
	}
	if !ic.Closed() {
		t.Fatal("expected Closed() to return true")
	}
	// Double close should not panic.
	ic.Close()
}

// --- helpers ---

func dialRawWS(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func sendRawSync(t *testing.T, ws *websocket.Conn, items []storesync.SyncStoreItem) client.Message {
	t.Helper()
	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: items},
	}
	if err := ws.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp client.Message
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
