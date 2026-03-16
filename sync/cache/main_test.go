package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/core"
)

// setup creates a StoreCache backed by an in-memory sync store. Both are
// registered for cleanup via t.Cleanup. Returns the cache and the underlying
// StoreSync for direct manipulation in tests. Accepts optional Option values.
func setup(t *testing.T, opts ...Option) (*StoreCache, *core.StoreSync) {
	t.Helper()
	mem := memory.New()
	ss := core.New(mem)
	t.Cleanup(func() { ss.Close() })
	cached := New(ss, opts...)
	t.Cleanup(func() { cached.Close() })
	return cached, ss
}

// setupDetached creates a StoreCache with the OnUpdate listener already
// unsubscribed. This allows testing pure cache behavior without sync-driven
// invalidation interfering. Accepts optional Option values.
func setupDetached(t *testing.T, opts ...Option) (*StoreCache, *core.StoreSync) {
	t.Helper()
	cached, ss := setup(t, opts...)
	cached.Close() // unsubscribe OnUpdate
	return cached, ss
}

// ---------------------------------------------------------------------------
// Get caching
// ---------------------------------------------------------------------------

func TestGet_PopulatesCache(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "k1", "v1")

	// First call: cache miss → hits underlying store.
	v, err := cached.Get(ctx, "k1")
	if err != nil || v != "v1" {
		t.Fatalf("expected v1, got %q err=%v", v, err)
	}

	// Mutate underlying directly (no invalidation since detached).
	ss.Set(ctx, "k1", "changed")

	// Second call: cache hit → returns stale value.
	v, err = cached.Get(ctx, "k1")
	if err != nil || v != "v1" {
		t.Fatalf("expected cached v1, got %q err=%v", v, err)
	}
}

func TestGet_CachesNotFoundErrors(t *testing.T) {
	cached, _ := setupDetached(t)
	ctx := context.Background()

	// First miss.
	_, err := cached.Get(ctx, "nonexistent")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Second call should return the cached error without hitting the store.
	_, err = cached.Get(ctx, "nonexistent")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected cached ErrNotFound, got %v", err)
	}
}

func TestGet_NotFoundInvalidatedBySet(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	// Cache a not-found.
	_, err := cached.Get(ctx, "k1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Write the key — should invalidate the negative cache entry.
	cached.Set(ctx, "k1", "hello")

	v, err := cached.Get(ctx, "k1")
	if err != nil || v != "hello" {
		t.Fatalf("expected hello, got %q err=%v", v, err)
	}
}

// ---------------------------------------------------------------------------
// Set invalidation
// ---------------------------------------------------------------------------

func TestSet_InvalidatesGetCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	cached.Set(ctx, "k1", "v2")
	v, _ := cached.Get(ctx, "k1")
	if v != "v2" {
		t.Fatalf("expected v2 after Set, got %q", v)
	}
}

func TestSet_InvalidatesGetItemCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.SetItem(ctx, "app", "k1", "v1")
	cached.GetItem(ctx, "k1") // populate

	cached.Set(ctx, "k1", "v2")
	item, err := cached.GetItem(ctx, "k1")
	if err != nil || item.Value != "v2" {
		t.Fatalf("expected v2, got %v err=%v", item, err)
	}
}

// ---------------------------------------------------------------------------
// Delete invalidation
// ---------------------------------------------------------------------------

func TestDelete_InvalidatesGetCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	cached.Delete(ctx, "k1")
	_, err := cached.Get(ctx, "k1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestDelete_InvalidatesListCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "ns/a", "1")
	cached.Set(ctx, "ns/b", "2")

	result, _ := cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}

	cached.Delete(ctx, "ns/a")

	result, _ = cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
	if len(result) != 1 {
		t.Fatalf("expected 1 after delete, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// SetIfNotExists
// ---------------------------------------------------------------------------

func TestSetIfNotExists_InvalidatesOnCreate(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	// Pre-populate list cache with empty result.
	cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})

	created, err := cached.SetIfNotExists(ctx, "ns/key", "val")
	if err != nil || !created {
		t.Fatalf("expected created=true, got %v err=%v", created, err)
	}

	result, _ := cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
	if len(result) != 1 {
		t.Fatalf("expected 1 item, got %d", len(result))
	}
}

