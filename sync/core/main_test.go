package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
)

// memStore is a minimal in-memory Store with context-aware signatures for testing.
// This is needed because the memory backend hasn't been updated with context params yet.
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
	defer s.mu.Unlock()
	s.data[key] = value
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
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		if args.Prefix != "" && !strings.HasPrefix(k, args.Prefix) {
			continue
		}
		if args.StartAfter != "" && k <= args.StartAfter {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if args.Limit > 0 && len(keys) > args.Limit {
		keys = keys[:args.Limit]
	}
	result := make([]storemd.KeyValuePair, len(keys))
	for i, k := range keys {
		result[i] = storemd.KeyValuePair{Key: k, Value: s.data[k]}
	}
	return result, nil
}

func newTestStore() storemd.Store {
	return newMemStore()
}

func newTestSyncStore() *StoreSync {
	return New(newTestStore(), int64(100*time.Millisecond)) // small offset for tests
}

func TestSetAndGet(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "myapp", "greeting", "hello"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	item, err := s.GetItem(ctx, "greeting")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.App != "myapp" {
		t.Fatalf("expected app %q, got %q", "myapp", item.App)
	}
	if item.Key != "greeting" {
		t.Fatalf("expected key %q, got %q", "greeting", item.Key)
	}
	if item.Value != "hello" {
		t.Fatalf("expected value %q, got %q", "hello", item.Value)
	}
	if item.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if item.Timestamp == 0 {
		t.Fatal("expected non-zero timestamp")
	}
	if item.WriteTimestamp == 0 {
		t.Fatal("expected non-zero write timestamp")
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	_, err := s.GetItem(ctx, "nonexistent")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSet_Overwrite(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "app", "key1", "value1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem(ctx, "app", "key1", "value2"); err != nil {
		t.Fatalf("Set overwrite failed: %v", err)
	}

	item, err := s.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "value2" {
		t.Fatalf("expected %q, got %q", "value2", item.Value)
	}
}

func TestSet_OlderTimestampIgnored(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	// Set an item directly with a near-future timestamp (within MaxClockSkew)
	futureItem := SyncStoreItem{
		App:       "app",
		Key:       "key1",
		Value:     "future",
		Timestamp: time.Now().UnixNano() + int64(2*time.Minute),
		ID:        "future-id",
	}
	if err := s.setItem(ctx, futureItem, s.notifyListeners); err != nil {
		t.Fatalf("set future item failed: %v", err)
	}

	// Try to overwrite with a current-time item (older timestamp)
	if err := s.SetItem(ctx, "app", "key1", "older"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	item, err := s.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "future" {
		t.Fatalf("expected older write to be ignored, got value %q", item.Value)
	}
}

func TestList(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem(ctx, "app", k, "val-"+k); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems(ctx, "", "", 0)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestList_Prefix(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	for _, k := range []string{"x/a", "x/b", "y/a"} {
		if err := s.SetItem(ctx, "app", k, "val"); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems(ctx, "x/", "", 0)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items with prefix x/, got %d", len(items))
	}
}

func TestList_Limit(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem(ctx, "app", k, "val"); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems(ctx, "", "", 2)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestSyncOut_Empty(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	payload, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_ReturnsItems(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	payload, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_Incremental(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Wait for writeTimestamp to be in the past
	time.Sleep(150 * time.Millisecond)

	// First sync out gets key1
	payload1, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut 1 failed: %v", err)
	}
	if len(payload1.Items) != 1 {
		t.Fatalf("expected 1 item in first sync, got %d", len(payload1.Items))
	}
	if err := s.AckSyncOut(ctx, "peer1", payload1); err != nil {
		t.Fatalf("AckSyncOut failed: %v", err)
	}

	// Add another item
	time.Sleep(time.Millisecond)
	if err := s.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Wait for writeTimestamp to be in the past
	time.Sleep(150 * time.Millisecond)

	// Second sync out should only get key2
	payload2, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut 2 failed: %v", err)
	}
	if len(payload2.Items) != 1 {
		t.Fatalf("expected 1 item in second sync, got %d", len(payload2.Items))
	}
	if payload2.Items[0].Key != "key2" {
		t.Fatalf("expected key2, got %q", payload2.Items[0].Key)
	}
}

func TestSyncOut_Limit(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem(ctx, "app", k, "val"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	payload, err := s.SyncOut(ctx, "peer1", 2)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_SeparatePerPeer(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	// Sync out to peer1
	p1, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 failed: %v", err)
	}
	if len(p1.Items) != 1 {
		t.Fatalf("expected 1 item for peer1, got %d", len(p1.Items))
	}
	if err := s.AckSyncOut(ctx, "peer1", p1); err != nil {
		t.Fatalf("AckSyncOut peer1 failed: %v", err)
	}

	// Sync out to peer2 should also get key1 (independent cursor)
	p2, err := s.SyncOut(ctx, "peer2", 0)
	if err != nil {
		t.Fatalf("SyncOut peer2 failed: %v", err)
	}
	if len(p2.Items) != 1 {
		t.Fatalf("expected 1 item for peer2, got %d", len(p2.Items))
	}

	// Sync out to peer1 again should get nothing
	p1again, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 again failed: %v", err)
	}
	if len(p1again.Items) != 0 {
		t.Fatalf("expected 0 items for peer1 second sync, got %d", len(p1again.Items))
	}
}

func TestSyncIn(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				App:       "app",
				Key:       "key1",
				Value:     "val1",
				Timestamp: time.Now().UnixNano(),
				ID:        "id-1",
			},
			{
				App:       "app",
				Key:       "key2",
				Value:     "val2",
				Timestamp: time.Now().UnixNano(),
				ID:        "id-2",
			},
		},
		LastSyncTimestamp: time.Now().UnixNano(),
	}

	if err := s.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify items are accessible via Get
	item1, err := s.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("Get key1 failed: %v", err)
	}
	if item1.Value != "val1" {
		t.Fatalf("expected %q, got %q", "val1", item1.Value)
	}

	item2, err := s.GetItem(ctx, "key2")
	if err != nil {
		t.Fatalf("Get key2 failed: %v", err)
	}
	if item2.Value != "val2" {
		t.Fatalf("expected %q, got %q", "val2", item2.Value)
	}
}

func TestSyncIn_TimestampConflict(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	// Set a local value
	if err := s.SetItem(ctx, "app", "key1", "local"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// SyncIn an older value (should be ignored)
	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				App:       "app",
				Key:       "key1",
				Value:     "remote-old",
				Timestamp: time.Now().UnixNano() - int64(10*time.Second), // older but within clock skew
				ID:        "remote-id",
			},
		},
		LastSyncTimestamp: time.Now().UnixNano(),
	}

	if err := s.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	item, err := s.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "local" {
		t.Fatalf("expected local value to win, got %q", item.Value)
	}
}

