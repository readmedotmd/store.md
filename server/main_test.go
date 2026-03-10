package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/bbolt"
	"github.com/readmedotmd/store.md/client"
	storesync "github.com/readmedotmd/store.md/sync"
)

func newTestServer(t *testing.T) (*Server, *storesync.StoreSync) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := bbolt.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create bbolt store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ss := storesync.New(store, int64(100*time.Millisecond))

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

func TestSyncRequest(t *testing.T) {
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

	req := client.Message{Type: "sync_request"}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if resp.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", resp.Type)
	}
	if resp.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(resp.Payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Payload.Items))
	}
}

func TestSyncPush(t *testing.T) {
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
		LastSyncTimestamp: time.Now().UnixNano(),
	}

	msg := client.Message{Type: "sync_push", Payload: &payload}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if resp.Type != "sync_ack" {
		t.Fatalf("expected sync_ack, got %q", resp.Type)
	}

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
		LastSyncTimestamp: time.Now().UnixNano(),
	}

	pushMsg := client.Message{Type: "sync_push", Payload: &payload}
	if err := conn1.WriteJSON(pushMsg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var ack client.Message
	if err := conn1.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if ack.Type != "sync_ack" {
		t.Fatalf("expected sync_ack, got %q", ack.Type)
	}

	item, err := ss.GetItem("shared-key")
	if err != nil {
		t.Fatalf("Get shared-key failed: %v", err)
	}
	if item.Value != "from-peer1" {
		t.Fatalf("expected %q, got %q", "from-peer1", item.Value)
	}

	// Peer2 requests data
	conn2 := dialWS(t, wsURL(ts), "token-peer2")

	reqMsg := client.Message{Type: "sync_request"}
	if err := conn2.WriteJSON(reqMsg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var resp client.Message
	if err := conn2.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if resp.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", resp.Type)
	}
	if resp.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Value != "from-peer1" {
		t.Fatalf("expected %q, got %q", "from-peer1", resp.Payload.Items[0].Value)
	}
}

// --- Multi-store tests ---

func newTestSyncStore(t *testing.T) *storesync.StoreSync {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := bbolt.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create bbolt store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return storesync.New(store, int64(100*time.Millisecond))
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

	connA := dialWS(t, wsURL(ts)+"/room-a", "token-peer1")
	if err := connA.WriteJSON(client.Message{Type: "sync_request"}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var respA client.Message
	if err := connA.ReadJSON(&respA); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if respA.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", respA.Type)
	}
	if len(respA.Payload.Items) != 1 {
		t.Fatalf("expected 1 item in room-a, got %d", len(respA.Payload.Items))
	}
	if respA.Payload.Items[0].Value != "room-a-val" {
		t.Fatalf("expected %q, got %q", "room-a-val", respA.Payload.Items[0].Value)
	}

	connB := dialWS(t, wsURL(ts)+"/room-b", "token-peer1")
	if err := connB.WriteJSON(client.Message{Type: "sync_request"}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var respB client.Message
	if err := connB.ReadJSON(&respB); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if respB.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", respB.Type)
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
		LastSyncTimestamp: time.Now().UnixNano(),
	}
	if err := conn.WriteJSON(client.Message{Type: "sync_push", Payload: &payload}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	var ack client.Message
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if ack.Type != "sync_ack" {
		t.Fatalf("expected sync_ack, got %q", ack.Type)
	}

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
		LastSyncTimestamp: time.Now().UnixNano(),
	}
	if err := conn1.WriteJSON(client.Message{Type: "sync_push", Payload: &payload}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var ack client.Message
	if err := conn1.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if ack.Type != "sync_ack" {
		t.Fatalf("expected sync_ack, got %q", ack.Type)
	}

	// Peer2 reads from same room-a
	conn2 := dialWS(t, wsURL(ts)+"/room-a", "token-peer2")
	if err := conn2.WriteJSON(client.Message{Type: "sync_request"}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var resp client.Message
	if err := conn2.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if resp.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", resp.Type)
	}
	if len(resp.Payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Payload.Items))
	}
	if resp.Payload.Items[0].Value != "from-peer1" {
		t.Fatalf("expected %q, got %q", "from-peer1", resp.Payload.Items[0].Value)
	}
}

func TestUnknownMessageType(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dialWS(t, wsURL(ts), "token-peer1")

	msg := client.Message{Type: "bogus"}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	// The adapter logs unknown types but doesn't send an error response.
	// Send a sync_request to verify the connection is still alive.
	if err := conn.WriteJSON(client.Message{Type: "sync_request"}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if resp.Type != "sync_response" {
		t.Fatalf("expected sync_response, got %q", resp.Type)
	}
}
