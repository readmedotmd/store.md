package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

func newTestInbox(t *testing.T) (*InboxServer, *httptest.Server) {
	t.Helper()
	store := memory.New()
	tokens := map[string]string{
		"token-alice": "alice",
		"token-bob":   "bob",
		"token-carol": "carol",
	}
	inbox := NewInbox(store, TokenAuth(tokens))
	inbox.SetAllowedOrigins([]string{"*"})
	inbox.EnableHTTP()

	ts := httptest.NewServer(inbox)
	t.Cleanup(ts.Close)
	return inbox, ts
}

func dialInboxWS(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
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

func sendSync(t *testing.T, ws *websocket.Conn, items []storesync.SyncStoreItem) client.Message {
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

func inboxHTTPSync(t *testing.T, ts *httptest.Server, token string, items []storesync.SyncStoreItem) client.Message {
	t.Helper()
	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: items},
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var respMsg client.Message
	if err := json.NewDecoder(resp.Body).Decode(&respMsg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return respMsg
}

// --- Basic WebSocket tests ---

func TestInbox_BasicStoreAndForward(t *testing.T) {
	_, ts := newTestInbox(t)

	// Alice connects and sends an item.
	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App:   "notes",
		Key:   "note-1",
		Value: "hello from alice",
		ID:    "item-1",
	}
	resp := sendSync(t, alice, []storesync.SyncStoreItem{item})
	// Alice should not get her own item back.
	if len(resp.Payload.Items) != 0 {
		t.Fatalf("expected 0 items echoed back, got %d", len(resp.Payload.Items))
	}

	// Bob connects and should receive Alice's item.
	bob := dialInboxWS(t, ts, "token-bob")
	// Bob sends empty sync to get pending items.
	resp = sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item for bob, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Key != "note-1" {
		t.Fatalf("expected key 'note-1', got %q", resp.Payload.Items[0].Key)
	}
	if resp.Payload.Items[0].Value != "hello from alice" {
		t.Fatalf("expected value 'hello from alice', got %q", resp.Payload.Items[0].Value)
	}
}

func TestInbox_PushToConnectedPeer(t *testing.T) {
	_, ts := newTestInbox(t)

	// Bob connects first.
	bob := dialInboxWS(t, ts, "token-bob")

	// Alice connects and sends an item.
	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App:   "notes",
		Key:   "note-1",
		Value: "pushed item",
		ID:    "push-1",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{item})

	// Bob should receive the item via push notification.
	bob.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg client.Message
	if err := bob.ReadJSON(&msg); err != nil {
		t.Fatalf("bob read push: %v", err)
	}
	if msg.Type != "sync" {
		t.Fatalf("expected sync message, got %q", msg.Type)
	}
	if len(msg.Payload.Items) != 1 {
		t.Fatalf("expected 1 pushed item, got %d", len(msg.Payload.Items))
	}
	if msg.Payload.Items[0].Key != "note-1" {
		t.Fatalf("expected key 'note-1', got %q", msg.Payload.Items[0].Key)
	}
}

func TestInbox_CursorAdvance(t *testing.T) {
	_, ts := newTestInbox(t)

	alice := dialInboxWS(t, ts, "token-alice")

	// Alice sends two items.
	items := []storesync.SyncStoreItem{
		{App: "notes", Key: "note-1", Value: "first", ID: "id-1"},
		{App: "notes", Key: "note-2", Value: "second", ID: "id-2"},
	}
	sendSync(t, alice, items)

	// Bob connects and gets both items.
	bob := dialInboxWS(t, ts, "token-bob")
	resp := sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Payload.Items))
	}

	// Bob syncs again — should get no new items (cursor advanced).
	resp = sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 0 {
		t.Fatalf("expected 0 items after cursor advance, got %d", len(resp.Payload.Items))
	}
}

func TestInbox_MultipleRecipients(t *testing.T) {
	_, ts := newTestInbox(t)

	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App: "notes", Key: "shared", Value: "for everyone", ID: "shared-1",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{item})

	// Both Bob and Carol should receive the item.
	bob := dialInboxWS(t, ts, "token-bob")
	resp := sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("bob expected 1 item, got %d", len(resp.Payload.Items))
	}

	carol := dialInboxWS(t, ts, "token-carol")
	resp = sendSync(t, carol, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("carol expected 1 item, got %d", len(resp.Payload.Items))
	}
}