func TestSyncIn_SeparateTimestampFromSyncOut(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	// SyncIn from peer1
	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				App:       "app",
				Key:       "from-peer",
				Value:     "val",
				Timestamp: time.Now().UnixNano(),
				ID:        "peer-id-1",
			},
		},
		LastSyncTimestamp: 12345,
	}
	if err := s.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// SyncOut to peer1 should still return items (independent cursor)
	out, err := s.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	// Should have the synced-in item in the queue
	if len(out.Items) == 0 {
		t.Fatal("expected SyncOut to return items independently of SyncIn timestamp")
	}
}

func TestSyncRoundTrip(t *testing.T) {
	// Simulate two stores syncing with each other
	store1 := newTestSyncStore()
	defer store1.Close()
	store2 := newTestSyncStore()
	defer store2.Close()
	ctx := context.Background()

	// Store1 sets some data
	if err := store1.SetItem(ctx, "app", "key1", "from-store1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// SyncOut from store1
	payload, err := store1.SyncOut(ctx, "store2", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}

	// SyncIn to store2
	if err := store2.SyncIn(ctx, "store1", *payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify store2 has the data
	item, err := store2.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("Get from store2 failed: %v", err)
	}
	if item.Value != "from-store1" {
		t.Fatalf("expected %q, got %q", "from-store1", item.Value)
	}
}

func TestOnUpdate_Set(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	var received []SyncStoreItem
	s.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := s.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(received))
	}
	if received[0].Key != "key1" || received[0].Value != "val1" {
		t.Fatalf("unexpected first update: %+v", received[0])
	}
	if received[1].Key != "key2" || received[1].Value != "val2" {
		t.Fatalf("unexpected second update: %+v", received[1])
	}
}

func TestOnUpdate_SyncIn(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	var received []SyncStoreItem
	s.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				App:       "app",
				Key:       "remote-key",
				Value:     "remote-val",
				Timestamp: time.Now().UnixNano(),
				ID:        "remote-id",
			},
		},
		LastSyncTimestamp: time.Now().UnixNano(),
	}
	if err := s.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	if len(received) != 1 {
		t.Fatalf("expected 1 update from SyncIn, got %d", len(received))
	}
	if received[0].Key != "remote-key" {
		t.Fatalf("expected key %q, got %q", "remote-key", received[0].Key)
	}
}

func TestOnUpdate_NotCalledForOlderTimestamp(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	if err := s.SetItem(ctx, "app", "key1", "current"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	var received []SyncStoreItem
	s.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	// SyncIn an older value -- should be ignored, no listener call
	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				App:       "app",
				Key:       "key1",
				Value:     "old",
				Timestamp: time.Now().UnixNano() - int64(10*time.Second), // older but within clock skew
				ID:        "old-id",
			},
		},
		LastSyncTimestamp: time.Now().UnixNano(),
	}
	if err := s.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	if len(received) != 0 {
		t.Fatalf("expected no updates for older timestamp, got %d", len(received))
	}
}

func TestOnUpdate_Unsubscribe(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	var count int
	unsub := s.OnUpdate(func(item SyncStoreItem) {
		count++
	})

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 update, got %d", count)
	}

	unsub()

	if err := s.SetItem(ctx, "app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected still 1 update after unsubscribe, got %d", count)
	}
}

func TestOnUpdate_MultipleListeners(t *testing.T) {
	s := newTestSyncStore()
	defer s.Close()
	ctx := context.Background()

	var count1, count2 int
	s.OnUpdate(func(item SyncStoreItem) { count1++ })
	s.OnUpdate(func(item SyncStoreItem) { count2++ })

	if err := s.SetItem(ctx, "app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected both listeners called once, got %d and %d", count1, count2)
	}
}

// --- New tests for the refactored features ---

func TestGCCleansOldValues(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)
	defer ss.Close()

	ctx := context.Background()

	// Write first value.
	if err := ss.SetItem(ctx, "", "key1", "value1"); err != nil {
		t.Fatal(err)
	}

	// Give GC a moment to process.
	time.Sleep(100 * time.Millisecond)

	// Get the first value's ID.
	item1, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	firstID := item1.ID

	// Overwrite with second value.
	if err := ss.SetItem(ctx, "", "key1", "value2"); err != nil {
		t.Fatal(err)
	}

	// Give GC a moment to process.
	time.Sleep(200 * time.Millisecond)

	// The old value key should have been cleaned up.
	_, err = store.Get(ctx, ValueKey(firstID))
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Errorf("expected old value to be cleaned up, got err=%v", err)
	}

	// The new value should exist.
	item2, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if item2.Value != "value2" {
		t.Errorf("expected value2, got %s", item2.Value)
	}
}

func TestTimestampBoundsCheck(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)
	defer ss.Close()
	ctx := context.Background()

	// Create an item with a timestamp far in the future.
	futureItem := SyncStoreItem{
		Key:       "future-key",
		Value:     "future-value",
		Timestamp: time.Now().UnixNano() + int64(10*time.Minute), // 10 min in future
		ID:        "future-id",
	}

	err := ss.setItem(ctx, futureItem, ss.notifyListeners)
	if err == nil {
		t.Fatal("expected error for future timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "clock skew") {
		t.Errorf("expected clock skew error, got: %v", err)
	}

	// An item within the allowed skew should succeed.
	okItem := SyncStoreItem{
		Key:       "ok-key",
		Value:     "ok-value",
		Timestamp: time.Now().UnixNano() + int64(1*time.Minute), // 1 min in future, within 5 min window
		ID:        "ok-id",
	}
	if err := ss.setItem(ctx, okItem, ss.notifyListeners); err != nil {
		t.Fatalf("expected no error for item within skew, got: %v", err)
	}
}

func TestTimestampBoundsCheck_ProductionOffset(t *testing.T) {
	store := newMemStore()
	ss := New(store) // default 10s offset
	defer ss.Close()
	ctx := context.Background()

	// An item within the allowed skew should succeed with production offset.
	okItem := SyncStoreItem{
		Key:       "ok-key",
		Value:     "ok-value",
		Timestamp: time.Now().UnixNano() + int64(1*time.Minute),
		ID:        "ok-id",
	}
	if err := ss.setItem(ctx, okItem, ss.notifyListeners); err != nil {
		t.Fatalf("expected no error for item within skew, got: %v", err)
	}

	// An item far in the future should still be rejected.
	futureItem := SyncStoreItem{
		Key:       "future-key",
		Value:     "future-value",
		Timestamp: time.Now().UnixNano() + int64(10*time.Minute) + 10_000_000_000, // beyond skew + offset
		ID:        "future-id",
	}
	if err := ss.setItem(ctx, futureItem, ss.notifyListeners); err == nil {
		t.Fatal("expected error for future timestamp with production offset, got nil")
	}
}

func TestSyncInPayloadSizeLimit(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)
	defer ss.Close()

	ctx := context.Background()

	// Create a payload that exceeds the limit.
	items := make([]SyncStoreItem, MaxSyncInItems+1)
	for i := range items {
		items[i] = SyncStoreItem{
			Key:       fmt.Sprintf("key-%d", i),
			Value:     "val",
			Timestamp: time.Now().UnixNano(),
			ID:        fmt.Sprintf("id-%d", i),
		}
	}

	err := ss.SyncIn(ctx, "peer1", SyncPayload{Items: items})
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got: %v", err)
	}

	// A payload within the limit should succeed.
	smallPayload := SyncPayload{Items: items[:10]}
	if err := ss.SyncIn(ctx, "peer1", smallPayload); err != nil {
		t.Fatalf("expected no error for small payload, got: %v", err)
	}
}

