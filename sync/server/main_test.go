package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

func newTestServer(t *testing.T) (*Server, *storesync.StoreSync) {
	t.Helper()
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))

	tokens := map[string]string{
		"token-peer1": "peer1",
		"token-peer2": "peer2",
	}

	srv := New(ss, TokenAuth(tokens))
	return srv, ss
}

func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func dialWS(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestAuth_InvalidToken(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer bad-token")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts), header)
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuth_ValidToken(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dialWS(t, wsURL(ts), "token-peer1")
	_ = conn
}

func TestSyncExchange(t *testing.T) {
	srv, ss := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if err := ss.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := ss.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	conn := dialWS(t, wsURL(ts), "token-peer1")

	// Send a sync message with no items (initiate). The server will call
	// Sync(peerID, payload) and respond with its queued items.
	req := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if resp.Type != "sync" {
		t.Fatalf("expected sync, got %q", resp.Type)
	}
	if resp.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(resp.Payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Payload.Items))
	}
}

func TestSyncPushItems(t *testing.T) {
	srv, ss := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dialWS(t, wsURL(ts), "token-peer1")

	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "pushed-key",
				Value:     "pushed-val",
				Timestamp: time.Now().UnixNano(),
				ID:        "push-id-1",
			},
		},
	}

	msg := client.Message{Type: "sync", Payload: &payload}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	// The server processes the sync and may respond with its own items.
	// For queue-based, Sync(peerID, incoming) applies items then SyncOuts.
	// Read response - it may have items or be a sync with nil payload.
	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		// If server has nothing to send back, connection may just be idle.
		// But with queue-based sync, SyncOut returns the items we just pushed
		// (they're in the queue with future writeTimestamp, so they won't be returned).
		// So we might not get a response. Let's verify the item was stored.
	}

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	item, err := ss.GetItem("pushed-key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "pushed-val" {
		t.Fatalf("expected %q, got %q", "pushed-val", item.Value)
	}
}

func TestSyncRoundTrip(t *testing.T) {
	srv, ss := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Peer1 pushes data
	conn1 := dialWS(t, wsURL(ts), "token-peer1")

	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "shared-key",
				Value:     "from-peer1",
				Timestamp: time.Now().UnixNano(),
				ID:        "roundtrip-id-1",
			},
		},
	}

	pushMsg := client.Message{Type: "sync", Payload: &payload}
	if err := conn1.WriteJSON(pushMsg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	// Wait for server to process
	time.Sleep(200 * time.Millisecond)

	item, err := ss.GetItem("shared-key")
	if err != nil {
		t.Fatalf("Get shared-key failed: %v", err)
	}
	if item.Value != "from-peer1" {
		t.Fatalf("expected %q, got %q", "from-peer1", item.Value)
	}

	// Peer2 initiates sync to get data
	conn2 := dialWS(t, wsURL(ts), "token-peer2")

	reqMsg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	if err := conn2.WriteJSON(reqMsg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var resp client.Message
	if err := conn2.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if resp.Type != "sync" {
		t.Fatalf("expected sync, got %q", resp.Type)
	}
	if resp.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(resp.Payload.Items) < 1 {
		t.Fatalf("expected at least 1 item, got %d", len(resp.Payload.Items))
	}

	found := false
	for _, item := range resp.Payload.Items {
		if item.Key == "shared-key" && item.Value == "from-peer1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find shared-key in sync response")
	}
}

// --- Multi-store tests ---

func newTestSyncStore(t *testing.T) *storesync.StoreSync {
	t.Helper()
	return storesync.New(memory.New(), int64(100*time.Millisecond))
}

func newMultiTestServer(t *testing.T) (*Server, map[string]*storesync.StoreSync) {
	t.Helper()
	stores := map[string]*storesync.StoreSync{
		"room-a": newTestSyncStore(t),
		"room-b": newTestSyncStore(t),
	}

	resolver := func(storeID string) (storesync.SyncStore, error) {
		ss, ok := stores[storeID]
		if !ok {
			return nil, fmt.Errorf("unknown store %q", storeID)
		}
		return ss, nil
	}

	tokens := map[string]string{
		"token-peer1": "peer1",
		"token-peer2": "peer2",
	}

	srv := NewMulti(resolver, TokenAuth(tokens))
	return srv, stores
}