func TestInbox_BidirectionalExchange(t *testing.T) {
	_, ts := newTestInbox(t)

	// Alice sends an item.
	alice := dialInboxWS(t, ts, "token-alice")
	aliceItem := storesync.SyncStoreItem{
		App: "notes", Key: "from-alice", Value: "alice data", ID: "alice-1",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{aliceItem})

	// Bob sends an item and should receive Alice's item in the response.
	bob := dialInboxWS(t, ts, "token-bob")
	bobItem := storesync.SyncStoreItem{
		App: "notes", Key: "from-bob", Value: "bob data", ID: "bob-1",
	}
	resp := sendSync(t, bob, []storesync.SyncStoreItem{bobItem})
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item for bob, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Key != "from-alice" {
		t.Fatalf("expected from-alice, got %q", resp.Payload.Items[0].Key)
	}

	// Alice syncs and should receive Bob's item.
	resp = sendSync(t, alice, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item for alice, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Key != "from-bob" {
		t.Fatalf("expected from-bob, got %q", resp.Payload.Items[0].Key)
	}
}

func TestInbox_OpaquePayload(t *testing.T) {
	// Verify items are stored exactly as received — no modification.
	_, ts := newTestInbox(t)

	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App:       "encrypted",
		Key:       "secret-key",
		Value:     "base64encodedencrypteddata==",
		ID:        "enc-1",
		Timestamp: 1234567890,
		PublicKey: "pk_alice_xyz",
		Signature: "sig_abc123",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{item})

	bob := dialInboxWS(t, ts, "token-bob")
	resp := sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}

	got := resp.Payload.Items[0]
	if got.App != item.App || got.Key != item.Key || got.Value != item.Value ||
		got.ID != item.ID || got.Timestamp != item.Timestamp ||
		got.PublicKey != item.PublicKey || got.Signature != item.Signature {
		t.Fatalf("item was modified during relay:\n  sent: %+v\n  got:  %+v", item, got)
	}
}

// --- HTTP POST tests ---

func TestInbox_HTTPPost(t *testing.T) {
	_, ts := newTestInbox(t)

	// Alice sends via HTTP POST.
	item := storesync.SyncStoreItem{
		App: "notes", Key: "http-note", Value: "via http", ID: "http-1",
	}
	inboxHTTPSync(t, ts, "token-alice", []storesync.SyncStoreItem{item})

	// Bob retrieves via HTTP POST.
	resp := inboxHTTPSync(t, ts, "token-bob", nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Key != "http-note" {
		t.Fatalf("expected key 'http-note', got %q", resp.Payload.Items[0].Key)
	}
}

func TestInbox_HTTPPost_CursorAdvance(t *testing.T) {
	_, ts := newTestInbox(t)

	// Alice sends via HTTP.
	item := storesync.SyncStoreItem{
		App: "notes", Key: "cursor-test", Value: "data", ID: "ct-1",
	}
	inboxHTTPSync(t, ts, "token-alice", []storesync.SyncStoreItem{item})

	// Bob polls twice — second should return nothing.
	resp := inboxHTTPSync(t, ts, "token-bob", nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("first poll: expected 1 item, got %d", len(resp.Payload.Items))
	}
	resp = inboxHTTPSync(t, ts, "token-bob", nil)
	if len(resp.Payload.Items) != 0 {
		t.Fatalf("second poll: expected 0 items, got %d", len(resp.Payload.Items))
	}
}

func TestInbox_MixedTransports(t *testing.T) {
	// Alice sends via WebSocket, Bob receives via HTTP POST.
	_, ts := newTestInbox(t)

	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App: "notes", Key: "mixed-1", Value: "from ws", ID: "mix-1",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{item})

	resp := inboxHTTPSync(t, ts, "token-bob", nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}

	// Bob sends via HTTP POST, Alice receives via WebSocket.
	bobItem := storesync.SyncStoreItem{
		App: "notes", Key: "mixed-2", Value: "from http", ID: "mix-2",
	}
	inboxHTTPSync(t, ts, "token-bob", []storesync.SyncStoreItem{bobItem})

	// Alice should get Bob's item pushed via WS.
	alice.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg client.Message
	if err := alice.ReadJSON(&msg); err != nil {
		t.Fatalf("alice read push: %v", err)
	}
	if len(msg.Payload.Items) != 1 || msg.Payload.Items[0].Key != "mixed-2" {
		t.Fatalf("expected mixed-2 push, got %+v", msg.Payload)
	}
}