func TestCloseStopsGCWorkers(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)

	// Write some items to trigger GC.
	ctx := context.Background()
	for i := range 5 {
		if err := ss.SetItem(ctx, "", fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Close should not hang.
	done := make(chan struct{})
	go func() {
		ss.Close()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return within 5 seconds")
	}
}

func TestConcurrentSetItem(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)
	defer ss.Close()

	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i%10) // Some key contention
			val := fmt.Sprintf("value-%d", i)
			if err := ss.SetItem(ctx, "", key, val); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent SetItem error: %v", err)
	}

	// Verify all keys have a value.
	for i := range 10 {
		key := fmt.Sprintf("key-%d", i)
		item, err := ss.GetItem(ctx, key)
		if err != nil {
			t.Errorf("GetItem(%s): %v", key, err)
			continue
		}
		if item.Value == "" {
			t.Errorf("GetItem(%s) returned empty value", key)
		}
	}
}

func TestNewWithOptions(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(5_000_000_000))
	defer ss.Close()

	ctx := context.Background()
	if err := ss.SetItem(ctx, "", "test", "value"); err != nil {
		t.Fatal(err)
	}
	item, err := ss.GetItem(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if item.Value != "value" {
		t.Errorf("expected 'value', got %q", item.Value)
	}
}

func TestDeleteReturnsTombstone(t *testing.T) {
	store := newMemStore()
	ss := New(store, 10_000_000_000)
	defer ss.Close()

	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "dkey", "dval"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Delete(ctx, "dkey"); err != nil {
		t.Fatal(err)
	}

	// Get should return ErrNotFound.
	_, err := ss.Get(ctx, "dkey")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got: %v", err)
	}

	// Delete again should return ErrNotFound.
	err = ss.Delete(ctx, "dkey")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Errorf("expected ErrNotFound on double delete, got: %v", err)
	}
}

func TestSetIfNotExists_Basic(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	ok, err := ss.SetIfNotExists(ctx, "newkey", "value1")
	if err != nil {
		t.Fatalf("SetIfNotExists failed: %v", err)
	}
	if !ok {
		t.Fatal("expected true for new key")
	}

	val, err := ss.Get(ctx, "newkey")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if val != "value1" {
		t.Fatalf("expected %q, got %q", "value1", val)
	}
}

func TestSetIfNotExists_Duplicate(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	ok, err := ss.SetIfNotExists(ctx, "dupkey", "first")
	if err != nil {
		t.Fatalf("first SetIfNotExists failed: %v", err)
	}
	if !ok {
		t.Fatal("expected true for first call")
	}

	ok, err = ss.SetIfNotExists(ctx, "dupkey", "second")
	if err != nil {
		t.Fatalf("second SetIfNotExists failed: %v", err)
	}
	if ok {
		t.Fatal("expected false for duplicate key")
	}

	val, err := ss.Get(ctx, "dupkey")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if val != "first" {
		t.Fatalf("expected original value %q, got %q", "first", val)
	}
}

func TestSetIfNotExists_Concurrent(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	const n = 10
	results := make(chan bool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := ss.SetIfNotExists(ctx, "race-key", fmt.Sprintf("writer-%d", i))
			if err != nil {
				t.Errorf("SetIfNotExists from goroutine %d failed: %v", i, err)
				return
			}
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)

	wins := 0
	for ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins)
	}
}

func TestSetIfNotExists_DoesNotSyncOnFalse(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	// Set key via normal SetItem first.
	if err := ss.SetItem(ctx, "app", "existing", "original"); err != nil {
		t.Fatal(err)
	}

	var updates []SyncStoreItem
	ss.OnUpdate(func(item SyncStoreItem) {
		updates = append(updates, item)
	})

	// SetIfNotExists should return false and NOT trigger OnUpdate.
	ok, err := ss.SetIfNotExists(ctx, "existing", "new-value")
	if err != nil {
		t.Fatalf("SetIfNotExists failed: %v", err)
	}
	if ok {
		t.Fatal("expected false for existing key")
	}
	if len(updates) != 0 {
		t.Fatalf("expected no updates when SetIfNotExists returns false, got %d", len(updates))
	}

	// Original value should be unchanged.
	val, err := ss.Get(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if val != "original" {
		t.Fatalf("expected %q, got %q", "original", val)
	}
}

func TestSetIfNotExists_SyncsToRemote(t *testing.T) {
	store1 := newTestSyncStore()
	defer store1.Close()
	store2 := newTestSyncStore()
	defer store2.Close()
	ctx := context.Background()

	// Use SetIfNotExists on store1.
	ok, err := store1.SetIfNotExists(ctx, "claim-key", "claimed-value")
	if err != nil {
		t.Fatalf("SetIfNotExists failed: %v", err)
	}
	if !ok {
		t.Fatal("expected true for new key")
	}

	// SyncOut from store1 should include the item.
	payload, err := store1.SyncOut(ctx, "store2", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) == 0 {
		t.Fatal("SyncOut returned no items — SetIfNotExists value not in sync queue")
	}

	found := false
	for _, item := range payload.Items {
		if item.Key == "claim-key" && item.Value == "claimed-value" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("claim-key not found in SyncOut payload")
	}

	// SyncIn to store2.
	if err := store2.SyncIn(ctx, "store1", *payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify store2 has the value.
	val, err := store2.Get(ctx, "claim-key")
	if err != nil {
		t.Fatalf("Get from store2 failed: %v", err)
	}
	if val != "claimed-value" {
		t.Fatalf("expected %q, got %q", "claimed-value", val)
	}
}

func TestSetIfNotExists_OnUpdateFires(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	var updates []SyncStoreItem
	ss.OnUpdate(func(item SyncStoreItem) {
		updates = append(updates, item)
	})

	ok, err := ss.SetIfNotExists(ctx, "notify-key", "notify-value")
	if err != nil {
		t.Fatalf("SetIfNotExists failed: %v", err)
	}
	if !ok {
		t.Fatal("expected true for new key")
	}

	if len(updates) != 1 {
		t.Fatalf("expected 1 OnUpdate call, got %d", len(updates))
	}
	if updates[0].Key != "notify-key" || updates[0].Value != "notify-value" {
		t.Fatalf("unexpected update: %+v", updates[0])
	}
}

// --- Hook tests ---

func TestSyncOutFilter_ExcludesItems(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncOutFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "secret" // exclude key "secret"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "public", "pub-val"); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetItem(ctx, "", "secret", "sec-val"); err != nil {
		t.Fatal(err)
	}

	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range payload.Items {
		if item.Key == "secret" {
			t.Fatal("filter should have excluded 'secret'")
		}
	}
	found := false
	for _, item := range payload.Items {
		if item.Key == "public" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'public' to be included")
	}
}

func TestSyncOutFilter_CursorAdvancesPastFilteredItems(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(1*time.Millisecond)),
		WithSyncOutFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "filtered"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "filtered", "val1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := ss.SetItem(ctx, "", "visible", "val2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	// First sync: should get "visible" only
	p1, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 1 || p1.Items[0].Key != "visible" {
		t.Fatalf("expected [visible], got %v", p1.Items)
	}
	if err := ss.AckSyncOut(ctx, "peer1", p1); err != nil {
		t.Fatal(err)
	}

	// Second sync: cursor should have advanced past both items
	p2, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 0 {
		t.Fatalf("expected 0 items on second sync, got %d", len(p2.Items))
	}
}