func TestSetIfNotExists_NoInvalidateWhenKeyExists(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "k1", "original")
	cached.Get(ctx, "k1") // populate cache

	created, _ := cached.SetIfNotExists(ctx, "k1", "new")
	if created {
		t.Fatal("expected created=false")
	}

	// Cache should be unchanged.
	v, _ := cached.Get(ctx, "k1")
	if v != "original" {
		t.Fatalf("expected original, got %q", v)
	}
}

func TestSetIfNotExists_InvalidatesNegativeCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	// Cache a miss.
	_, err := cached.Get(ctx, "k1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	cached.SetIfNotExists(ctx, "k1", "hello")

	v, err := cached.Get(ctx, "k1")
	if err != nil || v != "hello" {
		t.Fatalf("expected hello, got %q err=%v", v, err)
	}
}

// ---------------------------------------------------------------------------
// SetItem invalidation
// ---------------------------------------------------------------------------

func TestSetItem_InvalidatesGetAndGetItem(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.SetItem(ctx, "app", "k1", "v1")
	cached.Get(ctx, "k1")
	cached.GetItem(ctx, "k1")

	cached.SetItem(ctx, "app", "k1", "v2")

	v, _ := cached.Get(ctx, "k1")
	if v != "v2" {
		t.Fatalf("Get: expected v2, got %q", v)
	}
	item, _ := cached.GetItem(ctx, "k1")
	if item.Value != "v2" {
		t.Fatalf("GetItem: expected v2, got %q", item.Value)
	}
}

// ---------------------------------------------------------------------------
// List caching
// ---------------------------------------------------------------------------

func TestList_CachesResult(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "a", "1")
	ss.Set(ctx, "b", "2")

	result, _ := cached.List(ctx, storemd.ListArgs{})
	count := len(result)

	// Add another via underlying (no invalidation).
	ss.Set(ctx, "c", "3")

	// Should return stale result.
	result2, _ := cached.List(ctx, storemd.ListArgs{})
	if len(result2) != count {
		t.Fatalf("expected cached %d items, got %d", count, len(result2))
	}
}

func TestList_DifferentArgsCachedSeparately(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "a/1", "v1")
	cached.Set(ctx, "b/1", "v2")

	rA, _ := cached.List(ctx, storemd.ListArgs{Prefix: "a/"})
	rB, _ := cached.List(ctx, storemd.ListArgs{Prefix: "b/"})

	if len(rA) != 1 || rA[0].Key != "a/1" {
		t.Fatalf("prefix a/ returned %v", rA)
	}
	if len(rB) != 1 || rB[0].Key != "b/1" {
		t.Fatalf("prefix b/ returned %v", rB)
	}
}

func TestList_PaginationCachedSeparately(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k/a", "1")
	cached.Set(ctx, "k/b", "2")
	cached.Set(ctx, "k/c", "3")

	page1, _ := cached.List(ctx, storemd.ListArgs{Prefix: "k/", Limit: 2})
	page2, _ := cached.List(ctx, storemd.ListArgs{Prefix: "k/", StartAfter: "k/b", Limit: 2})

	if len(page1) != 2 {
		t.Fatalf("page1 expected 2, got %d", len(page1))
	}
	if len(page2) != 1 {
		t.Fatalf("page2 expected 1, got %d", len(page2))
	}
}

func TestSet_InvalidatesListByPrefix(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "prefix/a", "1")
	cached.Set(ctx, "prefix/b", "2")

	cached.List(ctx, storemd.ListArgs{Prefix: "prefix/"})

	// Write new key with matching prefix.
	cached.Set(ctx, "prefix/c", "3")

	result, _ := cached.List(ctx, storemd.ListArgs{Prefix: "prefix/"})
	if len(result) != 3 {
		t.Fatalf("expected 3 items after Set, got %d", len(result))
	}
}

func TestSet_DoesNotInvalidateUnrelatedList(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "users/1", "alice")

	// Cache a list with prefix "users/"
	cached.List(ctx, storemd.ListArgs{Prefix: "users/"})

	// Write to a different prefix.
	ss.Set(ctx, "orders/1", "order")

	// "users/" cache should still be valid (detached, no OnUpdate).
	cached.mu.RLock()
	_, ok := cached.lists[listCacheKey(storemd.ListArgs{Prefix: "users/"})]
	cached.mu.RUnlock()
	if !ok {
		t.Fatal("users/ list cache should not have been invalidated by orders/ write")
	}
}

