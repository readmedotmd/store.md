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

	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

func newSSETestServer(t *testing.T) (*Server, *storesync.StoreSync) {
	t.Helper()
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
		"token-peer2": "peer2",
	}
	srv := New(ss, TokenAuth(tokens))
	srv.EnableSSE()
	return srv, ss
}

func TestSSE_StreamAndPost(t *testing.T) {
	srv, ss := newSSETestServer(t)
	ts := httptest.NewServer(srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	if err := ss.SetItem(context.Background(), "app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	// Open SSE stream with cancellable context.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}

	// Read the first SSE event in a goroutine to avoid blocking.
	type sseResult struct {
		msg client.Message
		err error
	}
	resultCh := make(chan sseResult, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var dataLines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			}
			if line == "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				var msg client.Message
				if err := json.Unmarshal([]byte(data), &msg); err != nil {
					resultCh <- sseResult{err: err}
					return
				}
				resultCh <- sseResult{msg: msg}
				return
			}
		}
		resultCh <- sseResult{err: fmt.Errorf("stream ended: %v", scanner.Err())}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("SSE read error: %v", res.err)
		}
		if res.msg.Type != "sync" {
			t.Fatalf("expected sync, got %q", res.msg.Type)
		}
		if res.msg.Payload == nil || len(res.msg.Payload.Items) < 1 {
			t.Fatal("expected at least 1 item in SSE sync event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestSSE_PostSyncMessage(t *testing.T) {
	srv, ss := newSSETestServer(t)
	ts := httptest.NewServer(srv)

	// Use a cancellable context for the SSE stream so we can shut it
	// down cleanly before ts.Close() — otherwise the long-lived SSE
	// connection prevents the httptest server from closing.
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	// Open SSE stream in background.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
		}
	}()

	// Wait for stream to establish.
	time.Sleep(300 * time.Millisecond)

	// POST a sync message with items.
	payload := storesync.SyncPayload{
		Items: []storesync.SyncStoreItem{
			{
				App:       "app",
				Key:       "sse-pushed",
				Value:     "sse-val",
				Timestamp: time.Now().UnixNano(),
				ID:        "sse-push-1",
			},
		},
	}
	msg := client.Message{Type: "sync", Payload: &payload}
	body, _ := json.Marshal(msg)
	postReq, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Authorization", "Bearer token-peer1")
	postReq.Header.Set("X-Transport", "sse")

	resp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	// Verify item landed in the store.
	time.Sleep(300 * time.Millisecond)

	item, err := ss.GetItem(context.Background(), "sse-pushed")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "sse-val" {
		t.Fatalf("expected %q, got %q", "sse-val", item.Value)
	}
}

func TestSSE_PostWithoutStream_FallsBackToHTTP(t *testing.T) {
	srv, ss := newSSETestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "fb-key", "fb-val"); err != nil {
		t.Fatal(err)
	}

	// POST without an active SSE stream — should fall back to stateless HTTP.
	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer2")
	req.Header.Set("X-Transport", "sse")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (HTTP fallback), got %d", resp.StatusCode)
	}

	var respMsg client.Message
	if err := json.NewDecoder(resp.Body).Decode(&respMsg); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if respMsg.Payload == nil || len(respMsg.Payload.Items) < 1 {
		t.Fatal("expected items in HTTP fallback response")
	}
}

// --- Handler() route-based dispatch tests ---

func TestHandler_RouteBasedDispatch(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
	}
	srv := New(ss, TokenAuth(tokens))

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "route-key", "route-val"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", srv.Handler(srv.WebSocket()))
	mux.Handle("/http", srv.Handler(srv.HTTP()))
	mux.Handle("/sse", srv.Handler(srv.SSE()))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Test HTTP transport on /http route.
	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var respMsg client.Message
	if err := json.NewDecoder(resp.Body).Decode(&respMsg); err != nil {
		t.Fatal(err)
	}
	if respMsg.Type != "sync" {
		t.Fatalf("expected sync, got %q", respMsg.Type)
	}
	if respMsg.Payload == nil || len(respMsg.Payload.Items) < 1 {
		t.Fatal("expected items in response")
	}
}

