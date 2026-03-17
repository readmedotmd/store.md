package ephemeral

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/sync/core"
)

// memStore is a minimal in-memory Store for testing.
type memStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string]string)}
}

func (s *memStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", storemd.ErrNotFound
	}
	return v, nil
}

func (s *memStore) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
	return nil
}

func (s *memStore) SetIfNotExists(_ context.Context, key, value string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = value
	return true, nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return storemd.ErrNotFound
	}
	delete(s.data, key)
	return nil
}

func (s *memStore) Close() error { return nil }

func (s *memStore) List(_ context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []storemd.KeyValuePair
	for k, v := range s.data {
		if args.Prefix != "" && len(k) < len(args.Prefix) {
			continue
		}
		if args.Prefix != "" && k[:len(args.Prefix)] != args.Prefix {
			continue
		}
		if args.StartAfter != "" && k <= args.StartAfter {
			continue
		}
		result = append(result, storemd.KeyValuePair{Key: k, Value: v})
	}
	// Sort for deterministic ordering
	for i := range result {
		for j := i + 1; j < len(result); j++ {
			if result[i].Key > result[j].Key {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	if args.Limit > 0 && len(result) > args.Limit {
		result = result[:args.Limit]
	}
	return result, nil
}

func (s *memStore) keyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// newTestStoreSync creates a StoreSync with ephemeral hooks installed.
func newTestStoreSync(nodeID string, eph *Layer) (*core.StoreSync, *memStore) {
	store := newMemStore()
	ss := core.NewWithOptions(store, append(eph.Options(), core.WithTimeOffset(10_000_000_000))...)
	eph.Bind(ss)
	return ss, store
}

func TestParseEphKey(t *testing.T) {
	target, msgID, ok := parseEphKey("%eph%nodeB%msg123")
	if !ok {
		t.Fatal("expected ok")
	}
	if target != "nodeB" {
		t.Fatalf("expected nodeB, got %s", target)
	}
	if msgID != "msg123" {
		t.Fatalf("expected msg123, got %s", msgID)
	}

	// Non-ephemeral key
	_, _, ok = parseEphKey("%sync%view%foo")
	if ok {
		t.Fatal("expected not ok for non-ephemeral key")
	}

	// No message ID separator
	_, _, ok = parseEphKey("%eph%nodeB")
	if ok {
		t.Fatal("expected not ok for missing separator")
	}
}

func TestSyncOutFilter_OnlyTargetPeer(t *testing.T) {
	eph := New("nodeA")
	ss, _ := newTestStoreSync("nodeA", eph)
	defer ss.Close()

	ctx := context.Background()

	// Send ephemeral message to nodeB
	if err := eph.Send(ctx, "nodeB", "chat", "hello"); err != nil {
		t.Fatal(err)
	}

	// SyncOut for nodeB should include the item
	payloadB, err := ss.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payloadB == nil || len(payloadB.Items) == 0 {
		t.Fatal("expected items for nodeB")
	}

	found := false
	for _, item := range payloadB.Items {
		if _, _, ok := parseEphKey(item.Key); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("expected ephemeral item in nodeB payload")
	}
}

func TestSyncOutFilter_ExcludesOtherPeers(t *testing.T) {
	eph := New("nodeA")
	ss, _ := newTestStoreSync("nodeA", eph)
	defer ss.Close()

	ctx := context.Background()

	if err := eph.Send(ctx, "nodeB", "chat", "hello"); err != nil {
		t.Fatal(err)
	}

	// SyncOut for nodeC should NOT include the item
	payloadC, err := ss.Sync(ctx, "nodeC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payloadC != nil {
		for _, item := range payloadC.Items {
			if _, _, ok := parseEphKey(item.Key); ok {
				t.Fatal("ephemeral item should not be sent to nodeC")
			}
		}
	}
}

func TestSyncOutFilter_CursorAdvancesPastFiltered(t *testing.T) {
	// Use a small offset so write timestamps are close to now and the cursor
	// can advance past them within the test.
	eph := New("nodeA")
	store := newMemStore()
	ss := core.NewWithOptions(store, append(eph.Options(), core.WithTimeOffset(1_000_000))...) // 1ms offset
	eph.Bind(ss)
	defer ss.Close()

	ctx := context.Background()

	// Send ephemeral to nodeB
	if err := eph.Send(ctx, "nodeB", "chat", "msg1"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	// Write a normal (non-ephemeral) item
	if err := ss.SetItem(ctx, "", "normalKey", "normalVal"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	// SyncOut for nodeC: should skip the ephemeral but include the normal item
	payloadC, err := ss.Sync(ctx, "nodeC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payloadC == nil {
		t.Fatal("expected payload for nodeC with normal item")
	}

	foundNormal := false
	for _, item := range payloadC.Items {
		if item.Key == "normalKey" {
			foundNormal = true
		}
		if _, _, ok := parseEphKey(item.Key); ok {
			t.Fatal("ephemeral item leaked to nodeC")
		}
	}
	if !foundNormal {
		t.Fatal("expected normal item in nodeC payload")
	}

	// Second sync for nodeC should return nil (cursor advanced past everything)
	payload2, err := ss.Sync(ctx, "nodeC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload2 != nil {
		t.Fatal("expected nil payload on second sync (cursor should have advanced)")
	}
}

func TestSyncInFilter_NoPersistAndHandlerFires(t *testing.T) {
	// Receiver side
	ephB := New("nodeB")
	ssB, storeB := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var received []Envelope
	var mu sync.Mutex
	ephB.Handle("chat", func(msg Envelope) {
		mu.Lock()
		received = append(received, msg)
		mu.Unlock()
	})

	// Sender side
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ctx := context.Background()

	if err := ephA.Send(ctx, "nodeB", "chat", "hello from A"); err != nil {
		t.Fatal(err)
	}

	// Simulate sync: A → B
	outPayload, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if outPayload == nil {
		t.Fatal("expected outgoing payload")
	}

	// Apply on B
	_, err = ssB.Sync(ctx, "nodeA", outPayload)
	if err != nil {
		t.Fatal(err)
	}

	// Handler should have fired
	mu.Lock()
	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if received[0].Data != "hello from A" {
		t.Fatalf("expected 'hello from A', got %q", received[0].Data)
	}
	if received[0].From != "nodeA" {
		t.Fatalf("expected from nodeA, got %q", received[0].From)
	}
	mu.Unlock()

	// Verify nothing was persisted on B (only internal sync keys, no ephemeral view/value)
	keysBefore := storeB.keyCount()
	// The store should have only the lastsyncout cursor key, nothing ephemeral
	for k := range storeB.data {
		if _, _, ok := parseEphKey(k); ok {
			t.Fatalf("ephemeral key persisted on receiver: %s", k)
		}
		// Also check view/value keys for ephemeral content
		if len(k) > len("%sync%view%"+keyPrefix) && k[:len("%sync%view%"+keyPrefix)] == "%sync%view%"+keyPrefix {
			t.Fatalf("ephemeral view key persisted on receiver: %s", k)
		}
	}
	_ = keysBefore
}

func TestSyncInFilter_Dedup(t *testing.T) {
	ephB := New("nodeB", WithSeenCapacity(100))
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	callCount := 0
	ephB.Handle("ping", func(msg Envelope) {
		callCount++
	})

	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ctx := context.Background()

	if err := ephA.Send(ctx, "nodeB", "ping", "1"); err != nil {
		t.Fatal(err)
	}

	outPayload, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Deliver the same payload twice
	ssB.Sync(ctx, "nodeA", outPayload)
	ssB.Sync(ctx, "nodeA", outPayload)

	if callCount != 1 {
		t.Fatalf("expected handler called once (dedup), got %d", callCount)
	}
}

func TestPostSyncOut_DeletesDelivered(t *testing.T) {
	eph := New("nodeA")
	ss, store := newTestStoreSync("nodeA", eph)
	defer ss.Close()

	ctx := context.Background()

	if err := eph.Send(ctx, "nodeB", "chat", "hello"); err != nil {
		t.Fatal(err)
	}

	// Verify item exists in store before sync
	hasEph := false
	for k := range store.data {
		if _, _, ok := parseEphKey(k); ok {
			hasEph = true
		}
	}
	// The view key uses %sync%view%%eph%... so check for that
	hasEphView := false
	for k := range store.data {
		if len(k) > 12 && k[:12] == "%sync%view%%" {
			hasEphView = true
		}
	}
	if !hasEph && !hasEphView {
		// Check if any key contains "eph"
		found := false
		for k := range store.data {
			if len(k) > 4 {
				found = true
			}
		}
		if !found {
			t.Fatal("expected ephemeral data in store before sync")
		}
	}

	// SyncOut to nodeB — this triggers postSyncOut which deletes the items
	_, err := ss.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify ephemeral items are cleaned up from store
	for k := range store.data {
		// View keys for ephemeral items
		viewPrefix := core.ViewKey(keyPrefix)
		if len(k) >= len(viewPrefix) && k[:len(viewPrefix)] == viewPrefix {
			t.Fatalf("ephemeral view key not cleaned up: %s", k)
		}
	}
}

func TestNonEphemeralItemsUnaffected(t *testing.T) {
	eph := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", eph)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	ctx := context.Background()

	// Write a normal item
	if err := ssA.SetItem(ctx, "", "greeting", "hi"); err != nil {
		t.Fatal(err)
	}

	// Sync A → B
	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected payload")
	}

	_, err = ssB.Sync(ctx, "nodeA", out)
	if err != nil {
		t.Fatal(err)
	}

	// Normal item should be persisted on B
	val, err := ssB.Get(ctx, "greeting")
	if err != nil {
		t.Fatal(err)
	}
	if val != "hi" {
		t.Fatalf("expected 'hi', got %q", val)
	}
}

func TestFullRoundTrip(t *testing.T) {
	// Set up sender
	ephA := New("nodeA")
	ssA, storeA := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	// Set up receiver
	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var receivedMsg Envelope
	ephB.Handle("task", func(msg Envelope) {
		receivedMsg = msg
	})

	ctx := context.Background()

	// 1. Send
	if err := ephA.Send(ctx, "nodeB", "task", `{"action":"deploy"}`); err != nil {
		t.Fatal(err)
	}

	// 2. Sync A → B
	outPayload, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if outPayload == nil {
		t.Fatal("expected outgoing payload")
	}

	// 3. B processes
	_, err = ssB.Sync(ctx, "nodeA", outPayload)
	if err != nil {
		t.Fatal(err)
	}

	// 4. Verify handler fired
	if receivedMsg.MsgType != "task" {
		t.Fatalf("expected msgType 'task', got %q", receivedMsg.MsgType)
	}
	if receivedMsg.Data != `{"action":"deploy"}` {
		t.Fatalf("unexpected data: %q", receivedMsg.Data)
	}
	if receivedMsg.From != "nodeA" {
		t.Fatalf("expected from nodeA, got %q", receivedMsg.From)
	}

	// 5. Verify message deleted from sender's store (postSyncOut cleaned it up)
	ephKeys := 0
	for k := range storeA.data {
		vp := core.ViewKey(keyPrefix)
		if len(k) >= len(vp) && k[:len(vp)] == vp {
			ephKeys++
		}
	}
	if ephKeys > 0 {
		t.Fatalf("expected 0 ephemeral view keys after delivery, got %d", ephKeys)
	}
}

func TestMultipleMessages_SameTarget(t *testing.T) {
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var received []Envelope
	var mu sync.Mutex
	ephB.Handle("chat", func(msg Envelope) {
		mu.Lock()
		received = append(received, msg)
		mu.Unlock()
	})

	ctx := context.Background()

	// Send 3 messages
	for i := 0; i < 3; i++ {
		if err := ephA.Send(ctx, "nodeB", "chat", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	// Sync all at once
	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected payload")
	}

	_, err = ssB.Sync(ctx, "nodeA", out)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if len(received) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(received))
	}
	mu.Unlock()
}

func TestMultipleTargets(t *testing.T) {
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	ephC := New("nodeC")
	ssC, _ := newTestStoreSync("nodeC", ephC)
	defer ssC.Close()

	var bReceived, cReceived []Envelope
	var mu sync.Mutex
	ephB.Handle("msg", func(msg Envelope) {
		mu.Lock()
		bReceived = append(bReceived, msg)
		mu.Unlock()
	})
	ephC.Handle("msg", func(msg Envelope) {
		mu.Lock()
		cReceived = append(cReceived, msg)
		mu.Unlock()
	})

	ctx := context.Background()

	// Send one to B and one to C
	if err := ephA.Send(ctx, "nodeB", "msg", "for-B"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := ephA.Send(ctx, "nodeC", "msg", "for-C"); err != nil {
		t.Fatal(err)
	}

	// Sync A → B
	outB, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if outB != nil {
		ssB.Sync(ctx, "nodeA", outB)
	}

	// Sync A → C
	outC, err := ssA.Sync(ctx, "nodeC", nil)
	if err != nil {
		t.Fatal(err)
	}
	if outC != nil {
		ssC.Sync(ctx, "nodeA", outC)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bReceived) != 1 || bReceived[0].Data != "for-B" {
		t.Fatalf("B expected 'for-B', got %v", bReceived)
	}
	if len(cReceived) != 1 || cReceived[0].Data != "for-C" {
		t.Fatalf("C expected 'for-C', got %v", cReceived)
	}
}

func TestNoHandler_NoError(t *testing.T) {
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	// No handler registered on B for "unknown-type"

	ctx := context.Background()

	if err := ephA.Send(ctx, "nodeB", "unknown-type", "data"); err != nil {
		t.Fatal(err)
	}

	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected payload")
	}

	// Should not error even though there's no handler
	_, err = ssB.Sync(ctx, "nodeA", out)
	if err != nil {
		t.Fatalf("SyncIn should not error on unhandled message type: %v", err)
	}
}

func TestSeenCapacity_Eviction(t *testing.T) {
	ephB := New("nodeB", WithSeenCapacity(3))
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	callCount := 0
	ephB.Handle("ping", func(msg Envelope) {
		callCount++
	})

	ctx := context.Background()

	// Create 4 payloads with different IDs
	payloads := make([]*core.SyncPayload, 4)
	for i := 0; i < 4; i++ {
		ephA := New("nodeA")
		ssA, _ := newTestStoreSync("nodeA", ephA)
		if err := ephA.Send(ctx, "nodeB", "ping", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
		p, err := ssA.Sync(ctx, "nodeB", nil)
		if err != nil {
			t.Fatal(err)
		}
		payloads[i] = p
		ssA.Close()
	}

	// Deliver all 4 unique messages
	for _, p := range payloads {
		ssB.Sync(ctx, "nodeA", p)
	}
	if callCount != 4 {
		t.Fatalf("expected 4 unique messages handled, got %d", callCount)
	}

	// Replay the first message — capacity is 3, so the first should have been evicted
	ssB.Sync(ctx, "nodeA", payloads[0])
	if callCount != 5 {
		t.Fatalf("expected evicted message to be re-handled, got count %d", callCount)
	}

	// Replay the last message — should still be in the seen set
	ssB.Sync(ctx, "nodeA", payloads[3])
	if callCount != 5 {
		t.Fatalf("expected recent message to be deduped, got count %d", callCount)
	}
}

func TestEphemeralMixedWithNormalSync(t *testing.T) {
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var ephReceived []Envelope
	ephB.Handle("notify", func(msg Envelope) {
		ephReceived = append(ephReceived, msg)
	})

	ctx := context.Background()

	// Write a normal persistent item and an ephemeral message
	if err := ssA.SetItem(ctx, "", "persistent-key", "persistent-val"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := ephA.Send(ctx, "nodeB", "notify", "you have new data"); err != nil {
		t.Fatal(err)
	}

	// Sync A → B
	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected payload")
	}

	_, err = ssB.Sync(ctx, "nodeA", out)
	if err != nil {
		t.Fatal(err)
	}

	// Persistent item should be on B
	val, err := ssB.Get(ctx, "persistent-key")
	if err != nil {
		t.Fatalf("persistent item should be synced: %v", err)
	}
	if val != "persistent-val" {
		t.Fatalf("expected 'persistent-val', got %q", val)
	}

	// Ephemeral handler should have fired
	if len(ephReceived) != 1 {
		t.Fatalf("expected 1 ephemeral notification, got %d", len(ephReceived))
	}
	if ephReceived[0].Data != "you have new data" {
		t.Fatalf("expected 'you have new data', got %q", ephReceived[0].Data)
	}

	// Ephemeral message should NOT be persisted on B
	_, err = ssB.Get(ctx, keyPrefix+"nodeB")
	if err == nil {
		t.Fatal("ephemeral key should not be persisted on receiver")
	}
}

func TestEnvelope_HasCorrectFields(t *testing.T) {
	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var env Envelope
	ephB.Handle("test", func(msg Envelope) {
		env = msg
	})

	ctx := context.Background()
	before := time.Now().UnixNano()

	if err := ephA.Send(ctx, "nodeB", "test", "payload"); err != nil {
		t.Fatal(err)
	}

	out, _ := ssA.Sync(ctx, "nodeB", nil)
	ssB.Sync(ctx, "nodeA", out)

	if env.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if env.From != "nodeA" {
		t.Fatalf("expected From='nodeA', got %q", env.From)
	}
	if env.MsgType != "test" {
		t.Fatalf("expected MsgType='test', got %q", env.MsgType)
	}
	if env.Data != "payload" {
		t.Fatalf("expected Data='payload', got %q", env.Data)
	}
	if env.CreatedAt < before {
		t.Fatalf("expected CreatedAt >= %d, got %d", before, env.CreatedAt)
	}
}

func TestInMemoryStore_NoResidualData(t *testing.T) {
	// Verify that with an in-memory backing store, after a full send→sync→deliver
	// cycle, nothing remains in the sender's store except internal sync cursors.
	ephA := New("nodeA")
	ssA, storeA := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, storeB := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	ephB.Handle("chat", func(msg Envelope) {})

	ctx := context.Background()

	// Send multiple messages
	for i := 0; i < 5; i++ {
		if err := ephA.Send(ctx, "nodeB", "chat", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	// Before sync: sender should have ephemeral data in store
	preCount := storeA.keyCount()
	if preCount == 0 {
		t.Fatal("expected sender store to have data before sync")
	}

	// Sync A → B
	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected payload")
	}

	_, err = ssB.Sync(ctx, "nodeA", out)
	if err != nil {
		t.Fatal(err)
	}

	// After sync: sender's ephemeral data should be cleaned up by postSyncOut.
	// Only internal sync cursor keys (%sync%lastsyncout%) should remain.
	storeA.mu.RLock()
	for k := range storeA.data {
		viewPrefix := core.ViewKey(keyPrefix)
		if len(k) >= len(viewPrefix) && k[:len(viewPrefix)] == viewPrefix {
			t.Fatalf("ephemeral view key still in sender store after delivery: %s", k)
		}
	}
	storeA.mu.RUnlock()

	// Receiver should have no ephemeral data persisted
	storeB.mu.RLock()
	for k := range storeB.data {
		viewPrefix := core.ViewKey(keyPrefix)
		if len(k) >= len(viewPrefix) && k[:len(viewPrefix)] == viewPrefix {
			t.Fatalf("ephemeral view key persisted on receiver: %s", k)
		}
	}
	storeB.mu.RUnlock()
}

func TestSyncInFilter_MalformedEnvelope(t *testing.T) {
	// Verify that a malformed envelope value doesn't crash the filter.
	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	called := false
	ephB.Handle("chat", func(msg Envelope) {
		called = true
	})

	ctx := context.Background()

	// Craft a payload with an ephemeral key but invalid JSON value.
	payload := core.SyncPayload{
		Items: []core.SyncStoreItem{
			{
				Key:            "%eph%nodeB%bad-msg",
				Value:          "not valid json",
				Timestamp:      time.Now().UnixNano(),
				ID:             "bad-id",
				WriteTimestamp: time.Now().UnixNano(),
			},
		},
	}

	_, err := ssB.Sync(ctx, "nodeA", &payload)
	if err != nil {
		t.Fatal(err)
	}

	if called {
		t.Fatal("handler should not be called for malformed envelope")
	}
}

func TestHandlerReplacement(t *testing.T) {
	// Verify that registering a second handler for the same type replaces the first.
	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var which string
	ephB.Handle("chat", func(msg Envelope) { which = "first" })
	ephB.Handle("chat", func(msg Envelope) { which = "second" })

	ephA := New("nodeA")
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ctx := context.Background()

	if err := ephA.Send(ctx, "nodeB", "chat", "hello"); err != nil {
		t.Fatal(err)
	}
	out, _ := ssA.Sync(ctx, "nodeB", nil)
	ssB.Sync(ctx, "nodeA", out)

	if which != "second" {
		t.Fatalf("expected second handler to run, got %q", which)
	}
}

func TestOnDelivered_Hook(t *testing.T) {
	var mu sync.Mutex
	var deliveredEnvs []Envelope
	var deliveredPeers []string

	ephA := New("nodeA", WithOnDelivered(func(env Envelope, peerID string) {
		mu.Lock()
		deliveredEnvs = append(deliveredEnvs, env)
		deliveredPeers = append(deliveredPeers, peerID)
		mu.Unlock()
	}))
	ssA, _ := newTestStoreSync("nodeA", ephA)
	defer ssA.Close()

	ephB := New("nodeB")
	ssB, _ := newTestStoreSync("nodeB", ephB)
	defer ssB.Close()

	var received []Envelope
	ephB.Handle("chat", func(msg Envelope) {
		received = append(received, msg)
	})

	ctx := context.Background()

	if err := ephA.Send(ctx, "nodeB", "chat", "hello"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// SyncOut from A to B — should trigger OnDelivered.
	out, err := ssA.Sync(ctx, "nodeB", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected sync payload")
	}

	mu.Lock()
	if len(deliveredEnvs) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveredEnvs))
	}
	if deliveredEnvs[0].Data != "hello" {
		t.Fatalf("expected hello, got %q", deliveredEnvs[0].Data)
	}
	if deliveredPeers[0] != "nodeB" {
		t.Fatalf("expected nodeB, got %q", deliveredPeers[0])
	}
	mu.Unlock()
}