func TestSyncOutFilter_PerPeer(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncOutFilter(func(item SyncStoreItem, peerID string) bool {
			// Only "admin" items go to peer "admin", everything else goes everywhere
			if item.App == "admin" {
				return peerID == "admin-peer"
			}
			return true
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "admin", "config", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetItem(ctx, "public", "info", "hello"); err != nil {
		t.Fatal(err)
	}

	// Admin peer should get both
	adminPayload, err := ss.SyncOut(ctx, "admin-peer", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminPayload.Items) != 2 {
		t.Fatalf("admin should get 2 items, got %d", len(adminPayload.Items))
	}

	// Regular peer should only get the public item
	regularPayload, err := ss.SyncOut(ctx, "regular-peer", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(regularPayload.Items) != 1 {
		t.Fatalf("regular peer should get 1 item, got %d", len(regularPayload.Items))
	}
	if regularPayload.Items[0].Key != "info" {
		t.Fatalf("regular peer should get 'info', got %q", regularPayload.Items[0].Key)
	}
}

func TestSyncOutFilter_MultipleFilters(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncOutFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "a" // exclude "a"
		}),
		WithSyncOutFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "b" // exclude "b"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := ss.SetItem(ctx, "", k, "val"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Key != "c" {
		t.Fatalf("expected only [c], got %v", payload.Items)
	}
}

func TestSyncInFilter_SkipPersist(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncInFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "transient" // don't persist "transient"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{Key: "persistent", Value: "val1", Timestamp: time.Now().UnixNano(), ID: "id-1"},
			{Key: "transient", Value: "val2", Timestamp: time.Now().UnixNano(), ID: "id-2"},
		},
	}

	if err := ss.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	// "persistent" should be retrievable
	val, err := ss.Get(ctx, "persistent")
	if err != nil {
		t.Fatalf("expected persistent item: %v", err)
	}
	if val != "val1" {
		t.Fatalf("expected val1, got %q", val)
	}

	// "transient" should NOT be persisted
	_, err = ss.Get(ctx, "transient")
	if err == nil {
		t.Fatal("expected transient item to not be persisted")
	}
}

func TestSyncInFilter_ListenersStillFire(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncInFilter(func(item SyncStoreItem, peerID string) bool {
			return false // never persist
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	var received []SyncStoreItem
	ss.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{Key: "notif-key", Value: "notif-val", Timestamp: time.Now().UnixNano(), ID: "notif-id"},
		},
	}

	if err := ss.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	if len(received) != 1 {
		t.Fatalf("expected 1 listener notification, got %d", len(received))
	}
	if received[0].Key != "notif-key" {
		t.Fatalf("expected key 'notif-key', got %q", received[0].Key)
	}
}

func TestSyncInFilter_MultipleFilters(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncInFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "skip-a"
		}),
		WithSyncInFilter(func(item SyncStoreItem, peerID string) bool {
			return item.Key != "skip-b"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{Key: "skip-a", Value: "a", Timestamp: time.Now().UnixNano(), ID: "id-a"},
			{Key: "skip-b", Value: "b", Timestamp: time.Now().UnixNano(), ID: "id-b"},
			{Key: "keep", Value: "c", Timestamp: time.Now().UnixNano(), ID: "id-c"},
		},
	}

	if err := ss.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	// Only "keep" should be persisted
	val, err := ss.Get(ctx, "keep")
	if err != nil {
		t.Fatalf("expected 'keep': %v", err)
	}
	if val != "c" {
		t.Fatalf("expected 'c', got %q", val)
	}

	if _, err := ss.Get(ctx, "skip-a"); err == nil {
		t.Fatal("skip-a should not be persisted")
	}
	if _, err := ss.Get(ctx, "skip-b"); err == nil {
		t.Fatal("skip-b should not be persisted")
	}
}

func TestSyncInFilter_PeerIDPassedCorrectly(t *testing.T) {
	store := newMemStore()
	var capturedPeerID string
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithSyncInFilter(func(item SyncStoreItem, peerID string) bool {
			capturedPeerID = peerID
			return true
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{Key: "k", Value: "v", Timestamp: time.Now().UnixNano(), ID: "some-id"},
		},
	}

	if err := ss.SyncIn(ctx, "my-peer-42", payload); err != nil {
		t.Fatal(err)
	}

	if capturedPeerID != "my-peer-42" {
		t.Fatalf("expected peerID 'my-peer-42', got %q", capturedPeerID)
	}
}