// --- SSE tests ---

func TestInbox_SSE_StreamAndForward(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice", "token-bob": "bob"}
	inbox := NewInbox(store, TokenAuth(tokens))
	inbox.SetAllowedOrigins([]string{"*"})
	inbox.EnableSSE()
	inbox.EnableHTTP()

	mux := http.NewServeMux()
	mux.Handle("/sse", inbox.Handler(inbox.SSE()))
	mux.Handle("/http", inbox.Handler(inbox.HTTP()))
	mux.Handle("/ws", inbox.Handler(inbox.WebSocket()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Alice sends an item via HTTP POST.
	item := storesync.SyncStoreItem{
		App: "notes", Key: "sse-test", Value: "via sse", ID: "sse-1",
	}
	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: []storesync.SyncStoreItem{item}},
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	// Bob connects via SSE and should receive Alice's item as a push.
	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	sseReq.Header.Set("Accept", "text/event-stream")
	sseReq.Header.Set("Authorization", "Bearer token-bob")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", sseResp.StatusCode)
	}

	// Read the SSE event.
	scanner := bufio.NewScanner(sseResp.Body)
	var dataLine string
	deadline := time.After(2 * time.Second)
	dataCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				dataCh <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
	}()

	select {
	case dataLine = <-dataCh:
	case <-deadline:
		t.Fatal("timeout waiting for SSE event")
	}

	var sseMsg client.Message
	if err := json.Unmarshal([]byte(dataLine), &sseMsg); err != nil {
		t.Fatalf("unmarshal SSE data: %v", err)
	}
	if len(sseMsg.Payload.Items) != 1 {
		t.Fatalf("expected 1 item in SSE event, got %d", len(sseMsg.Payload.Items))
	}
	if sseMsg.Payload.Items[0].Key != "sse-test" {
		t.Fatalf("expected key 'sse-test', got %q", sseMsg.Payload.Items[0].Key)
	}
}