func TestMulti_IsolatedStores(t *testing.T) {
	srv, stores := newMultiTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if err := stores["room-a"].SetItem("app", "key1", "room-a-val"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if err := stores["room-b"].SetItem("app", "key1", "room-b-val"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	// Sync from room-a
	connA := dialWS(t, wsURL(ts)+"/room-a", "token-peer1")
	if err := connA.WriteJSON(client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var respA client.Message
	if err := connA.ReadJSON(&respA); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if respA.Type != "sync" {
		t.Fatalf("expected sync, got %q", respA.Type)
	}
	if len(respA.Payload.Items) != 1 {
		t.Fatalf("expected 1 item in room-a, got %d", len(respA.Payload.Items))
	}
	if respA.Payload.Items[0].Value != "room-a-val" {
		t.Fatalf("expected %q, got %q", "room-a-val", respA.Payload.Items[0].Value)
	}

	// Sync from room-b
	connB := dialWS(t, wsURL(ts)+"/room-b", "token-peer1")
	if err := connB.WriteJSON(client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var respB client.Message
	if err := connB.ReadJSON(&respB); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if respB.Type != "sync" {
		t.Fatalf("expected sync, got %q", respB.Type)
	}
	if len(respB.Payload.Items) != 1 {
		t.Fatalf("expected 1 item in room-b, got %d", len(respB.Payload.Items))
	}
	if respB.Payload.Items[0].Value != "room-b-val" {
		t.Fatalf("expected %q, got %q", "room-b-val", respB.Payload.Items[0].Value)
	}
}

func TestMulti_PushToSpecificStore(t *testing.T) {
	srv, stores := newMultiTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dialWS(t, wsURL(ts)+"/room-a", "token-peer1")

	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "pushed",
				Value:     "to-room-a",
				Timestamp: time.Now().UnixNano(),
				ID:        "multi-push-1",
			},
		},
	}
	if err := conn.WriteJSON(client.Message{Type: "sync", Payload: &payload}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	item, err := stores["room-a"].GetItem("pushed")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "to-room-a" {
		t.Fatalf("expected %q, got %q", "to-room-a", item.Value)
	}

	_, err = stores["room-b"].GetItem("pushed")
	if err == nil {
		t.Fatal("expected error for missing key in room-b, got nil")
	}
}

func TestMulti_UnknownStoreID(t *testing.T) {
	srv, _ := newMultiTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer token-peer1")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts)+"/nonexistent", header)
	if err == nil {
		t.Fatal("expected error for unknown store ID, got nil")
	}
	if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMulti_MissingStoreID(t *testing.T) {
	srv, _ := newMultiTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer token-peer1")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts)+"/", header)
	if err == nil {
		t.Fatal("expected error for missing store ID, got nil")
	}
	if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMulti_RoundTripBetweenPeers(t *testing.T) {
	srv, _ := newMultiTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Peer1 pushes to room-a
	conn1 := dialWS(t, wsURL(ts)+"/room-a", "token-peer1")
	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "shared",
				Value:     "from-peer1",
				Timestamp: time.Now().UnixNano(),
				ID:        "rt-multi-1",
			},
		},
	}
	if err := conn1.WriteJSON(client.Message{Type: "sync", Payload: &payload}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Peer2 reads from same room-a
	conn2 := dialWS(t, wsURL(ts)+"/room-a", "token-peer2")
	if err := conn2.WriteJSON(client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var resp client.Message
	if err := conn2.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if resp.Type != "sync" {
		t.Fatalf("expected sync, got %q", resp.Type)
	}
	if resp.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(resp.Payload.Items) < 1 {
		t.Fatalf("expected at least 1 item, got %d", len(resp.Payload.Items))
	}

	found := false
	for _, item := range resp.Payload.Items {
		if item.Key == "shared" && item.Value == "from-peer1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find shared key in sync response")
	}
}

func TestUnknownMessageType(t *testing.T) {
	srv, ss := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Add data so the server has something to respond with.
	ss.SetItem("app", "key1", "val1")

	conn := dialWS(t, wsURL(ts), "token-peer1")

	msg := client.Message{Type: "bogus"}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	// The adapter logs unknown types but doesn't send an error response.
	// Send a sync message to verify the connection is still alive.
	if err := conn.WriteJSON(client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if resp.Type != "sync" {
		t.Fatalf("expected sync, got %q", resp.Type)
	}
}