func TestPostSyncOut_Called(t *testing.T) {
	store := newMemStore()
	var calledWith []SyncStoreItem
	var calledPeerID string
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPostSyncOut(func(ctx context.Context, items []SyncStoreItem, peerID string) {
			calledWith = items
			calledPeerID = peerID
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "k1", "v1"); err != nil {
		t.Fatal(err)
	}

	payload, err := ss.SyncOut(ctx, "target-peer", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.AckSyncOut(ctx, "target-peer", payload); err != nil {
		t.Fatal(err)
	}

	if len(calledWith) != 1 {
		t.Fatalf("expected PostSyncOut called with 1 item, got %d", len(calledWith))
	}
	if calledWith[0].Key != "k1" {
		t.Fatalf("expected key 'k1', got %q", calledWith[0].Key)
	}
	if calledPeerID != "target-peer" {
		t.Fatalf("expected peerID 'target-peer', got %q", calledPeerID)
	}
}

func TestPostSyncOut_NotCalledWhenEmpty(t *testing.T) {
	store := newMemStore()
	called := false
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPostSyncOut(func(ctx context.Context, items []SyncStoreItem, peerID string) {
			called = true
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	// No items written — SyncOut should not trigger PostSyncOut
	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.AckSyncOut(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	if called {
		t.Fatal("PostSyncOut should not be called when no items are sent")
	}
}

func TestPostSyncOut_MultipleCallbacks(t *testing.T) {
	store := newMemStore()
	var count1, count2 int
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPostSyncOut(func(ctx context.Context, items []SyncStoreItem, peerID string) {
			count1++
		}),
		WithPostSyncOut(func(ctx context.Context, items []SyncStoreItem, peerID string) {
			count2++
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "k", "v"); err != nil {
		t.Fatal(err)
	}

	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.AckSyncOut(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected both PostSyncOut callbacks called once, got %d and %d", count1, count2)
	}
}

func TestRawStore(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss.Close()

	raw := ss.RawStore()
	if raw != store {
		t.Fatal("RawStore should return the underlying store")
	}

	// Direct writes bypass sync layer
	ctx := context.Background()
	if err := raw.Set(ctx, "direct-key", "direct-val"); err != nil {
		t.Fatal(err)
	}
	val, err := raw.Get(ctx, "direct-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "direct-val" {
		t.Fatalf("expected 'direct-val', got %q", val)
	}

	// Not accessible via sync layer (no view key)
	_, err = ss.Get(ctx, "direct-key")
	if err == nil {
		t.Fatal("direct write should not be visible via sync layer")
	}
}

func TestNoHooks_DefaultBehaviorUnchanged(t *testing.T) {
	// Verify that a StoreSync with no hooks behaves identically to before
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "k1", "v1"); err != nil {
		t.Fatal(err)
	}

	// SyncOut should include all items
	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}

	// SyncIn should persist all items
	store2 := newMemStore()
	ss2 := NewWithOptions(store2, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss2.Close()

	if err := ss2.SyncIn(ctx, "peer1", *payload); err != nil {
		t.Fatal(err)
	}
	val, err := ss2.Get(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if val != "v1" {
		t.Fatalf("expected 'v1', got %q", val)
	}
}

// TestSyncOut_CursorSafety_MultiInstance verifies that the SyncOut cursor
// does NOT advance past items with future WriteTimestamps. This protects
// against a real scenario where multiple StoreSync instances share the same
// backing store (e.g. two server processes on the same database).
//
// The time offset pushes WriteTimestamp into the future (WT = now + offset).
// If a concurrent writer on another instance hasn't committed yet, a SyncOut
// scan on this instance could see a later item (higher WT), advance the
// cursor past the yet-to-be-committed item, and permanently skip it.
//
// Timeline:
//
//	Instance A: writes item-1 at WT_A (slow, hasn't committed yet)
//	Instance B: writes item-2 at WT_B > WT_A (fast, committed)
//	SyncOut:    scans queue, sees item-2 at WT_B
//	            if cursor advances to WT_B → item-1 (at WT_A < WT_B) is skipped forever
//	            if cursor stays behind WT_B → next SyncOut catches item-1 ✓
//
// We simulate this by writing directly to the raw store (bypassing writeMu),
// as a second StoreSync instance sharing the same store would.
func TestSyncOut_CursorSafety_MultiInstance(t *testing.T) {
	store := newTestStore()
	// Use a large offset so WriteTimestamps are well into the future.
	offset := int64(10 * time.Second)
	ss := New(store, offset)
	defer ss.Close()
	ctx := context.Background()

	// Instance B writes item-2 first (higher WT because it starts later).
	if err := ss.SetItem(ctx, "app", "item-2", "val-2"); err != nil {
		t.Fatal(err)
	}

	// SyncOut for "remote-peer": returns item-2. Cursor must NOT advance
	// past item-2's future WT because instance A hasn't committed item-1 yet.
	payload1, err := ss.SyncOut(ctx, "remote-peer", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload1.Items) != 1 || payload1.Items[0].Key != "item-2" {
		t.Fatalf("expected [item-2], got %v", payload1.Items)
	}
	if err := ss.AckSyncOut(ctx, "remote-peer", payload1); err != nil {
		t.Fatal(err)
	}

	// Simulate Instance A committing item-1 AFTER the SyncOut above.
	// In real life this is a second StoreSync on the same DB whose write
	// was in-flight during the scan. We write with a WT slightly before
	// item-2's WT to model the race.
	item1WT := payload1.Items[0].WriteTimestamp - 1 // just before item-2
	item1 := SyncStoreItem{
		App:            "app",
		Key:            "item-1",
		Value:          "val-1",
		Timestamp:      time.Now().UnixNano(),
		ID:             "late-writer-id",
		WriteTimestamp: item1WT,
	}
	// Write directly to the raw store as the other instance would.
	encoded, _ := json.Marshal(item1)
	queueID := QueueID(item1WT, item1.ID, item1.Key)
	store.Set(ctx, QueueKey(queueID), item1.ID)
	store.Set(ctx, ValueKey(item1.ID), string(encoded))
	store.Set(ctx, ViewKey(item1.Key), item1.ID)

	// Second SyncOut: must include item-1 because the cursor should NOT
	// have advanced past its WT (which is in the future). If the cursor
	// had advanced to item-2's WT, item-1 would be permanently lost.
	payload2, err := ss.SyncOut(ctx, "remote-peer", 100)
	if err != nil {
		t.Fatal(err)
	}

	// We expect to see both item-1 (newly committed) and item-2 (cursor
	// didn't advance past it because WT > now).
	keys := map[string]bool{}
	for _, item := range payload2.Items {
		keys[item.Key] = true
	}
	if !keys["item-1"] {
		t.Errorf("item-1 missing from second SyncOut — cursor advanced too far")
		t.Logf("got items: %v", payload2.Items)
	}
	if !keys["item-2"] {
		t.Errorf("item-2 missing from second SyncOut — should still be returned (WT in future)")
		t.Logf("got items: %v", payload2.Items)
	}
}

func TestGC_ChannelFull_NoDataLoss(t *testing.T) {
	// Verify that writes succeed even when the GC channel is full.
	// The GC channel has capacity 256. We flood it by writing more items
	// than the channel can hold, then verify all items are retrievable.
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss.Close()
	ctx := context.Background()

	const n = 300 // more than GC channel capacity (256)
	for i := range n {
		key := fmt.Sprintf("gc-key-%d", i)
		if err := ss.SetItem(ctx, "", key, fmt.Sprintf("val-%d", i)); err != nil {
			t.Fatalf("SetItem %d failed: %v", i, err)
		}
	}

	// All items should be retrievable despite GC channel overflow.
	for i := range n {
		key := fmt.Sprintf("gc-key-%d", i)
		item, err := ss.GetItem(ctx, key)
		if err != nil {
			t.Fatalf("GetItem %q failed: %v", key, err)
		}
		expected := fmt.Sprintf("val-%d", i)
		if item.Value != expected {
			t.Fatalf("key %q: expected %q, got %q", key, expected, item.Value)
		}
	}
}

func TestGC_ChannelFull_UpdatesStillWork(t *testing.T) {
	// When GC channel is full, updating a key should still work correctly
	// even though old values won't be cleaned up immediately.
	store := newMemStore()
	// Use a large offset so that rapid writes don't trigger the write-time-in-the-past check.
	ss := NewWithOptions(store, WithTimeOffset(int64(10*time.Second)))
	defer ss.Close()
	ctx := context.Background()

	// Fill the GC channel by writing many different keys.
	for i := range 260 {
		if err := ss.SetItem(ctx, "", fmt.Sprintf("filler-%d", i), "x"); err != nil {
			t.Fatalf("SetItem filler %d failed: %v", i, err)
		}
	}

	// Now update a single key multiple times. Each update should succeed
	// and return the latest value, regardless of GC backpressure.
	for i := range 10 {
		if err := ss.SetItem(ctx, "", "contested", fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("SetItem contested %d failed: %v", i, err)
		}
	}

	item, err := ss.GetItem(ctx, "contested")
	if err != nil {
		t.Fatalf("GetItem contested failed: %v", err)
	}
	if item.Value != "v9" {
		t.Fatalf("expected v9, got %q", item.Value)
	}
}

func TestSyncIn_RejectsFutureTimestamp(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				Key:       "future-key",
				Value:     "val",
				Timestamp: time.Now().UnixNano() + int64(time.Hour),
				ID:        "future-id",
			},
		},
	}

	err := ss.SyncIn(ctx, "peer1", payload)
	if err == nil {
		t.Fatal("expected error for item with future timestamp")
	}
}

func TestSyncIn_AcceptsPastTimestamp(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)))
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{
				Key:       "ancient-key",
				Value:     "val",
				Timestamp: 1000000, // ~1970
				ID:        "ancient-id",
			},
		},
	}

	err := ss.SyncIn(ctx, "peer1", payload)
	if err != nil {
		t.Fatalf("old-timestamp items should be accepted for offline-first sync, got: %v", err)
	}

	item, err := ss.GetItem(ctx, "ancient-key")
	if err != nil {
		t.Fatalf("expected item to be persisted, got: %v", err)
	}
	if item.Value != "val" {
		t.Fatalf("expected value 'val', got %q", item.Value)
	}
}

