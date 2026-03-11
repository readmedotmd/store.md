package core

import (
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
)

func newTestStore(t *testing.T) storemd.Store {
	t.Helper()
	return memory.New()
}

func newTestSyncStore(t *testing.T) *StoreSync {
	t.Helper()
	return New(newTestStore(t), int64(100*time.Millisecond)) // small offset for tests
}

func TestUnderlyingStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		return newTestStore(t)
	})
}

func TestStoreSyncImplementsStoreInterface(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		return newTestSyncStore(t)
	})
}

func TestSetAndGet(t *testing.T) {
	s := newTestSyncStore(t)

	if err := s.SetItem("myapp", "greeting", "hello"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	item, err := s.GetItem("greeting")
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
	s := newTestSyncStore(t)

	_, err := s.GetItem("nonexistent")
	if err != storemd.NotFoundError {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestSet_Overwrite(t *testing.T) {
	s := newTestSyncStore(t)

	if err := s.SetItem("app", "key1", "value1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key1", "value2"); err != nil {
		t.Fatalf("Set overwrite failed: %v", err)
	}

	item, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "value2" {
		t.Fatalf("expected %q, got %q", "value2", item.Value)
	}
}

func TestSet_OlderTimestampIgnored(t *testing.T) {
	s := newTestSyncStore(t)

	// Set an item directly with a future timestamp
	futureItem := SyncStoreItem{
		App:       "app",
		Key:       "key1",
		Value:     "future",
		Timestamp: time.Now().UnixNano() + int64(time.Hour),
		ID:        "future-id",
	}
	if err := s.setItem(futureItem); err != nil {
		t.Fatalf("set future item failed: %v", err)
	}

	// Try to overwrite with a current-time item (older timestamp)
	if err := s.SetItem("app", "key1", "older"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	item, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "future" {
		t.Fatalf("expected older write to be ignored, got value %q", item.Value)
	}
}

func TestList(t *testing.T) {
	s := newTestSyncStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val-"+k); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("", "", 0)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestList_Prefix(t *testing.T) {
	s := newTestSyncStore(t)

	for _, k := range []string{"x/a", "x/b", "y/a"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("x/", "", 0)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items with prefix x/, got %d", len(items))
	}
}

func TestList_Limit(t *testing.T) {
	s := newTestSyncStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("Set %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("", "", 2)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestSyncOut_Empty(t *testing.T) {
	s := newTestSyncStore(t)

	payload, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_ReturnsItems(t *testing.T) {
	s := newTestSyncStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	payload, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_Incremental(t *testing.T) {
	s := newTestSyncStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Wait for writeTimestamp to be in the past
	time.Sleep(150 * time.Millisecond)

	// First sync out gets key1
	payload1, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut 1 failed: %v", err)
	}
	if len(payload1.Items) != 1 {
		t.Fatalf("expected 1 item in first sync, got %d", len(payload1.Items))
	}

	// Add another item
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Wait for writeTimestamp to be in the past
	time.Sleep(150 * time.Millisecond)

	// Second sync out should only get key2
	payload2, err := s.SyncOut("peer1", 0)
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
	s := newTestSyncStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	payload, err := s.SyncOut("peer1", 2)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_SeparatePerPeer(t *testing.T) {
	s := newTestSyncStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	// Sync out to peer1
	p1, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 failed: %v", err)
	}
	if len(p1.Items) != 1 {
		t.Fatalf("expected 1 item for peer1, got %d", len(p1.Items))
	}

	// Sync out to peer2 should also get key1 (independent cursor)
	p2, err := s.SyncOut("peer2", 0)
	if err != nil {
		t.Fatalf("SyncOut peer2 failed: %v", err)
	}
	if len(p2.Items) != 1 {
		t.Fatalf("expected 1 item for peer2, got %d", len(p2.Items))
	}

	// Sync out to peer1 again should get nothing
	p1again, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 again failed: %v", err)
	}
	if len(p1again.Items) != 0 {
		t.Fatalf("expected 0 items for peer1 second sync, got %d", len(p1again.Items))
	}
}

func TestSyncIn(t *testing.T) {
	s := newTestSyncStore(t)

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

	if err := s.SyncIn("peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify items are accessible via Get
	item1, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("Get key1 failed: %v", err)
	}
	if item1.Value != "val1" {
		t.Fatalf("expected %q, got %q", "val1", item1.Value)
	}

	item2, err := s.GetItem("key2")
	if err != nil {
		t.Fatalf("Get key2 failed: %v", err)
	}
	if item2.Value != "val2" {
		t.Fatalf("expected %q, got %q", "val2", item2.Value)
	}
}

func TestSyncIn_TimestampConflict(t *testing.T) {
	s := newTestSyncStore(t)

	// Set a local value
	if err := s.SetItem("app", "key1", "local"); err != nil {
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

	if err := s.SyncIn("peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	item, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if item.Value != "local" {
		t.Fatalf("expected local value to win, got %q", item.Value)
	}
}

func TestSyncIn_SeparateTimestampFromSyncOut(t *testing.T) {
	s := newTestSyncStore(t)

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
	if err := s.SyncIn("peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// SyncOut to peer1 should still return items (independent cursor)
	out, err := s.SyncOut("peer1", 0)
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
	store1 := newTestSyncStore(t)
	store2 := newTestSyncStore(t)

	// Store1 sets some data
	if err := store1.SetItem("app", "key1", "from-store1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// SyncOut from store1
	payload, err := store1.SyncOut("store2", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}

	// SyncIn to store2
	if err := store2.SyncIn("store1", *payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify store2 has the data
	item, err := store2.GetItem("key1")
	if err != nil {
		t.Fatalf("Get from store2 failed: %v", err)
	}
	if item.Value != "from-store1" {
		t.Fatalf("expected %q, got %q", "from-store1", item.Value)
	}
}

func TestOnUpdate_Set(t *testing.T) {
	s := newTestSyncStore(t)

	var received []SyncStoreItem
	s.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := s.SetItem("app", "key2", "val2"); err != nil {
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
	s := newTestSyncStore(t)

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
	if err := s.SyncIn("peer1", payload); err != nil {
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
	s := newTestSyncStore(t)

	if err := s.SetItem("app", "key1", "current"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	var received []SyncStoreItem
	s.OnUpdate(func(item SyncStoreItem) {
		received = append(received, item)
	})

	// SyncIn an older value — should be ignored, no listener call
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
	if err := s.SyncIn("peer1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	if len(received) != 0 {
		t.Fatalf("expected no updates for older timestamp, got %d", len(received))
	}
}

func TestOnUpdate_Unsubscribe(t *testing.T) {
	s := newTestSyncStore(t)

	var count int
	unsub := s.OnUpdate(func(item SyncStoreItem) {
		count++
	})

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 update, got %d", count)
	}

	unsub()

	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected still 1 update after unsubscribe, got %d", count)
	}
}

func TestOnUpdate_MultipleListeners(t *testing.T) {
	s := newTestSyncStore(t)

	var count1, count2 int
	s.OnUpdate(func(item SyncStoreItem) { count1++ })
	s.OnUpdate(func(item SyncStoreItem) { count2++ })

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected both listeners called once, got %d and %d", count1, count2)
	}
}
