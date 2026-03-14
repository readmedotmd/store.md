package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), header)
	if err != nil {
		t.Fatalf("expected successful upgrade, got error: %v", err)
	}
	defer conn.Close()

	// The server upgrades the WebSocket then closes with 4401 so that
	// browser clients can detect auth failures (HTTP status codes are
	// not visible to the browser WebSocket API on failed upgrades).
	_, _, readErr := conn.ReadMessage()
	if readErr == nil {
		t.Fatal("expected close error, got nil")
	}
	closeErr, ok := readErr.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected *websocket.CloseError, got %T: %v", readErr, readErr)
	}
	if closeErr.Code != CloseCodeAuthFailed {
		t.Fatalf("expected close code %d, got %d", CloseCodeAuthFailed, closeErr.Code)
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

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := ss.SetItem(ctx, "app", "key2", "val2"); err != nil {
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

	// The server applies the items via SyncIn. It does NOT echo them back
	// (Sync filters out items the sender just sent). Give it time to process.
	time.Sleep(200 * time.Millisecond)

	item, err := ss.GetItem(context.Background(), "pushed-key")
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

	item, err := ss.GetItem(context.Background(), "shared-key")
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

	ctx := context.Background()
	if err := stores["room-a"].SetItem(ctx, "app", "key1", "room-a-val"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if err := stores["room-b"].SetItem(ctx, "app", "key1", "room-b-val"); err != nil {
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

	item, err := stores["room-a"].GetItem(context.Background(), "pushed")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "to-room-a" {
		t.Fatalf("expected %q, got %q", "to-room-a", item.Value)
	}

	_, err = stores["room-b"].GetItem(context.Background(), "pushed")
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
	ss.SetItem(context.Background(), "app", "key1", "val1")

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

// --- HTTP transport tests ---

func newHTTPTestServer(t *testing.T) (*Server, *storesync.StoreSync) {
	t.Helper()
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
		"token-peer2": "peer2",
	}
	srv := New(ss, TokenAuth(tokens))
	srv.EnableHTTP()
	return srv, ss
}

func httpPost(t *testing.T, url, token string, msg client.Message) *http.Response {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP POST failed: %v", err)
	}
	return resp
}

func TestHTTP_SyncExchange(t *testing.T) {
	srv, ss := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if err := ss.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	resp := httpPost(t, ts.URL, "token-peer1", msg)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var respMsg client.Message
	if err := json.NewDecoder(resp.Body).Decode(&respMsg); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if respMsg.Type != "sync" {
		t.Fatalf("expected sync, got %q", respMsg.Type)
	}
	if respMsg.Payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if len(respMsg.Payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(respMsg.Payload.Items))
	}
}

func TestHTTP_PushItems(t *testing.T) {
	srv, ss := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "http-pushed",
				Value:     "http-val",
				Timestamp: time.Now().UnixNano(),
				ID:        "http-push-1",
			},
		},
	}

	msg := client.Message{Type: "sync", Payload: &payload}
	resp := httpPost(t, ts.URL, "token-peer1", msg)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify item landed in the server store.
	time.Sleep(100 * time.Millisecond)

	item, err := ss.GetItem(context.Background(), "http-pushed")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "http-val" {
		t.Fatalf("expected %q, got %q", "http-val", item.Value)
	}
}