func TestOnSyncComplete_Hook(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var calls []struct {
		peer     string
		in, out  int
	}

	ss := NewWithOptions(newMemStore(),
		WithTimeOffset(int64(100*time.Millisecond)),
		WithOnSyncComplete(func(peerID string, itemsIn, itemsOut int) {
			mu.Lock()
			calls = append(calls, struct {
				peer     string
				in, out  int
			}{peerID, itemsIn, itemsOut})
			mu.Unlock()
		}),
	)
	defer ss.Close()

	// Write some items.
	if err := ss.SetItem(ctx, "app", "k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetItem(ctx, "app", "k2", "v2"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Sync with no incoming — should report items out.
	_, err := ss.Sync(ctx, "peer1", nil)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 OnSyncComplete call, got %d", len(calls))
	}
	if calls[0].peer != "peer1" {
		t.Fatalf("expected peer1, got %s", calls[0].peer)
	}
	if calls[0].in != 0 {
		t.Fatalf("expected 0 items in, got %d", calls[0].in)
	}
	if calls[0].out != 2 {
		t.Fatalf("expected 2 items out, got %d", calls[0].out)
	}
	mu.Unlock()
}

// TestProductionOffset verifies sync works correctly with the default 10-second
// offset. Items are included in SyncOut immediately, but the cursor does NOT
// advance past them until their WriteTimestamp has matured (current time >= WT).
// This ensures immature items are re-sent on subsequent SyncOut calls until the
// window closes, preventing missed items from concurrent writers.
func TestProductionOffset(t *testing.T) {
	store := newTestStore()
	ss := New(store) // uses default 10-second offset
	defer ss.Close()
	ctx := context.Background()

	// Write an item — its WriteTimestamp will be ~10s in the future.
	if err := ss.SetItem(ctx, "app", "key1", "value1"); err != nil {
		t.Fatal(err)
	}

	// SyncOut includes the item immediately (it's in the queue).
	payload, err := ss.SyncOut(ctx, "peer1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}

	// Cursor should NOT have advanced (WriteTimestamp is still in the future).
	// Ack and re-sync: the same item should appear again.
	if err := ss.AckSyncOut(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}
	payload2, err := ss.SyncOut(ctx, "peer1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload2.Items) != 1 {
		t.Fatalf("expected 1 item again (cursor not advanced past immature WT), got %d", len(payload2.Items))
	}

	// The item should be readable via Get (view layer is immediate).
	val, err := ss.Get(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if val != "value1" {
		t.Fatalf("expected value1, got %s", val)
	}
}

// TestWithMaxClockSkew verifies the WithMaxClockSkew option rejects items
// with timestamps beyond the configured skew.
func TestWithMaxClockSkew(t *testing.T) {
	store := newTestStore()
	ss := NewWithOptions(store, WithTimeOffset(int64(100*time.Millisecond)), WithMaxClockSkew(1*time.Second))
	defer ss.Close()
	ctx := context.Background()

	// A normal write should succeed.
	if err := ss.SetItem(ctx, "app", "key1", "value1"); err != nil {
		t.Fatal(err)
	}

	// A SyncIn item with a timestamp far in the future should be rejected.
	futureItem := SyncStoreItem{
		App:       "app",
		Key:       "key2",
		Value:     "value2",
		Timestamp: time.Now().UnixNano() + int64(10*time.Second), // 10s in the future
		ID:        "future-id",
	}
	err := ss.SyncIn(ctx, "peer1", SyncPayload{Items: []SyncStoreItem{futureItem}})
	if err == nil {
		t.Fatal("expected error for future timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "clock skew") {
		t.Fatalf("expected clock skew error, got: %v", err)
	}
}

// --- PreSetItem hook tests ---

func TestPreSetItem_ModifiesItem(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPreSetItem(func(item *SyncStoreItem) {
			item.PublicKey = "my-pub-key"
			item.Signature = "my-sig"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "app", "signed-key", "signed-val"); err != nil {
		t.Fatal(err)
	}

	item, err := ss.GetItem(ctx, "signed-key")
	if err != nil {
		t.Fatal(err)
	}
	if item.PublicKey != "my-pub-key" {
		t.Fatalf("expected PublicKey 'my-pub-key', got %q", item.PublicKey)
	}
	if item.Signature != "my-sig" {
		t.Fatalf("expected Signature 'my-sig', got %q", item.Signature)
	}
}

func TestPreSetItem_MultipleHooks_Chain(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPreSetItem(func(item *SyncStoreItem) {
			item.Value = item.Value + "-hook1"
		}),
		WithPreSetItem(func(item *SyncStoreItem) {
			item.Value = item.Value + "-hook2"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "", "chain", "base"); err != nil {
		t.Fatal(err)
	}

	item, err := ss.GetItem(ctx, "chain")
	if err != nil {
		t.Fatal(err)
	}
	if item.Value != "base-hook1-hook2" {
		t.Fatalf("expected 'base-hook1-hook2', got %q", item.Value)
	}
}

func TestPreSetItem_SyncInAlsoApplies(t *testing.T) {
	store := newMemStore()
	var hookCalled bool
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithPreSetItem(func(item *SyncStoreItem) {
			hookCalled = true
			item.Signature = "verified"
		}),
	)
	defer ss.Close()
	ctx := context.Background()

	payload := SyncPayload{
		Items: []SyncStoreItem{
			{Key: "remote", Value: "val", Timestamp: time.Now().UnixNano(), ID: "remote-id"},
		},
	}
	if err := ss.SyncIn(ctx, "peer1", payload); err != nil {
		t.Fatal(err)
	}

	if !hookCalled {
		t.Fatal("PreSetItem hook should be called during SyncIn")
	}
	item, err := ss.GetItem(ctx, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if item.Signature != "verified" {
		t.Fatalf("expected Signature 'verified', got %q", item.Signature)
	}
}

// --- WithLogger tests ---

// logCapture is a slog.Handler that captures log records.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *logCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *logCapture) WithGroup(_ string) slog.Handler      { return h }

func (h *logCapture) find(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return true
		}
	}
	return false
}

func TestWithLogger(t *testing.T) {
	store := newMemStore()
	capture := &logCapture{}
	logger := slog.New(capture)

	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithLogger(logger),
	)

	// Close should log "store sync closed"
	ss.Close()

	if !capture.find("store sync closed") {
		t.Fatal("expected 'store sync closed' log message from custom logger")
	}
}

func TestWithLogger_StaleWriteLogged(t *testing.T) {
	store := newMemStore()
	capture := &logCapture{}
	logger := slog.New(capture)

	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithLogger(logger),
	)
	defer ss.Close()
	ctx := context.Background()

	// Write an item with a future timestamp so the next SetItem is stale.
	futureItem := SyncStoreItem{
		App:       "app",
		Key:       "k",
		Value:     "future",
		Timestamp: time.Now().UnixNano() + int64(2*time.Minute),
		ID:        "future-id",
	}
	if err := ss.setItem(ctx, futureItem, ss.notifyListeners); err != nil {
		t.Fatal(err)
	}

	// This SetItem should be silently skipped (stale) and logged.
	if err := ss.SetItem(ctx, "app", "k", "older"); err != nil {
		t.Fatal(err)
	}

	if !capture.find("stale write skipped") {
		t.Fatal("expected 'stale write skipped' debug log")
	}
}

// --- WithGCWorkers tests ---

func TestWithGCWorkers_Disabled(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(100*time.Millisecond)),
		WithGCWorkers(0),
	)

	ctx := context.Background()
	// Writes should still work with GC disabled.
	if err := ss.SetItem(ctx, "", "k1", "v1"); err != nil {
		t.Fatal(err)
	}
	item, err := ss.GetItem(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Value != "v1" {
		t.Fatalf("expected v1, got %q", item.Value)
	}

	// Close should not hang.
	done := make(chan struct{})
	go func() {
		ss.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung with 0 GC workers")
	}
}