func TestInbox_SSE_PostToSession(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice", "token-bob": "bob"}
	inbox := NewInbox(store, TokenAuth(tokens))
	inbox.SetAllowedOrigins([]string{"*"})
	inbox.EnableSSE()
	inbox.EnableHTTP()

	ts := httptest.NewServer(inbox)
	t.Cleanup(ts.Close)

	// Alice opens an SSE stream.
	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	sseReq.Header.Set("Accept", "text/event-stream")
	sseReq.Header.Set("Authorization", "Bearer token-alice")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseResp.Body.Close()

	// Bob sends via HTTP POST.
	item := storesync.SyncStoreItem{
		App: "notes", Key: "sse-post", Value: "data", ID: "sp-1",
	}
	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: []storesync.SyncStoreItem{item}},
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-bob")
	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	postResp.Body.Close()

	// Alice should receive via SSE stream.
	scanner := bufio.NewScanner(sseResp.Body)
	dataCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				dataCh <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
	}()

	select {
	case dataLine := <-dataCh:
		var sseMsg client.Message
		if err := json.Unmarshal([]byte(dataLine), &sseMsg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(sseMsg.Payload.Items) != 1 || sseMsg.Payload.Items[0].Key != "sse-post" {
			t.Fatalf("unexpected SSE data: %+v", sseMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for SSE push")
	}
}

// --- Route-based dispatch tests ---

func TestInbox_RouteBasedDispatch(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice", "token-bob": "bob"}
	inbox := NewInbox(store, TokenAuth(tokens))
	inbox.SetAllowedOrigins([]string{"*"})

	mux := http.NewServeMux()
	mux.Handle("/ws", inbox.Handler(inbox.WebSocket()))
	mux.Handle("/http", inbox.Handler(inbox.HTTP()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Alice sends via WebSocket on /ws route.
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer token-alice")
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	item := storesync.SyncStoreItem{
		App: "notes", Key: "route-test", Value: "routed", ID: "rt-1",
	}
	msg := client.Message{
		Type:    "sync",
		Payload: &storesync.SyncPayload{Items: []storesync.SyncStoreItem{item}},
	}
	if err := ws.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp client.Message
	if err := ws.ReadJSON(&resp); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Bob retrieves via HTTP on /http route.
	pollMsg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	body, _ := json.Marshal(pollMsg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-bob")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer httpResp.Body.Close()

	var httpMsg client.Message
	json.NewDecoder(httpResp.Body).Decode(&httpMsg)
	if len(httpMsg.Payload.Items) != 1 {
		t.Fatalf("expected 1 item on /http route, got %d", len(httpMsg.Payload.Items))
	}
	if httpMsg.Payload.Items[0].Key != "route-test" {
		t.Fatalf("expected key 'route-test', got %q", httpMsg.Payload.Items[0].Key)
	}
}

func TestInbox_RouteBasedDispatch_AuthRequired(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice"}
	inbox := NewInbox(store, TokenAuth(tokens))

	mux := http.NewServeMux()
	mux.Handle("/http", inbox.Handler(inbox.HTTP()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Request without auth should fail.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Auth and limits tests ---

func TestInbox_AuthFailure(t *testing.T) {
	_, ts := newTestInbox(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer bad-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestInbox_AuthFailure_WebSocket(t *testing.T) {
	_, ts := newTestInbox(t)

	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer bad-token")
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		// Connection may succeed but should get close frame with 4401.
		_, _, readErr := ws.ReadMessage()
		ws.Close()
		if readErr == nil {
			t.Fatal("expected close after auth failure")
		}
	}
}

func TestInbox_ConnectionLimit(t *testing.T) {
	inbox, ts := newTestInbox(t)
	inbox.SetMaxConnsPerPeer(1)

	// First connection should succeed.
	ws1 := dialInboxWS(t, ts, "token-alice")
	_ = ws1

	// Second connection should fail.
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer token-alice")
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		t.Fatal("expected dial to fail due to connection limit")
	}
	if resp != nil && resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
}

func TestInbox_MaxTotalConns(t *testing.T) {
	inbox, ts := newTestInbox(t)
	inbox.SetMaxTotalConns(1)

	// First connection should succeed.
	ws1 := dialInboxWS(t, ts, "token-alice")
	_ = ws1

	// Second connection (different peer) should fail.
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer token-bob")
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		t.Fatal("expected dial to fail due to total connection limit")
	}
	if resp != nil && resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
}

// --- Hook tests ---

func TestInbox_OnPeerConnectHook(t *testing.T) {
	inbox, ts := newTestInbox(t)

	var connected []string
	inbox.OnPeerConnect(func(peerID string, r *http.Request) error {
		connected = append(connected, peerID)
		return nil
	})

	ws := dialInboxWS(t, ts, "token-alice")
	_ = ws
	time.Sleep(50 * time.Millisecond)

	if len(connected) != 1 || connected[0] != "alice" {
		t.Fatalf("expected [alice], got %v", connected)
	}
}

func TestInbox_OnPeerConnectHook_Reject(t *testing.T) {
	inbox, ts := newTestInbox(t)

	inbox.OnPeerConnect(func(peerID string, r *http.Request) error {
		if peerID == "alice" {
			return fmt.Errorf("banned")
		}
		return nil
	})

	// Alice should be rejected.
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer token-alice")
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		t.Fatal("expected dial to fail due to hook rejection")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}

	// Bob should succeed.
	bob := dialInboxWS(t, ts, "token-bob")
	_ = bob
}

func TestInbox_OnPeerDisconnectHook(t *testing.T) {
	inbox, ts := newTestInbox(t)

	disconnected := make(chan string, 1)
	inbox.OnPeerDisconnect(func(peerID string) {
		disconnected <- peerID
	})

	ws := dialInboxWS(t, ts, "token-alice")
	time.Sleep(50 * time.Millisecond)
	ws.Close()

	select {
	case peerID := <-disconnected:
		if peerID != "alice" {
			t.Fatalf("expected alice, got %q", peerID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for disconnect hook")
	}
}

// --- Cleanup tests ---

func TestInbox_Cleanup(t *testing.T) {
	inbox, ts := newTestInbox(t)

	// Alice sends an item.
	alice := dialInboxWS(t, ts, "token-alice")
	item := storesync.SyncStoreItem{
		App: "notes", Key: "cleanup-test", Value: "data", ID: "cleanup-1",
	}
	sendSync(t, alice, []storesync.SyncStoreItem{item})

	// Bob reads it (advances his cursor past it).
	bob := dialInboxWS(t, ts, "token-bob")
	resp := sendSync(t, bob, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}

	// Carol reads it too.
	carol := dialInboxWS(t, ts, "token-carol")
	resp = sendSync(t, carol, nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}

	// Run cleanup — all peers have read past this item.
	ctx := context.Background()
	if err := inbox.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Verify the item is gone from the store.
	items, err := inbox.store.List(ctx, storemd.ListArgs{Prefix: inboxMsgPrefix})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items after cleanup, got %d", len(items))
	}
}

func TestInbox_Cleanup_PartialDelivery(t *testing.T) {
	inbox, ts := newTestInbox(t)

	// Carol syncs via HTTP (stateless — no push notifications) to establish a cursor.
	inboxHTTPSync(t, ts, "token-carol", nil)

	// Alice sends an item via HTTP AFTER Carol's cursor was established.
	item := storesync.SyncStoreItem{
		App: "notes", Key: "partial", Value: "data", ID: "partial-1",
	}
	inboxHTTPSync(t, ts, "token-alice", []storesync.SyncStoreItem{item})

	// Bob reads it via HTTP (advances his cursor past it).
	resp := inboxHTTPSync(t, ts, "token-bob", nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected bob to receive 1 item, got %d", len(resp.Payload.Items))
	}

	// Cleanup should NOT delete the item because Carol's cursor is
	// still before it (she synced before the item was sent).
	ctx := context.Background()
	inbox.Cleanup(ctx)

	items, _ := inbox.store.List(ctx, storemd.ListArgs{Prefix: inboxMsgPrefix})
	if len(items) != 1 {
		t.Fatalf("expected 1 item (Carol hasn't read it), got %d", len(items))
	}

	// Carol reads the item via HTTP.
	resp = inboxHTTPSync(t, ts, "token-carol", nil)
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected carol to receive 1 item, got %d", len(resp.Payload.Items))
	}

	// Now cleanup should remove it — all peers have read past it.
	inbox.Cleanup(ctx)

	items, _ = inbox.store.List(ctx, storemd.ListArgs{Prefix: inboxMsgPrefix})
	if len(items) != 0 {
		t.Fatalf("expected 0 items after all peers read, got %d", len(items))
	}
}

// --- PeerHandler interface tests ---

func TestInbox_ImplementsPeerHandler(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice"}
	inbox := NewInbox(store, TokenAuth(tokens))

	// Verify InboxServer satisfies PeerHandler at compile time.
	var _ PeerHandler = inbox
}

func TestInbox_HandleMessage_AckSemantics(t *testing.T) {
	store := memory.New()
	tokens := map[string]string{"token-alice": "alice", "token-bob": "bob"}
	inbox := NewInbox(store, TokenAuth(tokens))

	ctx := context.Background()

	// Alice sends an item.
	msg := client.Message{
		Type: "sync",
		Payload: &storesync.SyncPayload{Items: []storesync.SyncStoreItem{
			{App: "notes", Key: "ack-test", Value: "data", ID: "ack-1"},
		}},
	}
	inbox.HandleMessage(ctx, "alice", msg)

	// Bob reads without ack — should get the item.
	resp, ack, err := inbox.HandleMessage(ctx, "bob", client.Message{
		Type: "sync", Payload: &storesync.SyncPayload{},
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}

	// Without calling ack, Bob should get the same item again.
	resp2, _, _ := inbox.HandleMessage(ctx, "bob", client.Message{
		Type: "sync", Payload: &storesync.SyncPayload{},
	})
	if len(resp2.Payload.Items) != 1 {
		t.Fatalf("expected 1 item without ack, got %d", len(resp2.Payload.Items))
	}

	// After calling ack, Bob should get nothing.
	ack()
	resp3, _, _ := inbox.HandleMessage(ctx, "bob", client.Message{
		Type: "sync", Payload: &storesync.SyncPayload{},
	})
	if len(resp3.Payload.Items) != 0 {
		t.Fatalf("expected 0 items after ack, got %d", len(resp3.Payload.Items))
	}
}
