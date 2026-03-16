// Package cache provides an optional in-memory caching layer for any [storemd.Store].
//
// StoreCache wraps any [storemd.Store] and transparently caches the results
// of read operations (Get, List) in process memory. Write operations (Set,
// Delete, SetIfNotExists) automatically invalidate affected cache entries.
//
// # Max size and eviction
//
// Each of the two cache maps (gets, lists) is independently bounded by a
// configurable maximum number of entries. The default limit is 1000 entries
// per map (2000 total). When a cache map is full, a random existing entry
// is evicted to make room. Random eviction is O(1) via Go's map iteration
// order and avoids the overhead of maintaining an LRU list.
//
// Configure the max size with [WithMaxSize]:
//
//	cached := cache.New(store, cache.WithMaxSize(500))   // 500 per map
//	cached := cache.New(store, cache.WithMaxSize(0))     // unbounded (no eviction)
//
// # Invalidation strategy
//
// The cache maintains two maps:
//
//   - gets:  key → value/error  (from Get)
//   - lists: serialized ListArgs → []KeyValuePair/error  (from List)
//
// When a key is written or deleted:
//
//  1. The key's Get entry is evicted.
//  2. Every cached List result is evicted if:
//     (a) the written key has the list's prefix (the key *could* appear in the
//     result), OR
//     (b) the written key appears in the cached result set (the key *was* in
//     the result and its value may have changed).
//
// This conservative approach ensures correctness at the cost of evicting some
// list results that may not actually be affected.
//
// # Negative caching
//
// ErrNotFound responses from Get are cached. This prevents repeated misses
// from hitting the underlying store. These entries are invalidated normally
// when the key is written.
//
// # Sync integration
//
// StoreCache implements [storemd.Store], not [core.SyncStore]. When wrapping
// a sync store, external invalidation can be wired via [StoreCache.InvalidateKey]:
//
//	ss := core.New(backend)
//	cached := cache.New(ss)
//	ss.OnUpdate(func(item core.SyncStoreItem) {
//	    cached.InvalidateKey(item.Key)
//	})
//
// # Thread safety
//
// StoreCache is safe for concurrent use. It uses a [sync.RWMutex] internally:
// cache reads acquire a read lock, cache writes and invalidations acquire a
// write lock.
//
// # Usage
//
// StoreCache is a drop-in wrapper — it implements [storemd.Store], so it can
// be used anywhere a Store is expected:
//
//	store := memory.New()
//	cached := cache.New(store)
//	defer cached.Close()
//
//	cached.Set(ctx, "key", "value")
//	v, _ := cached.Get(ctx, "key")  // populated from store
//	v, _ = cached.Get(ctx, "key")   // served from cache
//
// It also works with sync stores (which implement storemd.Store):
//
//	ss := core.New(backend)
//	cached := cache.New(ss)
//
// The cache can be bypassed by accessing the underlying store directly:
//
//	v, _ := cached.Unwrap().Get(ctx, "key")
//
// # Limitations
//
//   - List invalidation is prefix-based: a write to "users/123" invalidates
//     all cached lists with prefix "users/", "users/1", "u", or "".
//   - The cache does not deduplicate concurrent fetches for the same key
//     (no request coalescing / singleflight). Both calls will hit the store
//     and the second write wins.
package cache

import (
	"context"
	"fmt"
	"strings"
	gosync "sync"

	storemd "github.com/readmedotmd/store.md"
)

// DefaultMaxSize is the default maximum number of entries per cache map.
// Each of the two maps (gets, lists) is bounded independently, so the total
// maximum entries is 2 × DefaultMaxSize. Set to 0 for unbounded caches.
const DefaultMaxSize = 1000

// Option configures a [StoreCache].
type Option func(*StoreCache)

// WithMaxSize sets the maximum number of entries per cache map. When a map
// is full, a random entry is evicted to make room for the new one. Set to 0
// to disable eviction (unbounded). Default is [DefaultMaxSize] (1000).
func WithMaxSize(n int) Option {
	return func(c *StoreCache) { c.maxSize = n }
}

// StoreCache wraps a [storemd.Store] and caches read results in memory.
// Write operations automatically invalidate affected entries. Each cache map
// is bounded by maxSize; when full, a random entry is evicted.
// See the package documentation for details on invalidation strategy and usage.
type StoreCache struct {
	store   storemd.Store
	maxSize int // max entries per map; 0 = unbounded

	mu    gosync.RWMutex
	gets  map[string]getCacheEntry  // key → cached Get result
	lists map[string]listCacheEntry // serialized ListArgs → cached List result
}

type getCacheEntry struct {
	value string
	err   error
}

type listCacheEntry struct {
	result []storemd.KeyValuePair
	err    error
}

