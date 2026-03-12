package core

import (
	"context"
	"errors"
	"fmt"
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
	if err := s.setItem(futureItem); err != nil {
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
				Timestamp: 1, // very old timestamp
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
				Timestamp: 1,
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

	// Create an item with a timestamp far in the future.
	futureItem := SyncStoreItem{
		Key:       "future-key",
		Value:     "future-value",
		Timestamp: time.Now().UnixNano() + int64(10*time.Minute), // 10 min in future
		ID:        "future-id",
	}

	err := ss.setItem(futureItem)
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
	if err := ss.setItem(okItem); err != nil {
		t.Fatalf("expected no error for item within skew, got: %v", err)
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