func TestWithGCWorkers_Custom(t *testing.T) {
	store := newMemStore()
	ss := NewWithOptions(store,
		WithTimeOffset(int64(10*time.Second)),
		WithGCWorkers(1),
	)
	defer ss.Close()
	ctx := context.Background()

	// Write first value, then overwrite. GC with 1 worker should still clean up.
	if err := ss.SetItem(ctx, "", "gk", "v1"); err != nil {
		t.Fatal(err)
	}
	item1, _ := ss.GetItem(ctx, "gk")
	firstID := item1.ID

	time.Sleep(time.Millisecond)
	if err := ss.SetItem(ctx, "", "gk", "v2"); err != nil {
		t.Fatal(err)
	}

	// Wait for GC.
	time.Sleep(200 * time.Millisecond)

	_, err := store.Get(ctx, ValueKey(firstID))
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Errorf("expected old value cleaned up by single GC worker, got err=%v", err)
	}
}

// --- Sync() method tests ---

func TestSync_InitiateWithNil(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	if err := ss.SetItem(ctx, "app", "k1", "v1"); err != nil {
		t.Fatal(err)
	}

	// Sync with nil incoming should return queued items.
	payload, err := ss.Sync(ctx, "peer1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload == nil || len(payload.Items) != 1 {
		t.Fatalf("expected 1 item from Sync(nil), got %v", payload)
	}
	if payload.Items[0].Key != "k1" {
		t.Fatalf("expected key k1, got %q", payload.Items[0].Key)
	}
}

func TestSync_EchoBackFiltering(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	ts := time.Now().UnixNano()
	incoming := &SyncPayload{
		Items: []SyncStoreItem{
			{Key: "echo-key", Value: "echo-val", Timestamp: ts, ID: "echo-id"},
		},
	}

	// Sync with incoming: the item should be written but NOT echoed back.
	payload, err := ss.Sync(ctx, "peer1", incoming)
	if err != nil {
		t.Fatal(err)
	}

	// The synced-in item should not appear in the response (echo-back filtering).
	if payload != nil {
		for _, item := range payload.Items {
			if item.ID == "echo-id" && item.Timestamp == ts {
				t.Fatal("item was echoed back to the sender — echo-back filtering failed")
			}
		}
	}

	// The item should still be persisted.
	item, err := ss.GetItem(ctx, "echo-key")
	if err != nil {
		t.Fatal(err)
	}
	if item.Value != "echo-val" {
		t.Fatalf("expected echo-val, got %q", item.Value)
	}
}

func TestSync_ReturnsNilWhenEmpty(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	payload, err := ss.Sync(ctx, "peer1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload when no items to send, got %+v", payload)
	}
}