func TestSet_InvalidatesListContainingKey(t *testing.T) {
	cached, _ := setupDetached(t)
	ctx := context.Background()

	// Populate a list that has an empty prefix (matches everything).
	cached.ss.Set(ctx, "x", "1")
	cached.List(ctx, storemd.ListArgs{Prefix: "x"})

	// Write to "x" — should invalidate because "x" has prefix "x".
	cached.invalidateKey("x")

	cached.mu.RLock()
	_, ok := cached.lists[listCacheKey(storemd.ListArgs{Prefix: "x"})]
	cached.mu.RUnlock()
	if ok {
		t.Fatal("list with prefix 'x' should have been invalidated")
	}
}

// ---------------------------------------------------------------------------
// ListItems caching
// ---------------------------------------------------------------------------

func TestListItems_CachesResult(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.SetItem(ctx, "", "items/a", "1")
	ss.SetItem(ctx, "", "items/b", "2")

	items, err := cached.ListItems(ctx, "items/", "", 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 items, got %d err=%v", len(items), err)
	}

	ss.SetItem(ctx, "", "items/c", "3")

	// Stale cache.
	items, _ = cached.ListItems(ctx, "items/", "", 0)
	if len(items) != 2 {
		t.Fatalf("expected cached 2 items, got %d", len(items))
	}
}