func TestHTTP_InvalidMethod(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
		"token-peer2": "peer2",
	}
	// Create a server with only HTTP transport (no WebSocket).
	srv := &Server{
		store:         ss,
		auth:          TokenAuth(tokens),
		logger:        slog.Default(),
		transports:    []Transport{&HTTPTransport{}},
		maxConnsPerPeer: 10,
		maxTotalConns:   1000,
		peerConns:      make(map[string]int),
		authFailCount:  make(map[string]*authFailEntry),
		adapters:       make(map[storesync.SyncStore]*client.Client),
	}
	srv.adapters[ss] = client.New(ss)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-peer1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestHTTP_InvalidMessageType(t *testing.T) {
	srv, _ := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := client.Message{Type: "bogus"}
	resp := httpPost(t, ts.URL, "token-peer1", msg)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestHTTP_AuthRequired(t *testing.T) {
	srv, _ := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	resp := httpPost(t, ts.URL, "", msg)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestHTTP_WebSocketAndHTTPCoexist(t *testing.T) {
	srv, ss := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "coexist-key", "coexist-val"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	// Verify WebSocket still works.
	conn := dialWS(t, wsURL(ts), "token-peer1")
	wsReq := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	if err := conn.WriteJSON(wsReq); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}
	var wsResp client.Message
	if err := conn.ReadJSON(&wsResp); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if wsResp.Type != "sync" {
		t.Fatalf("expected sync from WS, got %q", wsResp.Type)
	}
	if wsResp.Payload == nil || len(wsResp.Payload.Items) < 1 {
		t.Fatal("expected at least 1 item from WS sync")
	}

	// Verify HTTP POST also works on the same server.
	httpMsg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	resp := httpPost(t, ts.URL, "token-peer2", httpMsg)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var httpResp client.Message
	if err := json.NewDecoder(resp.Body).Decode(&httpResp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if httpResp.Type != "sync" {
		t.Fatalf("expected sync from HTTP, got %q", httpResp.Type)
	}
	if httpResp.Payload == nil || len(httpResp.Payload.Items) < 1 {
		t.Fatal("expected at least 1 item from HTTP sync")
	}
}

func TestHTTP_BodySizeLimit(t *testing.T) {
	srv, _ := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Create a body larger than the 10MB limit.
	bigBody := make([]byte, 11<<20) // 11MB
	for i := range bigBody {
		bigBody[i] = 'a'
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(bigBody))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for oversized body, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestHTTP_MalformedJSON(t *testing.T) {
	srv, _ := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{invalid json"))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for malformed JSON, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestHTTP_MalformedPayload_HugeTimestamp(t *testing.T) {
	srv, ss := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Item with a timestamp far in the future (should be rejected by clock skew check).
	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "future-key",
				Value:     "val",
				Timestamp: time.Now().UnixNano() + int64(time.Hour),
				ID:        "future-id",
			},
		},
	}

	msg := client.Message{Type: "sync", Payload: &payload}
	resp := httpPost(t, ts.URL, "token-peer1", msg)
	defer resp.Body.Close()

	// The sync should either reject the item or return an error.
	// The item should NOT be persisted.
	time.Sleep(100 * time.Millisecond)
	_, err := ss.GetItem(context.Background(), "future-key")
	if err == nil {
		t.Fatal("item with huge future timestamp should not be persisted")
	}
}

func TestHTTP_PastTimestamp_Accepted(t *testing.T) {
	srv, ss := newHTTPTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Items with old timestamps are valid in offline-first sync (LWW resolves conflicts).
	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "ancient-key",
				Value:     "val",
				Timestamp: 1000000, // ~1970
				ID:        "ancient-id",
			},
		},
	}

	msg := client.Message{Type: "sync", Payload: &payload}
	resp := httpPost(t, ts.URL, "token-peer1", msg)
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	item, err := ss.GetItem(context.Background(), "ancient-key")
	if err != nil {
		t.Fatalf("old-timestamp item should be accepted, got: %v", err)
	}
	if item.Value != "val" {
		t.Fatalf("expected value 'val', got %q", item.Value)
	}
}

func TestServer_TrustedProxies(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetTrustedProxies([]string{"127.0.0.1"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// With a trusted proxy, the X-Forwarded-For header should be respected.
	// Make many bad auth attempts with different X-Forwarded-For IPs —
	// they should each get their own rate limit bucket.
	for i := range 15 {
		header := http.Header{}
		header.Set("Authorization", "Bearer bad-token")
		header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i))

		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
		// Each unique IP should get 401, not 429 (since each is a fresh IP).
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d got 429 despite unique X-Forwarded-For IP", i)
		}
	}
}

func TestServer_UntrustedProxy_IgnoresXFF(t *testing.T) {
	srv, _ := newTestServer(t)
	// Don't set any trusted proxies — X-Forwarded-For should be ignored.
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// All requests come from the same RemoteAddr (the test client).
	// After 10 failures, should get rate limited.
	for i := range 15 {
		header := http.Header{}
		header.Set("Authorization", "Bearer bad-token")
		header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i)) // should be ignored

		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}
	// After 10+ failures from same IP, should be rate limited.
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer bad-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after rate limit, got %d", resp.StatusCode)
	}
}