func TestHandler_SSERoute(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
	}
	srv := New(ss, TokenAuth(tokens))

	if err := ss.SetItem(context.Background(), "app", "sse-route-key", "sse-route-val"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/sse", srv.Handler(srv.SSE()))

	ts := httptest.NewServer(mux)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}

	dataCh := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				dataCh <- true
				return
			}
		}
		dataCh <- false
	}()

	select {
	case got := <-dataCh:
		if !got {
			t.Fatal("expected at least one SSE data line")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE data")
	}
}

func TestHandler_WebSocketRoute(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{
		"token-peer1": "peer1",
	}
	srv := New(ss, TokenAuth(tokens))

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "ws-route-key", "ws-route-val"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", srv.Handler(srv.WebSocket()))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// WebSocket on /ws route.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn := dialWS(t, wsURL, "token-peer1")

	reqMsg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	if err := conn.WriteJSON(reqMsg); err != nil {
		t.Fatal(err)
	}

	var resp client.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "sync" {
		t.Fatalf("expected sync, got %q", resp.Type)
	}
	if resp.Payload == nil || len(resp.Payload.Items) < 1 {
		t.Fatal("expected items in WS response")
	}
}

func TestHandler_AuthRequired(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{"token-peer1": "peer1"}
	srv := New(ss, TokenAuth(tokens))

	mux := http.NewServeMux()
	mux.Handle("/http", srv.Handler(srv.HTTP()))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No auth header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestSSE_MultiStore(t *testing.T) {
	store1 := storesync.New(memory.New(), int64(100*time.Millisecond))
	store2 := storesync.New(memory.New(), int64(100*time.Millisecond))

	ctx := context.Background()
	if err := store1.SetItem(ctx, "app", "s1-key", "s1-val"); err != nil {
		t.Fatal(err)
	}
	if err := store2.SetItem(ctx, "app", "s2-key", "s2-val"); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{"token-peer1": "peer1"}
	srv := NewMulti(func(storeID string) (storesync.SyncStore, error) {
		switch storeID {
		case "store1":
			return store1, nil
		case "store2":
			return store2, nil
		default:
			return nil, fmt.Errorf("unknown store: %s", storeID)
		}
	}, TokenAuth(tokens))
	srv.EnableSSE()

	ts := httptest.NewServer(srv)
	sseCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	// Open SSE stream to store1.
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/sync/store1", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Read SSE event — should contain store1 data.
	type sseResult struct {
		msg client.Message
		err error
	}
	resultCh := make(chan sseResult, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var dataLines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			}
			if line == "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				var msg client.Message
				if err := json.Unmarshal([]byte(data), &msg); err != nil {
					resultCh <- sseResult{err: err}
					return
				}
				resultCh <- sseResult{msg: msg}
				return
			}
		}
		resultCh <- sseResult{err: fmt.Errorf("stream ended: %v", scanner.Err())}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("SSE read error: %v", res.err)
		}
		if res.msg.Payload == nil || len(res.msg.Payload.Items) < 1 {
			t.Fatal("expected items in SSE event")
		}
		found := false
		for _, item := range res.msg.Payload.Items {
			if item.Key == "s1-key" && item.Value == "s1-val" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected s1-key=s1-val in SSE event from store1")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestSSE_OnPeerConnectHook(t *testing.T) {
	srv, _ := newSSETestServer(t)

	var connected []string
	srv.OnPeerConnect(func(peerID string, r *http.Request) error {
		connected = append(connected, peerID)
		return nil
	})

	ts := httptest.NewServer(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	// Open SSE stream — should trigger OnPeerConnect.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	if len(connected) != 1 || connected[0] != "peer1" {
		t.Fatalf("expected OnPeerConnect called once with peer1, got %v", connected)
	}
}