func TestSetItem_InvalidatesListItems(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.SetItem(ctx, "", "ns/a", "1")
	cached.ListItems(ctx, "ns/", "", 0) // populate

	cached.SetItem(ctx, "", "ns/b", "2")

	items, _ := cached.ListItems(ctx, "ns/", "", 0)
	if len(items) != 2 {
		t.Fatalf("expected 2, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// GetItem caching
// ---------------------------------------------------------------------------

func TestGetItem_CachesResult(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.SetItem(ctx, "app", "k1", "v1")

	item, err := cached.GetItem(ctx, "k1")
	if err != nil || item.Value != "v1" {
		t.Fatalf("expected v1, got %v err=%v", item, err)
	}

	ss.SetItem(ctx, "app", "k1", "v2")

	// Stale cache.
	item, _ = cached.GetItem(ctx, "k1")
	if item.Value != "v1" {
		t.Fatalf("expected cached v1, got %q", item.Value)
	}
}

func TestGetItem_CachesNotFound(t *testing.T) {
	cached, _ := setupDetached(t)
	ctx := context.Background()

	_, err := cached.GetItem(ctx, "missing")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	_, err = cached.GetItem(ctx, "missing")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected cached ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// OnUpdate / sync-driven invalidation
// ---------------------------------------------------------------------------

func TestOnUpdate_InvalidatesGetCache(t *testing.T) {
	cached, ss := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	// Write directly to underlying — triggers OnUpdate → invalidation.
	ss.Set(ctx, "k1", "v2")

	v, _ := cached.Get(ctx, "k1")
	if v != "v2" {
		t.Fatalf("expected v2 after sync update, got %q", v)
	}
}

func TestOnUpdate_InvalidatesGetItemCache(t *testing.T) {
	cached, ss := setup(t)
	ctx := context.Background()

	cached.SetItem(ctx, "app", "k1", "v1")
	cached.GetItem(ctx, "k1") // populate

	ss.SetItem(ctx, "app", "k1", "v2")

	item, _ := cached.GetItem(ctx, "k1")
	if item.Value != "v2" {
		t.Fatalf("expected v2, got %q", item.Value)
	}
}

func TestOnUpdate_InvalidatesListCache(t *testing.T) {
	cached, ss := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "ns/a", "1")
	cached.List(ctx, storemd.ListArgs{Prefix: "ns/"}) // populate

	// Remote write
	ss.Set(ctx, "ns/b", "2")

	result, _ := cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
	if len(result) != 2 {
		t.Fatalf("expected 2 after sync, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// InvalidateAll
// ---------------------------------------------------------------------------

func TestInvalidateAll_ClearsAllMaps(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1")
	cached.GetItem(ctx, "k1")
	cached.Set(ctx, "ns/a", "1")
	cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
	cached.ListItems(ctx, "ns/", "", 0)

	cached.InvalidateAll()

	cached.mu.RLock()
	gLen := len(cached.gets)
	giLen := len(cached.getItems)
	lLen := len(cached.lists)
	liLen := len(cached.listKeys)
	cached.mu.RUnlock()

	if gLen != 0 || giLen != 0 || lLen != 0 || liLen != 0 {
		t.Fatalf("expected all maps empty, got gets=%d getItems=%d lists=%d listKeys=%d",
			gLen, giLen, lLen, liLen)
	}
}

func TestInvalidateAll_SubsequentGetHitsStore(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	// Still stale.
	v, _ := cached.Get(ctx, "k1")
	if v != "v1" {
		t.Fatalf("expected stale v1, got %q", v)
	}

	cached.InvalidateAll()

	// After InvalidateAll, the entry should be gone from the cache map.
	cached.mu.RLock()
	_, ok := cached.gets["k1"]
	cached.mu.RUnlock()
	if ok {
		t.Fatal("expected k1 evicted from cache after InvalidateAll")
	}

	// Next Get hits the store again (re-populates cache).
	v, err := cached.Get(ctx, "k1")
	if err != nil || v != "v1" {
		t.Fatalf("expected v1 from store, got %q err=%v", v, err)
	}
}

// ---------------------------------------------------------------------------
// InvalidateKey (manual)
// ---------------------------------------------------------------------------

func TestInvalidateKey_EvictsSpecificKey(t *testing.T) {
	cached, ss := setupDetached(t)
	ctx := context.Background()

	ss.Set(ctx, "k1", "v1")
	ss.Set(ctx, "k2", "v2")
	cached.Get(ctx, "k1")
	cached.Get(ctx, "k2")

	ss.Set(ctx, "k1", "changed")

	// k1 stale, k2 stale.
	v, _ := cached.Get(ctx, "k1")
	if v != "v1" {
		t.Fatalf("expected stale v1, got %q", v)
	}

	cached.InvalidateKey("k1")

	v, _ = cached.Get(ctx, "k1")
	if v != "changed" {
		t.Fatalf("expected changed after InvalidateKey, got %q", v)
	}

	// k2 still cached.
	v, _ = cached.Get(ctx, "k2")
	if v != "v2" {
		t.Fatalf("expected stale v2, got %q", v)
	}
}

// ---------------------------------------------------------------------------
// Unwrap
// ---------------------------------------------------------------------------

func TestUnwrap_ReturnsSameStore(t *testing.T) {
	cached, ss := setup(t)
	if cached.Unwrap() != core.SyncStore(ss) {
		t.Fatal("Unwrap should return the underlying SyncStore")
	}
}

func TestUnwrap_BypassesCache(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	// Read through Unwrap — should not affect cache.
	v, _ := cached.Unwrap().Get(ctx, "k1")
	if v != "v1" {
		t.Fatalf("expected v1 from Unwrap, got %q", v)
	}
}

// ---------------------------------------------------------------------------
// Close behavior
// ---------------------------------------------------------------------------

func TestClose_StopsAutoInvalidation(t *testing.T) {
	cached, ss := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	cached.Close()

	// Write to underlying — OnUpdate no longer fires.
	ss.Set(ctx, "k1", "v2")

	// Should return stale value.
	v, _ := cached.Get(ctx, "k1")
	if v != "v1" {
		t.Fatalf("expected stale v1 after Close, got %q", v)
	}
}

func TestClose_LocalWritesStillInvalidate(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")
	cached.Get(ctx, "k1") // populate

	cached.Close()

	// Write through cache wrapper — explicit invalidation in Set still works.
	cached.Set(ctx, "k1", "v2")

	// The get cache entry for "k1" should have been evicted by Set's
	// invalidateKey call, even though OnUpdate is no longer active.
	cached.mu.RLock()
	_, ok := cached.gets["k1"]
	cached.mu.RUnlock()
	if ok {
		t.Fatal("expected k1 to be evicted from get cache after Set")
	}
}

func TestClose_Idempotent(t *testing.T) {
	cached, _ := setup(t)
	if err := cached.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := cached.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sync pass-through
// ---------------------------------------------------------------------------

func TestSync_Delegates(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	_, err := cached.Sync(ctx, "peer1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OnUpdate pass-through
// ---------------------------------------------------------------------------

func TestOnUpdate_DelegatesToUnderlying(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	var got core.SyncStoreItem
	unsub := cached.OnUpdate(func(item core.SyncStoreItem) {
		got = item
	})
	defer unsub()

	cached.Set(ctx, "k1", "v1")
	if got.Key != "k1" {
		t.Fatalf("expected OnUpdate with key k1, got %q", got.Key)
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

func TestImplementsSyncStore(t *testing.T) {
	var _ core.SyncStore = (*StoreCache)(nil)
}

// ---------------------------------------------------------------------------
// Concurrency safety
// ---------------------------------------------------------------------------

func TestConcurrentReadWrite(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.Set(ctx, "k1", "v1")

	var wg sync.WaitGroup
	errs := make(chan error, 200)

	// 100 concurrent readers — NOT_FOUND is acceptable because concurrent
	// writers go through the sync layer which briefly tombstones during
	// conflict resolution.
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cached.Get(ctx, "k1")
			if err != nil && !errors.Is(err, storemd.ErrNotFound) {
				errs <- err
			}
		}()
	}

	// 100 concurrent writers.
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cached.Set(ctx, "k1", fmt.Sprintf("v%d", i))
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}
}

func TestConcurrentListAndWrite(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	for i := range 10 {
		cached.Set(ctx, fmt.Sprintf("ns/%d", i), "v")
	}

	var wg sync.WaitGroup

	// Concurrent list readers.
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cached.List(ctx, storemd.ListArgs{Prefix: "ns/"})
		}()
	}

	// Concurrent writers that invalidate the list.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cached.Set(ctx, fmt.Sprintf("ns/new%d", i), "v")
		}(i)
	}

	wg.Wait()
}

// ---------------------------------------------------------------------------
// List invalidation edge cases
// ---------------------------------------------------------------------------

func TestList_EmptyPrefixInvalidatedByAnyWrite(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	// Cache a list with empty prefix (matches everything).
	cached.List(ctx, storemd.ListArgs{})

	cached.Set(ctx, "anything", "v")

	// The empty-prefix list should be invalidated because "anything"
	// has prefix "" (everything does).
	cached.mu.RLock()
	_, ok := cached.lists[listCacheKey(storemd.ListArgs{})]
	cached.mu.RUnlock()
	if ok {
		t.Fatal("empty-prefix list should have been invalidated")
	}
}

func TestList_NestedPrefixInvalidation(t *testing.T) {
	// Use detached setup to avoid OnUpdate firing from intermediate Set calls
	// which would make it hard to reason about which lists get invalidated.
	cached, _ := setupDetached(t)
	ctx := context.Background()

	cached.ss.Set(ctx, "a/b/c", "1")

	// Cache lists at different prefix depths.
	cached.List(ctx, storemd.ListArgs{Prefix: "a/"})
	cached.List(ctx, storemd.ListArgs{Prefix: "a/b/"})
	cached.List(ctx, storemd.ListArgs{Prefix: "a/b/c"})

	// Simulate a write to "a/b/d" — should invalidate "a/" and "a/b/"
	// (because "a/b/d" has those prefixes) but NOT "a/b/c" (because
	// strings.HasPrefix("a/b/d", "a/b/c") is false).
	cached.invalidateKey("a/b/d")

	cached.mu.RLock()
	_, okA := cached.lists[listCacheKey(storemd.ListArgs{Prefix: "a/"})]
	_, okAB := cached.lists[listCacheKey(storemd.ListArgs{Prefix: "a/b/"})]
	_, okABC := cached.lists[listCacheKey(storemd.ListArgs{Prefix: "a/b/c"})]
	cached.mu.RUnlock()

	if okA {
		t.Fatal("a/ list should have been invalidated")
	}
	if okAB {
		t.Fatal("a/b/ list should have been invalidated")
	}
	if !okABC {
		t.Fatal("a/b/c list should NOT have been invalidated by a/b/d write")
	}
}

func TestDelete_InvalidatesListItems(t *testing.T) {
	cached, _ := setup(t)
	ctx := context.Background()

	cached.SetItem(ctx, "", "ns/a", "1")
	cached.SetItem(ctx, "", "ns/b", "2")
	cached.ListItems(ctx, "ns/", "", 0) // populate

	cached.Delete(ctx, "ns/a")

	items, _ := cached.ListItems(ctx, "ns/", "", 0)
	if len(items) != 1 {
		t.Fatalf("expected 1 after delete, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// MaxSize and eviction
// ---------------------------------------------------------------------------

func TestDefaultMaxSize(t *testing.T) {
	cached, _ := setup(t)
	if cached.MaxSize() != DefaultMaxSize {
		t.Fatalf("expected default max size %d, got %d", DefaultMaxSize, cached.MaxSize())
	}
}

func TestWithMaxSize_CustomValue(t *testing.T) {
	cached, _ := setup(t, WithMaxSize(42))
	if cached.MaxSize() != 42 {
		t.Fatalf("expected max size 42, got %d", cached.MaxSize())
	}
}

func TestWithMaxSize_Zero_Unbounded(t *testing.T) {
	cached, _ := setupDetached(t, WithMaxSize(0))
	ctx := context.Background()

	// Fill well beyond the default — should never evict.
	const count = 1500
	for i := range count {
		cached.ss.Set(ctx, fmt.Sprintf("k%d", i), "v")
	}
	for i := range count {
		cached.Get(ctx, fmt.Sprintf("k%d", i))
	}

	cached.mu.RLock()
	n := len(cached.gets)
	cached.mu.RUnlock()
	if n != count {
		t.Fatalf("expected %d cached entries (unbounded), got %d", count, n)
	}
}

func TestGet_EvictsWhenAtCapacity(t *testing.T) {
	cached, _ := setupDetached(t, WithMaxSize(3))
	ctx := context.Background()

	// Populate underlying store with 5 keys.
	for i := range 5 {
		cached.ss.Set(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}

	// Cache 3 entries to reach capacity.
	for i := range 3 {
		cached.Get(ctx, fmt.Sprintf("k%d", i))
	}
	cached.mu.RLock()
	if len(cached.gets) != 3 {
		t.Fatalf("expected 3 cached, got %d", len(cached.gets))
	}
	cached.mu.RUnlock()

	// Adding a 4th should evict one, keeping size at 3.
	cached.Get(ctx, "k3")
	cached.mu.RLock()
	n := len(cached.gets)
	cached.mu.RUnlock()
	if n != 3 {
		t.Fatalf("expected 3 after eviction, got %d", n)
	}
}

func TestGetItem_EvictsWhenAtCapacity(t *testing.T) {
	cached, _ := setupDetached(t, WithMaxSize(2))
	ctx := context.Background()

	for i := range 4 {
		cached.ss.SetItem(ctx, "", fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}

	// Fill to capacity.
	cached.GetItem(ctx, "k0")
	cached.GetItem(ctx, "k1")

	// One more — evicts one, stays at 2.
	cached.GetItem(ctx, "k2")
	cached.mu.RLock()
	n := len(cached.getItems)
	cached.mu.RUnlock()
	if n != 2 {
		t.Fatalf("expected 2 after eviction, got %d", n)
	}
}

func TestList_EvictsWhenAtCapacity(t *testing.T) {
	cached, _ := setupDetached(t, WithMaxSize(2))
	ctx := context.Background()

	cached.ss.Set(ctx, "a/1", "v")
	cached.ss.Set(ctx, "b/1", "v")
	cached.ss.Set(ctx, "c/1", "v")
	cached.ss.Set(ctx, "d/1", "v")

	// Fill to capacity with 2 different list queries.
	cached.List(ctx, storemd.ListArgs{Prefix: "a/"})
	cached.List(ctx, storemd.ListArgs{Prefix: "b/"})

	cached.mu.RLock()
	if len(cached.lists) != 2 {
		t.Fatalf("expected 2, got %d", len(cached.lists))
	}
	cached.mu.RUnlock()

	// Third list — evicts one, stays at 2.
	cached.List(ctx, storemd.ListArgs{Prefix: "c/"})
	cached.mu.RLock()
	n := len(cached.lists)
	cached.mu.RUnlock()
	if n != 2 {
		t.Fatalf("expected 2 after eviction, got %d", n)
	}
}

func TestListItems_EvictsWhenAtCapacity(t *testing.T) {
	cached, _ := setupDetached(t, WithMaxSize(2))
	ctx := context.Background()

	cached.ss.SetItem(ctx, "", "a/1", "v")
	cached.ss.SetItem(ctx, "", "b/1", "v")
	cached.ss.SetItem(ctx, "", "c/1", "v")

	cached.ListItems(ctx, "a/", "", 0)
	cached.ListItems(ctx, "b/", "", 0)

	// Third — evicts one, stays at 2.
	cached.ListItems(ctx, "c/", "", 0)
	cached.mu.RLock()
	n := len(cached.listKeys)
	cached.mu.RUnlock()
	if n != 2 {
		t.Fatalf("expected 2 after eviction, got %d", n)
	}
}

func TestEviction_DoesNotLoseNewEntry(t *testing.T) {
	// After eviction the newly inserted entry must be present.
	cached, _ := setupDetached(t, WithMaxSize(1))
	ctx := context.Background()

	cached.ss.Set(ctx, "k0", "v0")
	cached.ss.Set(ctx, "k1", "v1")

	cached.Get(ctx, "k0") // fills the single slot

	// This should evict k0 and insert k1.
	v, err := cached.Get(ctx, "k1")
	if err != nil || v != "v1" {
		t.Fatalf("expected v1, got %q err=%v", v, err)
	}

	// k1 must be cached.
	cached.mu.RLock()
	_, ok := cached.gets["k1"]
	cached.mu.RUnlock()
	if !ok {
		t.Fatal("newly inserted entry should be in cache after eviction")
	}
}

func TestEviction_MaxSizeOneStillWorks(t *testing.T) {
	// Edge case: maxSize=1 means the cache always holds at most one entry
	// per map, constantly evicting on every new insert.
	cached, _ := setupDetached(t, WithMaxSize(1))
	ctx := context.Background()

	for i := range 10 {
		cached.ss.Set(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}

	for i := range 10 {
		v, err := cached.Get(ctx, fmt.Sprintf("k%d", i))
		if err != nil || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("key k%d: expected v%d, got %q err=%v", i, i, v, err)
		}
	}

	// Cache should have exactly 1 entry (the last one read).
	cached.mu.RLock()
	n := len(cached.gets)
	cached.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expected 1 cached entry, got %d", n)
	}
}

func TestEviction_InvalidationReducesSizeBelowMax(t *testing.T) {
	// After invalidation frees space, the next insert should not evict.
	cached, _ := setupDetached(t, WithMaxSize(3))
	ctx := context.Background()

	for i := range 3 {
		cached.ss.Set(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
		cached.Get(ctx, fmt.Sprintf("k%d", i))
	}

	// Invalidate one key — size drops to 2.
	cached.InvalidateKey("k1")

	cached.mu.RLock()
	n := len(cached.gets)
	cached.mu.RUnlock()
	if n != 2 {
		t.Fatalf("expected 2 after invalidation, got %d", n)
	}

	// Insert a new key — should not evict since size < max.
	cached.ss.Set(ctx, "k3", "v3")
	cached.Get(ctx, "k3")

	cached.mu.RLock()
	n = len(cached.gets)
	cached.mu.RUnlock()
	if n != 3 {
		t.Fatalf("expected 3 (no eviction needed), got %d", n)
	}
}

func TestConcurrentEviction(t *testing.T) {
	// Verify no panics or data races under concurrent access with a small cache.
	cached, _ := setup(t, WithMaxSize(5))
	ctx := context.Background()

	// Pre-populate store with many keys.
	for i := range 100 {
		cached.ss.Set(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cached.Get(ctx, fmt.Sprintf("k%d", i))
		}(i)
	}
	wg.Wait()

	// Cache should not exceed maxSize.
	cached.mu.RLock()
	n := len(cached.gets)
	cached.mu.RUnlock()
	if n > 5 {
		t.Fatalf("expected at most 5 entries, got %d", n)
	}
}