func TestSync_FullExchange(t *testing.T) {
	store1 := newTestSyncStore()
	defer store1.Close()
	store2 := newTestSyncStore()
	defer store2.Close()
	ctx := context.Background()

	// Store1 writes data.
	if err := store1.SetItem(ctx, "app", "s1-key", "s1-val"); err != nil {
		t.Fatal(err)
	}
	// Store2 writes data.
	if err := store2.SetItem(ctx, "app", "s2-key", "s2-val"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond)

	// Store1 initiates sync: gets its own items to send.
	out1, err := store1.Sync(ctx, "store2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out1 == nil {
		t.Fatal("expected items from store1")
	}

	// Store2 receives store1's items and responds with its own.
	out2, err := store2.Sync(ctx, "store1", out1)
	if err != nil {
		t.Fatal(err)
	}

	// Store2 should now have store1's data.
	val, err := store2.Get(ctx, "s1-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "s1-val" {
		t.Fatalf("expected s1-val, got %q", val)
	}

	// Store1 receives store2's response.
	if out2 != nil {
		_, err = store1.Sync(ctx, "store2", out2)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Store1 should now have store2's data.
	val, err = store1.Get(ctx, "s2-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "s2-val" {
		t.Fatalf("expected s2-val, got %q", val)
	}
}

// --- New feature tests ---

func TestNotifyListeners_ConcurrentUnsubscribe(t *testing.T) {
	// Verify that unsubscribing from inside a listener callback doesn't
	// deadlock or panic, thanks to the snapshot pattern.
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	var unsub func()
	var called bool
	unsub = ss.OnUpdate(func(item SyncStoreItem) {
		called = true
		unsub() // unsubscribe from within the callback
	})

	if err := ss.SetItem(ctx, "", "k", "v"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("listener should have been called")
	}

	// Second write should not invoke the unsubscribed listener.
	called = false
	if err := ss.SetItem(ctx, "", "k2", "v2"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unsubscribed listener should not be called")
	}
}

func TestNotifyListeners_SubscribeFromCallback(t *testing.T) {
	// Verify that subscribing a new listener from within a callback
	// doesn't deadlock (the new listener won't fire for the current event).
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	var innerCalled bool
	ss.OnUpdate(func(item SyncStoreItem) {
		// Register a new listener from within the callback.
		ss.OnUpdate(func(item SyncStoreItem) {
			innerCalled = true
		})
	})

	if err := ss.SetItem(ctx, "", "k", "v"); err != nil {
		t.Fatal(err)
	}
	// The inner listener was registered during the first notification,
	// so it should fire on the next write.
	if err := ss.SetItem(ctx, "", "k2", "v2"); err != nil {
		t.Fatal(err)
	}
	if !innerCalled {
		t.Fatal("inner listener registered from callback should fire on subsequent writes")
	}
}

// failingStore wraps a memStore and can be configured to fail specific operations.
type failingStore struct {
	*memStore
	failValueKeySet bool
}

func (f *failingStore) Set(ctx context.Context, key, value string) error {
	if f.failValueKeySet && strings.HasPrefix(key, "%sync%value%") {
		return fmt.Errorf("injected ValueKey write failure")
	}
	return f.memStore.Set(ctx, key, value)
}

func TestSetItem_ValueKeyFailure_RestoresViewKey(t *testing.T) {
	inner := newMemStore()
	store := &failingStore{memStore: inner}
	ss := New(store, int64(100*time.Millisecond))
	defer ss.Close()
	ctx := context.Background()

	// Write an initial value successfully.
	if err := ss.SetItem(ctx, "", "restore-key", "original"); err != nil {
		t.Fatal(err)
	}

	// Now make ValueKey writes fail.
	store.failValueKeySet = true
	time.Sleep(time.Millisecond)

	// Attempt to overwrite — should fail.
	err := ss.SetItem(ctx, "", "restore-key", "overwrite")
	if err == nil {
		t.Fatal("expected error from failing ValueKey write")
	}

	// Re-enable writes to verify state.
	store.failValueKeySet = false

	// The original value should still be intact (ViewKey restored).
	val, getErr := ss.Get(ctx, "restore-key")
	if getErr != nil {
		t.Fatalf("Get after failed overwrite: %v", getErr)
	}
	if val != "original" {
		t.Fatalf("expected 'original' after rollback, got %q", val)
	}
}

func TestSetItem_ValueKeyFailure_NewKey_CleansUp(t *testing.T) {
	inner := newMemStore()
	store := &failingStore{memStore: inner}
	ss := New(store, int64(100*time.Millisecond))
	defer ss.Close()
	ctx := context.Background()

	// Make ValueKey writes fail from the start.
	store.failValueKeySet = true

	err := ss.SetItem(ctx, "", "new-fail-key", "val")
	if err == nil {
		t.Fatal("expected error from failing ValueKey write")
	}

	// Re-enable writes.
	store.failValueKeySet = false

	// The key should not exist (ViewKey was cleaned up).
	_, getErr := ss.Get(ctx, "new-fail-key")
	if !errors.Is(getErr, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cleaned-up key, got %v", getErr)
	}
}

func TestGcKey_RespectsContextCancellation(t *testing.T) {
	store := newMemStore()
	ss := New(store, int64(100*time.Millisecond))
	defer ss.Close()
	ctx := context.Background()

	// Write several items to create queue entries.
	for i := range 5 {
		if err := ss.SetItem(ctx, "", fmt.Sprintf("gc-ctx-key-%d", i), "v"); err != nil {
			t.Fatal(err)
		}
	}

	// Calling gcKey with a cancelled context should not panic and should
	// exit gracefully without fully completing (or at least not error).
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	// This should not panic or hang.
	ss.gcKey(cancelledCtx, "gc-ctx-key-0", "nonexistent-id")
}

// --- MaxSyncOutLimit test ---

func TestSyncOut_DefaultLimit(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	// Write items and verify SyncOut respects the default limit when 0 is passed.
	for i := range 5 {
		if err := ss.SetItem(ctx, "", fmt.Sprintf("limit-key-%d", i), "v"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	payload, err := ss.SyncOut(ctx, "peer1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 5 {
		t.Fatalf("expected 5 items with default limit, got %d", len(payload.Items))
	}
}

// --- Delete edge cases ---

func TestDelete_NonExistentKey(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	err := ss.Delete(ctx, "does-not-exist")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-existent key, got %v", err)
	}
}

func TestDelete_SyncsToRemote(t *testing.T) {
	store1 := newTestSyncStore()
	defer store1.Close()
	store2 := newTestSyncStore()
	defer store2.Close()
	ctx := context.Background()

	// Set and sync to store2.
	if err := store1.SetItem(ctx, "", "del-key", "val"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	p, err := store1.SyncOut(ctx, "store2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.SyncIn(ctx, "store1", *p); err != nil {
		t.Fatal(err)
	}
	if err := store1.AckSyncOut(ctx, "store2", p); err != nil {
		t.Fatal(err)
	}

	// Delete on store1.
	if err := store1.Delete(ctx, "del-key"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	// Sync the tombstone to store2.
	p2, err := store1.SyncOut(ctx, "store2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.SyncIn(ctx, "store1", *p2); err != nil {
		t.Fatal(err)
	}

	// store2 should see the key as deleted.
	_, err = store2.Get(ctx, "del-key")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after synced delete, got %v", err)
	}
}

// --- List through storemd.Store interface ---

func TestList_StoreInterface(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	for _, k := range []string{"list/a", "list/b", "list/c", "other"} {
		if err := ss.Set(ctx, k, "v-"+k); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	list, err := ss.List(ctx, storemd.ListArgs{Prefix: "list/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 items with prefix 'list/', got %d", len(list))
	}
}

func TestList_ExcludesDeleted(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	if err := ss.Set(ctx, "alive", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Set(ctx, "dead", "v2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := ss.Delete(ctx, "dead"); err != nil {
		t.Fatal(err)
	}

	list, err := ss.List(ctx, storemd.ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Key != "alive" {
		t.Fatalf("expected [alive], got %v", list)
	}
}

// --- SetIfNotExists edge case: reclaim tombstone ---

func TestSetIfNotExists_ReclaimsTombstone(t *testing.T) {
	ss := newTestSyncStore()
	defer ss.Close()
	ctx := context.Background()

	// Create and delete a key to leave a tombstone.
	if err := ss.Set(ctx, "tomb-key", "old"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := ss.Delete(ctx, "tomb-key"); err != nil {
		t.Fatal(err)
	}

	// SetIfNotExists should detect the tombstone and reclaim it via SetItem.
	ok, err := ss.SetIfNotExists(ctx, "tomb-key", "new")
	if err != nil {
		t.Fatalf("SetIfNotExists on tombstone: %v", err)
	}
	if !ok {
		t.Fatal("expected true when reclaiming a tombstoned key")
	}

	val, err := ss.Get(ctx, "tomb-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "new" {
		t.Fatalf("expected 'new', got %q", val)
	}
}