func TestSSE_OnPeerConnectHook_Reject(t *testing.T) {
	srv, _ := newSSETestServer(t)

	srv.OnPeerConnect(func(peerID string, r *http.Request) error {
		return fmt.Errorf("not allowed")
	})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestSSE_ConnectionLimitPerPeer(t *testing.T) {
	srv, ss := newSSETestServer(t)
	srv.SetMaxConnsPerPeer(1)

	if err := ss.SetItem(context.Background(), "app", "k", "v"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer func() {
		cancel1()
		time.Sleep(100 * time.Millisecond)
		ts.Close()
	}()

	// First SSE stream — should succeed.
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, ts.URL, nil)
	req1.Header.Set("Accept", "text/event-stream")
	req1.Header.Set("Authorization", "Bearer token-peer1")

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first stream: expected 200, got %d", resp1.StatusCode)
	}

	time.Sleep(200 * time.Millisecond)

	// Second SSE stream for same peer — should be rejected (429).
	req2, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req2.Header.Set("Accept", "text/event-stream")
	req2.Header.Set("Authorization", "Bearer token-peer1")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream: expected 429, got %d", resp2.StatusCode)
	}
}

func TestSSE_ConcurrentPeers(t *testing.T) {
	srv, ss := newSSETestServer(t)

	if err := ss.SetItem(context.Background(), "app", "shared-key", "shared-val"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	// Open two SSE streams for different peers sequentially to avoid
	// race conditions with adapter initialization.
	peers := []string{"token-peer1", "token-peer2"}

	for _, token := range peers {
		ctx, cancel := context.WithCancel(context.Background())

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("peer %s: GET failed: %v", token, err)
		}

		dataCh := make(chan bool, 1)
		go func() {
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data: ") {
					dataCh <- true
					return
				}
			}
			dataCh <- false
		}()

		select {
		case ok := <-dataCh:
			if !ok {
				cancel()
				t.Fatalf("peer %s did not receive SSE data", token)
			}
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatalf("peer %s timed out waiting for SSE data", token)
		}
		cancel()
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSSE_PostToClosedSession(t *testing.T) {
	srv, _ := newSSETestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// POST with X-Transport: sse but no active session — should fall back to HTTP.
	// But the message type is not "sync", so it should return 400.
	msg := client.Message{Type: "unknown"}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer1")
	req.Header.Set("X-Transport", "sse")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported message type, got %d", resp.StatusCode)
	}
}

func TestSSE_CanHandle(t *testing.T) {
	transport := &SSETransport{}

	tests := []struct {
		name   string
		method string
		accept string
		xTrans string
		want   bool
	}{
		{"GET with event-stream accept", http.MethodGet, "text/event-stream", "", true},
		{"GET without event-stream", http.MethodGet, "application/json", "", false},
		{"POST with X-Transport sse", http.MethodPost, "", "sse", true},
		{"POST without X-Transport", http.MethodPost, "", "", false},
		{"POST with wrong X-Transport", http.MethodPost, "", "http", false},
		{"PUT request", http.MethodPut, "", "sse", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, "http://example.com", nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			if tt.xTrans != "" {
				req.Header.Set("X-Transport", tt.xTrans)
			}
			if got := transport.CanHandle(req); got != tt.want {
				t.Fatalf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandler_MultipleTransports_SameServer(t *testing.T) {
	ss := storesync.New(memory.New(), int64(100*time.Millisecond))
	tokens := map[string]string{"token-peer1": "peer1"}
	srv := New(ss, TokenAuth(tokens))

	ctx := context.Background()
	if err := ss.SetItem(ctx, "app", "multi-key", "multi-val"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", srv.Handler(srv.WebSocket()))
	mux.Handle("/http", srv.Handler(srv.HTTP()))
	mux.Handle("/sse", srv.Handler(srv.SSE()))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Verify HTTP works.
	msg := client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/http", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-peer1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var httpResp client.Message
	json.NewDecoder(resp.Body).Decode(&httpResp)
	resp.Body.Close()

	if httpResp.Payload == nil || len(httpResp.Payload.Items) < 1 {
		t.Fatal("expected items from HTTP endpoint")
	}

	// Verify WebSocket works on same server.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn := dialWS(t, wsURL, "token-peer1")
	if err := conn.WriteJSON(client.Message{Type: "sync", Payload: &storesync.SyncPayload{}}); err != nil {
		t.Fatal(err)
	}
	var wsResp client.Message
	if err := conn.ReadJSON(&wsResp); err != nil {
		t.Fatal(err)
	}
	if wsResp.Payload == nil || len(wsResp.Payload.Items) < 1 {
		t.Fatal("expected items from WS endpoint")
	}
}