// New creates a StoreCache wrapping the given Store.
//
// Options can be passed to configure behavior:
//
//	cached := cache.New(store, cache.WithMaxSize(500))
func New(store storemd.Store, opts ...Option) *StoreCache {
	c := &StoreCache{
		store:   store,
		maxSize: DefaultMaxSize,
		gets:    make(map[string]getCacheEntry),
		lists:   make(map[string]listCacheEntry),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Close releases the cache. It does not close the underlying Store.
func (c *StoreCache) Close() error {
	return nil
}

// Unwrap returns the underlying Store. Operations performed directly on
// the unwrapped store bypass the cache entirely — results won't be cached
// and writes won't trigger invalidation.
func (c *StoreCache) Unwrap() storemd.Store {
	return c.store
}

// MaxSize returns the configured maximum number of entries per cache map.
// Returns 0 if the cache is unbounded.
func (c *StoreCache) MaxSize() int {
	return c.maxSize
}

// --- Cache eviction ---

// evictOneGet removes one random entry from a gets map.
// Go's map iteration order is randomized, so ranging and breaking after the
// first key gives O(1) random eviction without maintaining an LRU list.
// Must be called with c.mu held for writing.
func evictOneGet(m map[string]getCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

// evictOneList removes one random entry from a lists map.
// Must be called with c.mu held for writing.
func evictOneList(m map[string]listCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

// --- Cache invalidation ---

// invalidateKey evicts all cache entries that could be affected by a write
// to the given key: the key's Get entry plus any List results whose prefix
// matches or whose result set contains the key.
func (c *StoreCache) invalidateKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.gets, key)
	c.invalidateListsLocked(key)
}

// invalidateListsLocked removes cached list results that could be affected
// by a write to key. A list is evicted if:
//   - key has the list's prefix (the key could appear in the result), OR
//   - key appears in the cached result set (the key was in the result)
//
// Must be called with c.mu held for writing.
func (c *StoreCache) invalidateListsLocked(key string) {
	for k, entry := range c.lists {
		prefix, _ := parseListCacheKey(k)
		if strings.HasPrefix(key, prefix) {
			delete(c.lists, k)
			continue
		}
		for _, kv := range entry.result {
			if kv.Key == key {
				delete(c.lists, k)
				break
			}
		}
	}
}

// listCacheKey produces a deterministic cache key from ListArgs.
// Fields are separated by null bytes to avoid ambiguity.
func listCacheKey(args storemd.ListArgs) string {
	return fmt.Sprintf("%s\x00%s\x00%d", args.Prefix, args.StartAfter, args.Limit)
}

// parseListCacheKey extracts the prefix from a list cache key.
func parseListCacheKey(k string) (prefix, rest string) {
	i := strings.IndexByte(k, 0)
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}

// InvalidateAll clears the entire cache. This is useful as a manual escape
// hatch — for example, after a bulk import or when memory usage needs to
// be reclaimed.
func (c *StoreCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = make(map[string]getCacheEntry)
	c.lists = make(map[string]listCacheEntry)
}

// InvalidateKey manually evicts all cache entries affected by the given key.
// This is useful for external invalidation — for example, wiring up sync
// store update notifications:
//
//	ss.OnUpdate(func(item core.SyncStoreItem) {
//	    cached.InvalidateKey(item.Key)
//	})
func (c *StoreCache) InvalidateKey(key string) {
	c.invalidateKey(key)
}

// --- Store interface ---

// Get returns the value for key, serving from cache on a hit. Cache misses
// (including ErrNotFound) are stored so subsequent calls avoid the underlying
// store. The entry is automatically evicted when the key is written via Set,
// Delete, or [InvalidateKey]. If the gets map is at capacity, a random
// existing entry is evicted first.
func (c *StoreCache) Get(ctx context.Context, key string) (string, error) {
	c.mu.RLock()
	entry, ok := c.gets[key]
	c.mu.RUnlock()
	if ok {
		return entry.value, entry.err
	}

	value, err := c.store.Get(ctx, key)

	c.mu.Lock()
	if c.maxSize > 0 && len(c.gets) >= c.maxSize {
		evictOneGet(c.gets)
	}
	c.gets[key] = getCacheEntry{value: value, err: err}
	c.mu.Unlock()
	return value, err
}

// Set writes a value and invalidates the key's cache entries plus any list
// caches whose prefix matches. The invalidation happens after the underlying
// write succeeds; if the write fails the cache is left unchanged.
func (c *StoreCache) Set(ctx context.Context, key, value string) error {
	err := c.store.Set(ctx, key, value)
	if err == nil {
		c.invalidateKey(key)
	}
	return err
}

// SetIfNotExists writes the key only if it does not already exist. Cache
// invalidation only occurs when the key is actually created (created=true).
// If the key already exists, the cache is left unchanged.
func (c *StoreCache) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	created, err := c.store.SetIfNotExists(ctx, key, value)
	if err == nil && created {
		c.invalidateKey(key)
	}
	return created, err
}

// Delete removes the key and invalidates all related cache entries.
func (c *StoreCache) Delete(ctx context.Context, key string) error {
	err := c.store.Delete(ctx, key)
	if err == nil {
		c.invalidateKey(key)
	}
	return err
}

// List returns key-value pairs matching the given args, serving from cache
// on a hit. The cache key includes Prefix, StartAfter, and Limit, so
// different pagination windows are cached independently. Any write to a key
// that matches the list's prefix invalidates the cached result. If the lists
// map is at capacity, a random existing entry is evicted first.
func (c *StoreCache) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	ck := listCacheKey(args)

	c.mu.RLock()
	entry, ok := c.lists[ck]
	c.mu.RUnlock()
	if ok {
		return entry.result, entry.err
	}

	result, err := c.store.List(ctx, args)

	c.mu.Lock()
	if c.maxSize > 0 && len(c.lists) >= c.maxSize {
		evictOneList(c.lists)
	}
	c.lists[ck] = listCacheEntry{result: result, err: err}
	c.mu.Unlock()
	return result, err
}

var _ storemd.Store = (*StoreCache)(nil)
