package fingerprintsync

import (
	"path/filepath"
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/bbolt"
	"github.com/readmedotmd/store.md/sync"
)

func newTestStore(t *testing.T) storemd.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := bbolt.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create bbolt store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestFingerprintStore(t *testing.T) *StoreFingerprint {
	t.Helper()
	return New(newTestStore(t), int64(100*time.Millisecond))
}

// --- Store interface compliance ---

func TestStoreFingerprintImplementsStoreInterface(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		return newTestFingerprintStore(t)
	})
}

// --- SyncStore item management ---

func TestSetAndGetItem(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("myapp", "greeting", "hello"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	item, err := s.GetItem("greeting")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
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

func TestGetItem_NotFound(t *testing.T) {
	s := newTestFingerprintStore(t)

	_, err := s.GetItem("nonexistent")
	if err != storemd.NotFoundError {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestSetItem_Overwrite(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "value1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key1", "value2"); err != nil {
		t.Fatalf("SetItem overwrite failed: %v", err)
	}

	item, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "value2" {
		t.Fatalf("expected %q, got %q", "value2", item.Value)
	}
}

func TestSetItem_OlderTimestampIgnored(t *testing.T) {
	s := newTestFingerprintStore(t)

	futureItem := sync.SyncStoreItem{
		App:       "app",
		Key:       "key1",
		Value:     "future",
		Timestamp: time.Now().UnixNano() + int64(time.Hour),
		ID:        "future-id",
	}
	if err := s.setItem(futureItem); err != nil {
		t.Fatalf("set future item failed: %v", err)
	}

	if err := s.SetItem("app", "key1", "older"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	item, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "future" {
		t.Fatalf("expected older write to be ignored, got value %q", item.Value)
	}
}

func TestListItems(t *testing.T) {
	s := newTestFingerprintStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val-"+k); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("", "", 0)
	if err != nil {
		t.Fatalf("ListItems failed: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestListItems_Prefix(t *testing.T) {
	s := newTestFingerprintStore(t)

	for _, k := range []string{"x/a", "x/b", "y/a"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("x/", "", 0)
	if err != nil {
		t.Fatalf("ListItems failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items with prefix x/, got %d", len(items))
	}
}

func TestListItems_Limit(t *testing.T) {
	s := newTestFingerprintStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	items, err := s.ListItems("", "", 2)
	if err != nil {
		t.Fatalf("ListItems failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

// --- SyncOut / SyncIn ---

func TestSyncOut_Empty(t *testing.T) {
	s := newTestFingerprintStore(t)

	payload, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_ReturnsItems(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	payload, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
}

func TestSyncOut_IncludesFingerprints(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	payload, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Fingerprints) != NumBuckets {
		t.Fatalf("expected %d fingerprint buckets, got %d", NumBuckets, len(payload.Fingerprints))
	}

	// At least one bucket should be non-zero since we have data.
	hasNonZero := false
	for _, v := range payload.Fingerprints {
		if v != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Fatal("expected at least one non-zero fingerprint bucket")
	}
}

func TestSyncOut_Incremental(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	payload1, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut 1 failed: %v", err)
	}
	if len(payload1.Items) != 1 {
		t.Fatalf("expected 1 item in first sync, got %d", len(payload1.Items))
	}

	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

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
	s := newTestFingerprintStore(t)

	for _, k := range []string{"a", "b", "c"} {
		if err := s.SetItem("app", k, "val"); err != nil {
			t.Fatalf("SetItem failed: %v", err)
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
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	p1, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 failed: %v", err)
	}
	if len(p1.Items) != 1 {
		t.Fatalf("expected 1 item for peer1, got %d", len(p1.Items))
	}

	p2, err := s.SyncOut("peer2", 0)
	if err != nil {
		t.Fatalf("SyncOut peer2 failed: %v", err)
	}
	if len(p2.Items) != 1 {
		t.Fatalf("expected 1 item for peer2, got %d", len(p2.Items))
	}

	p1again, err := s.SyncOut("peer1", 0)
	if err != nil {
		t.Fatalf("SyncOut peer1 again failed: %v", err)
	}
	if len(p1again.Items) != 0 {
		t.Fatalf("expected 0 items for peer1 second sync, got %d", len(p1again.Items))
	}
}

func TestSyncIn(t *testing.T) {
	s := newTestFingerprintStore(t)

	payload := sync.SyncPayload{
		Items: []sync.SyncStoreItem{
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

	item1, err := s.GetItem("key1")
	if err != nil {
		t.Fatalf("GetItem key1 failed: %v", err)
	}
	if item1.Value != "val1" {
		t.Fatalf("expected %q, got %q", "val1", item1.Value)
	}

	item2, err := s.GetItem("key2")
	if err != nil {
		t.Fatalf("GetItem key2 failed: %v", err)
	}
	if item2.Value != "val2" {
		t.Fatalf("expected %q, got %q", "val2", item2.Value)
	}
}

func TestSyncIn_TimestampConflict(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "local"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	payload := sync.SyncPayload{
		Items: []sync.SyncStoreItem{
			{
				App:       "app",
				Key:       "key1",
				Value:     "remote-old",
				Timestamp: 1,
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
		t.Fatalf("GetItem failed: %v", err)
	}
	if item.Value != "local" {
		t.Fatalf("expected local value to win, got %q", item.Value)
	}
}

func TestSyncRoundTrip(t *testing.T) {
	store1 := newTestFingerprintStore(t)
	store2 := newTestFingerprintStore(t)

	if err := store1.SetItem("app", "key1", "from-store1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	payload, err := store1.SyncOut("store2", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}

	if err := store2.SyncIn("store1", *payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	item, err := store2.GetItem("key1")
	if err != nil {
		t.Fatalf("GetItem from store2 failed: %v", err)
	}
	if item.Value != "from-store1" {
		t.Fatalf("expected %q, got %q", "from-store1", item.Value)
	}
}

// --- OnUpdate listeners ---

func TestOnUpdate_Set(t *testing.T) {
	s := newTestFingerprintStore(t)

	var received []sync.SyncStoreItem
	s.OnUpdate(func(item sync.SyncStoreItem) {
		received = append(received, item)
	})

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
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
	s := newTestFingerprintStore(t)

	var received []sync.SyncStoreItem
	s.OnUpdate(func(item sync.SyncStoreItem) {
		received = append(received, item)
	})

	payload := sync.SyncPayload{
		Items: []sync.SyncStoreItem{
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

func TestOnUpdate_Unsubscribe(t *testing.T) {
	s := newTestFingerprintStore(t)

	var count int
	unsub := s.OnUpdate(func(item sync.SyncStoreItem) {
		count++
	})

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 update, got %d", count)
	}

	unsub()

	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected still 1 update after unsubscribe, got %d", count)
	}
}

func TestOnUpdate_MultipleListeners(t *testing.T) {
	s := newTestFingerprintStore(t)

	var count1, count2 int
	s.OnUpdate(func(item sync.SyncStoreItem) { count1++ })
	s.OnUpdate(func(item sync.SyncStoreItem) { count2++ })

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected both listeners called once, got %d and %d", count1, count2)
	}
}

func TestOnUpdate_NotCalledForOlderTimestamp(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "current"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	var received []sync.SyncStoreItem
	s.OnUpdate(func(item sync.SyncStoreItem) {
		received = append(received, item)
	})

	payload := sync.SyncPayload{
		Items: []sync.SyncStoreItem{
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

// --- Fingerprint-specific tests ---

func TestComputeFingerprints_Empty(t *testing.T) {
	s := newTestFingerprintStore(t)

	fp, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints failed: %v", err)
	}

	for i, v := range fp.Buckets {
		if v != 0 {
			t.Fatalf("expected all zero buckets for empty store, bucket %d = %d", i, v)
		}
	}
}

func TestComputeFingerprints_NonEmpty(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	fp, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints failed: %v", err)
	}

	nonZero := 0
	for _, v := range fp.Buckets {
		if v != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatal("expected at least one non-zero bucket")
	}
}

func TestComputeFingerprints_Deterministic(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	fp1, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints 1 failed: %v", err)
	}

	fp2, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints 2 failed: %v", err)
	}

	for i := 0; i < NumBuckets; i++ {
		if fp1.Buckets[i] != fp2.Buckets[i] {
			t.Fatalf("fingerprints not deterministic: bucket %d differs (%d vs %d)", i, fp1.Buckets[i], fp2.Buckets[i])
		}
	}
}

func TestComputeFingerprints_ChangesOnUpdate(t *testing.T) {
	s := newTestFingerprintStore(t)

	if err := s.SetItem("app", "key1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	fp1, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints 1 failed: %v", err)
	}

	time.Sleep(time.Millisecond)
	if err := s.SetItem("app", "key1", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	fp2, err := s.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints 2 failed: %v", err)
	}

	// The bucket for key1 should differ since the value ID changed.
	bucket := bucketFor("key1")
	if fp1.Buckets[bucket] == fp2.Buckets[bucket] {
		t.Fatalf("expected bucket %d to change after update", bucket)
	}
}

func TestComputeFingerprints_IdenticalStores(t *testing.T) {
	s1 := newTestFingerprintStore(t)
	s2 := newTestFingerprintStore(t)

	// Write the same data to both stores via SyncIn so IDs match.
	items := []sync.SyncStoreItem{
		{App: "app", Key: "a", Value: "1", Timestamp: 100, ID: "id-a"},
		{App: "app", Key: "b", Value: "2", Timestamp: 200, ID: "id-b"},
		{App: "app", Key: "c", Value: "3", Timestamp: 300, ID: "id-c"},
	}

	payload := sync.SyncPayload{Items: items, LastSyncTimestamp: 0}
	if err := s1.SyncIn("test", payload); err != nil {
		t.Fatalf("SyncIn s1 failed: %v", err)
	}
	if err := s2.SyncIn("test", payload); err != nil {
		t.Fatalf("SyncIn s2 failed: %v", err)
	}

	fp1, err := s1.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s1 failed: %v", err)
	}
	fp2, err := s2.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s2 failed: %v", err)
	}

	diff := DiffBuckets(fp1, fp2)
	if len(diff) != 0 {
		t.Fatalf("expected identical fingerprints, got %d differing buckets", len(diff))
	}
}

func TestDiffBuckets_NoDiff(t *testing.T) {
	fp := &Fingerprints{}
	fp.Buckets[0] = 42
	fp.Buckets[100] = 99

	diff := DiffBuckets(fp, fp)
	if len(diff) != 0 {
		t.Fatalf("expected 0 diffs for same fingerprints, got %d", len(diff))
	}
}

func TestDiffBuckets_WithDiff(t *testing.T) {
	local := &Fingerprints{}
	remote := &Fingerprints{}

	local.Buckets[5] = 42
	remote.Buckets[5] = 99

	local.Buckets[200] = 1
	remote.Buckets[200] = 2

	diff := DiffBuckets(local, remote)
	if len(diff) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(diff))
	}

	diffSet := make(map[int]bool)
	for _, d := range diff {
		diffSet[d] = true
	}
	if !diffSet[5] || !diffSet[200] {
		t.Fatalf("expected buckets 5 and 200 in diff, got %v", diff)
	}
}

func TestItemsForBuckets_Empty(t *testing.T) {
	s := newTestFingerprintStore(t)

	items, err := s.ItemsForBuckets(nil)
	if err != nil {
		t.Fatalf("ItemsForBuckets failed: %v", err)
	}
	if items != nil {
		t.Fatalf("expected nil for empty bucket list, got %d items", len(items))
	}
}

func TestItemsForBuckets_FiltersCorrectly(t *testing.T) {
	s := newTestFingerprintStore(t)

	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	for _, k := range keys {
		if err := s.SetItem("app", k, "val-"+k); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	// Get the bucket for "alpha" and request items for that bucket.
	targetBucket := bucketFor("alpha")
	items, err := s.ItemsForBuckets([]int{targetBucket})
	if err != nil {
		t.Fatalf("ItemsForBuckets failed: %v", err)
	}

	// All returned items should hash to the target bucket.
	for _, item := range items {
		if bucketFor(item.Key) != targetBucket {
			t.Fatalf("item %q has bucket %d, expected %d", item.Key, bucketFor(item.Key), targetBucket)
		}
	}

	// "alpha" should definitely be among the results.
	found := false
	for _, item := range items {
		if item.Key == "alpha" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'alpha' in results for its bucket")
	}
}

func TestItemsForBuckets_MultipleBuckets(t *testing.T) {
	s := newTestFingerprintStore(t)

	keys := []string{"alpha", "beta", "gamma", "delta"}
	for _, k := range keys {
		if err := s.SetItem("app", k, "val-"+k); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	// Get buckets for all keys.
	bucketSet := make(map[int]bool)
	for _, k := range keys {
		bucketSet[bucketFor(k)] = true
	}
	var buckets []int
	for b := range bucketSet {
		buckets = append(buckets, b)
	}

	items, err := s.ItemsForBuckets(buckets)
	if err != nil {
		t.Fatalf("ItemsForBuckets failed: %v", err)
	}

	// Should get all items back (all keys belong to the requested buckets).
	if len(items) != len(keys) {
		t.Fatalf("expected %d items, got %d", len(keys), len(items))
	}
}

// --- Fingerprint reconciliation integration ---

func TestFingerprintReconciliation(t *testing.T) {
	s1 := newTestFingerprintStore(t)
	s2 := newTestFingerprintStore(t)

	// Write data only to s1 (simulating missed queue sync).
	for _, k := range []string{"a", "b", "c"} {
		if err := s1.SetItem("app", k, "val-"+k); err != nil {
			t.Fatalf("SetItem %q failed: %v", k, err)
		}
		time.Sleep(time.Millisecond)
	}

	// Compute fingerprints on both sides.
	fp1, err := s1.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s1 failed: %v", err)
	}
	fp2, err := s2.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s2 failed: %v", err)
	}

	// Find differing buckets.
	diff := DiffBuckets(fp1, fp2)
	if len(diff) == 0 {
		t.Fatal("expected differing buckets between non-empty and empty store")
	}

	// Get items from s1 for the differing buckets.
	items, err := s1.ItemsForBuckets(diff)
	if err != nil {
		t.Fatalf("ItemsForBuckets failed: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected items for differing buckets")
	}

	// SyncIn those items to s2.
	payload := sync.SyncPayload{Items: items}
	if err := s2.SyncIn("s1", payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// Verify fingerprints now match.
	fp1After, err := s1.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s1 after failed: %v", err)
	}
	fp2After, err := s2.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s2 after failed: %v", err)
	}

	diffAfter := DiffBuckets(fp1After, fp2After)
	if len(diffAfter) != 0 {
		t.Fatalf("expected 0 diffs after reconciliation, got %d", len(diffAfter))
	}

	// Verify s2 has all the data.
	for _, k := range []string{"a", "b", "c"} {
		item, err := s2.GetItem(k)
		if err != nil {
			t.Fatalf("GetItem %q from s2 failed: %v", k, err)
		}
		if item.Value != "val-"+k {
			t.Fatalf("expected %q, got %q", "val-"+k, item.Value)
		}
	}
}

func TestFingerprintReconciliation_BidirectionalDiff(t *testing.T) {
	s1 := newTestFingerprintStore(t)
	s2 := newTestFingerprintStore(t)

	// Write different data to each store.
	if err := s1.SetItem("app", "only-s1", "val1"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := s2.SetItem("app", "only-s2", "val2"); err != nil {
		t.Fatalf("SetItem failed: %v", err)
	}

	// Compute fingerprints.
	fp1, err := s1.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s1 failed: %v", err)
	}
	fp2, err := s2.ComputeFingerprints()
	if err != nil {
		t.Fatalf("ComputeFingerprints s2 failed: %v", err)
	}

	diff := DiffBuckets(fp1, fp2)
	if len(diff) == 0 {
		t.Fatal("expected diffs between stores with different data")
	}

	// Exchange items from both sides.
	items1, err := s1.ItemsForBuckets(diff)
	if err != nil {
		t.Fatalf("ItemsForBuckets s1 failed: %v", err)
	}
	items2, err := s2.ItemsForBuckets(diff)
	if err != nil {
		t.Fatalf("ItemsForBuckets s2 failed: %v", err)
	}

	if len(items1) > 0 {
		if err := s2.SyncIn("s1", sync.SyncPayload{Items: items1}); err != nil {
			t.Fatalf("SyncIn to s2 failed: %v", err)
		}
	}
	if len(items2) > 0 {
		if err := s1.SyncIn("s2", sync.SyncPayload{Items: items2}); err != nil {
			t.Fatalf("SyncIn to s1 failed: %v", err)
		}
	}

	// Both stores should now have both keys.
	for _, s := range []*StoreFingerprint{s1, s2} {
		item1, err := s.GetItem("only-s1")
		if err != nil {
			t.Fatalf("GetItem only-s1 failed: %v", err)
		}
		if item1.Value != "val1" {
			t.Fatalf("expected val1, got %q", item1.Value)
		}

		item2, err := s.GetItem("only-s2")
		if err != nil {
			t.Fatalf("GetItem only-s2 failed: %v", err)
		}
		if item2.Value != "val2" {
			t.Fatalf("expected val2, got %q", item2.Value)
		}
	}
}

func TestBucketFor_Deterministic(t *testing.T) {
	b1 := bucketFor("testkey")
	b2 := bucketFor("testkey")
	if b1 != b2 {
		t.Fatalf("bucketFor not deterministic: %d vs %d", b1, b2)
	}
	if b1 < 0 || b1 >= NumBuckets {
		t.Fatalf("bucket out of range: %d", b1)
	}
}

func TestItemHash_Deterministic(t *testing.T) {
	h1 := itemHash("key", "value-id")
	h2 := itemHash("key", "value-id")
	if h1 != h2 {
		t.Fatalf("itemHash not deterministic: %d vs %d", h1, h2)
	}
}

func TestItemHash_DifferentInputs(t *testing.T) {
	h1 := itemHash("key1", "id1")
	h2 := itemHash("key2", "id2")
	if h1 == h2 {
		t.Fatal("expected different hashes for different inputs")
	}
}
