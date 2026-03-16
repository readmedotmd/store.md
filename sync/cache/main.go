// Package cache provides an optional in-memory caching layer for SyncStore.
//
// StoreCache wraps any [core.SyncStore] and transparently caches the results
// of read operations (Get, GetItem, List, ListItems) in process memory.
// Write operations (Set, SetItem, Delete, SetIfNotExists) automatically
// invalidate affected cache entries, as do remote updates received via the
// sync protocol.
//
// # Max size and eviction
//
// Each of the four cache maps (gets, getItems, lists, listKeys) is
// independently bounded by a configurable maximum number of entries.
// The default limit is 1000 entries per map (4000 total across all maps).
// When a cache map is full, a random existing entry is evicted to make room.
// Random eviction is O(1) via Go's map iteration order and avoids the
// overhead of maintaining an LRU list.
//
// Configure the max size with [WithMaxSize]:
//
//	cached := cache.New(ss, cache.WithMaxSize(500))   // 500 per map
//	cached := cache.New(ss, cache.WithMaxSize(0))     // unbounded (no eviction)
//
// # Invalidation strategy
//
// The cache maintains four independent maps:
//
//   - gets:     key → value      (from Get)
//   - getItems: key → SyncStoreItem  (from GetItem)
//   - lists:    serialized ListArgs → []KeyValuePair  (from List)
//   - listKeys: serialized list params → []SyncStoreItem  (from ListItems)
//
// When a key is written or deleted:
//
//  1. The key's Get and GetItem entries are evicted.
//  2. Every cached List/ListItems result is evicted if:
//     (a) the written key has the list's prefix (the key *could* appear in the
//     result), OR
//     (b) the written key appears in the cached result set (the key *was* in
//     the result and its value may have changed).
//
// This conservative approach ensures correctness at the cost of evicting some
// list results that may not actually be affected. For write-heavy workloads
// with many distinct list prefixes, consider calling [StoreCache.InvalidateAll]
// periodically or restructuring key namespaces.
//
// # Negative caching
//
// ErrNotFound responses from Get and GetItem are cached. This prevents
// repeated misses from hitting the underlying store. These entries are
// invalidated normally when the key is written.
//
// # Sync integration
//
// StoreCache subscribes to the wrapped store's OnUpdate listener. When items
// arrive via the sync protocol (peer writes, SyncIn), the cache automatically
// invalidates the affected key. This means the cache stays consistent even
// when multiple nodes write to the same sync store.
//
// # Thread safety
//
// StoreCache is safe for concurrent use. It uses a [sync.RWMutex] internally:
// cache reads acquire a read lock, cache writes and invalidations acquire a
// write lock.
//
// # Usage
//
// StoreCache is a drop-in wrapper — it implements [core.SyncStore], so it can
// be used anywhere a SyncStore is expected:
//
//	ss := core.New(backend)
//	cached := cache.New(ss)
//	defer cached.Close()
//
//	// Use cached exactly like ss — reads are cached, writes invalidate.
//	cached.Set(ctx, "key", "value")
//	v, _ := cached.Get(ctx, "key")  // populated from store
//	v, _ = cached.Get(ctx, "key")   // served from cache
//
// With options:
//
//	cached := cache.New(ss, cache.WithMaxSize(5000))  // 5000 entries per map
//
// The cache can be bypassed for specific operations by accessing the
// underlying store directly, though this is generally not recommended:
//
//	// Bypass: read directly (result won't be cached)
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
	"github.com/readmedotmd/store.md/sync/core"
)

// DefaultMaxSize is the default maximum number of entries per cache map.
// Each of the four maps (gets, getItems, lists, listKeys) is bounded
// independently, so the total maximum entries is 4 × DefaultMaxSize.
// Set to 0 for unbounded caches.
const DefaultMaxSize = 1000

// Option configures a [StoreCache].
type Option func(*StoreCache)

// WithMaxSize sets the maximum number of entries per cache map. When a map
// is full, a random entry is evicted to make room for the new one. Set to 0
// to disable eviction (unbounded). Default is [DefaultMaxSize] (1000).
func WithMaxSize(n int) Option {
	return func(c *StoreCache) { c.maxSize = n }
}

// StoreCache wraps a [core.SyncStore] and caches read results in memory.
// Write operations and sync updates automatically invalidate affected entries.
// Each cache map is bounded by maxSize; when full, a random entry is evicted.
// See the package documentation for details on invalidation strategy and usage.
type StoreCache struct {
	ss      core.SyncStore
	maxSize int // max entries per map; 0 = unbounded

	mu       gosync.RWMutex
	gets     map[string]getCacheEntry      // key → cached Get result
	getItems map[string]getItemCacheEntry  // key → cached GetItem result
	lists    map[string]listCacheEntry     // serialized ListArgs → cached List result
	listKeys map[string]listItemCacheEntry // serialized list params → cached ListItems result

	unsub func()
}

type getCacheEntry struct {
	value string
	err   error
}

type getItemCacheEntry struct {
	item *core.SyncStoreItem
	err  error
}

type listCacheEntry struct {
	result []storemd.KeyValuePair
	err    error
}

type listItemCacheEntry struct {
	result []core.SyncStoreItem
	err    error
}

// New creates a StoreCache wrapping the given SyncStore. The cache subscribes
// to the store's OnUpdate notifications so that remote sync updates
// automatically invalidate stale entries. Call [StoreCache.Close] when done
// to unsubscribe the listener.
//
// Options can be passed to configure behavior:
//
//	cached := cache.New(ss, cache.WithMaxSize(500))
func New(ss core.SyncStore, opts ...Option) *StoreCache {
	c := &StoreCache{
		ss:       ss,
		maxSize:  DefaultMaxSize,
		gets:     make(map[string]getCacheEntry),
		getItems: make(map[string]getItemCacheEntry),
		lists:    make(map[string]listCacheEntry),
		listKeys: make(map[string]listItemCacheEntry),
	}
	for _, o := range opts {
		o(c)
	}
	c.unsub = ss.OnUpdate(c.onUpdate)
	return c
}

// Close unsubscribes the cache's OnUpdate listener. After Close, remote sync
// updates will no longer invalidate cache entries, but local writes through
// the StoreCache still invalidate normally. Close does not close the
// underlying SyncStore.
func (c *StoreCache) Close() error {
	if c.unsub != nil {
		c.unsub()
		c.unsub = nil
	}
	return nil
}

// Unwrap returns the underlying SyncStore. Operations performed directly on
// the unwrapped store bypass the cache entirely — results won't be cached
// and writes won't trigger invalidation (though OnUpdate will still fire
// if the listener is active).
func (c *StoreCache) Unwrap() core.SyncStore {
	return c.ss
}

// MaxSize returns the configured maximum number of entries per cache map.
// Returns 0 if the cache is unbounded.
func (c *StoreCache) MaxSize() int {
	return c.maxSize
}

// --- Cache eviction ---

// evictOneLocked removes one random entry from a map to make room.
// Go's map iteration order is randomized, so ranging and breaking after the
// first key gives O(1) random eviction without maintaining an LRU list.
// Must be called with c.mu held for writing.

func evictOneGet(m map[string]getCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

func evictOneGetItem(m map[string]getItemCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

func evictOneList(m map[string]listCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

func evictOneListItem(m map[string]listItemCacheEntry) {
	for k := range m {
		delete(m, k)
		return
	}
}

// --- Cache invalidation ---

// onUpdate is the OnUpdate callback registered with the underlying store.
// It invalidates the cache for the key that was written or synced.
func (c *StoreCache) onUpdate(item core.SyncStoreItem) {
	c.invalidateKey(item.Key)
}

// invalidateKey evicts all cache entries that could be affected by a write
// to the given key: the key's Get/GetItem entry plus any List/ListItems
// results whose prefix matches or whose result set contains the key.
func (c *StoreCache) invalidateKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.gets, key)
	delete(c.getItems, key)
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
	for k, entry := range c.listKeys {
		prefix, _ := parseListItemCacheKey(k)
		if strings.HasPrefix(key, prefix) {
			delete(c.listKeys, k)
			continue
		}
		for _, item := range entry.result {
			if item.Key == key {
				delete(c.listKeys, k)
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

// listItemCacheKey produces a deterministic cache key for ListItems params.
func listItemCacheKey(prefix, startAfter string, limit int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", prefix, startAfter, limit)
}

func parseListItemCacheKey(k string) (prefix, rest string) {
	return parseListCacheKey(k)
}

// InvalidateAll clears the entire cache. This is useful as a manual escape
// hatch — for example, after a bulk import or when memory usage needs to
// be reclaimed.
func (c *StoreCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = make(map[string]getCacheEntry)
	c.getItems = make(map[string]getItemCacheEntry)
	c.lists = make(map[string]listCacheEntry)
	c.listKeys = make(map[string]listItemCacheEntry)
}

// InvalidateKey manually evicts all cache entries affected by the given key.
// This is useful when you know external state has changed outside the cache
// wrapper (e.g., a direct write to the underlying store).
func (c *StoreCache) InvalidateKey(key string) {
	c.invalidateKey(key)
}

// --- Store interface ---

// Get returns the value for key, serving from cache on a hit. Cache misses
// (including ErrNotFound) are stored so subsequent calls avoid the underlying
// store. The entry is automatically evicted when the key is written via Set,
// SetItem, Delete, or arrives via sync. If the gets map is at capacity, a
// random existing entry is evicted first.
func (c *StoreCache) Get(ctx context.Context, key string) (string, error) {
	c.mu.RLock()
	entry, ok := c.gets[key]
	c.mu.RUnlock()
	if ok {
		return entry.value, entry.err
	}

	value, err := c.ss.Get(ctx, key)

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
	err := c.ss.Set(ctx, key, value)
	if err == nil {
		c.invalidateKey(key)
	}
	return err
}

// SetIfNotExists writes the key only if it does not already exist. Cache
// invalidation only occurs when the key is actually created (created=true).
// If the key already exists, the cache is left unchanged.
func (c *StoreCache) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	created, err := c.ss.SetIfNotExists(ctx, key, value)
	if err == nil && created {
		c.invalidateKey(key)
	}
	return created, err
}

// Delete removes the key (writes a tombstone in the sync layer) and
// invalidates all related cache entries.
func (c *StoreCache) Delete(ctx context.Context, key string) error {
	err := c.ss.Delete(ctx, key)
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

	result, err := c.ss.List(ctx, args)

	c.mu.Lock()
	if c.maxSize > 0 && len(c.lists) >= c.maxSize {
		evictOneList(c.lists)
	}
	c.lists[ck] = listCacheEntry{result: result, err: err}
	c.mu.Unlock()
	return result, err
}

// --- SyncStore methods ---

// GetItem returns the full SyncStoreItem for the given key, serving from
// cache on a hit. Like Get, ErrNotFound responses are cached. If the
// getItems map is at capacity, a random existing entry is evicted first.
func (c *StoreCache) GetItem(ctx context.Context, key string) (*core.SyncStoreItem, error) {
	c.mu.RLock()
	entry, ok := c.getItems[key]
	c.mu.RUnlock()
	if ok {
		return entry.item, entry.err
	}

	item, err := c.ss.GetItem(ctx, key)

	c.mu.Lock()
	if c.maxSize > 0 && len(c.getItems) >= c.maxSize {
		evictOneGetItem(c.getItems)
	}
	c.getItems[key] = getItemCacheEntry{item: item, err: err}
	c.mu.Unlock()
	return item, err
}

// SetItem writes a value with an app label and invalidates the key's cache
// entries plus any matching list caches.
func (c *StoreCache) SetItem(ctx context.Context, app, key, value string) error {
	err := c.ss.SetItem(ctx, app, key, value)
	if err == nil {
		c.invalidateKey(key)
	}
	return err
}

// ListItems returns SyncStoreItems matching the given prefix, serving from
// cache on a hit. Different prefix/startAfter/limit combinations are cached
// independently. If the listKeys map is at capacity, a random existing entry
// is evicted first.
func (c *StoreCache) ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]core.SyncStoreItem, error) {
	ck := listItemCacheKey(prefix, startAfter, limit)

	c.mu.RLock()
	entry, ok := c.listKeys[ck]
	c.mu.RUnlock()
	if ok {
		return entry.result, entry.err
	}

	result, err := c.ss.ListItems(ctx, prefix, startAfter, limit)

	c.mu.Lock()
	if c.maxSize > 0 && len(c.listKeys) >= c.maxSize {
		evictOneListItem(c.listKeys)
	}
	c.listKeys[ck] = listItemCacheEntry{result: result, err: err}
	c.mu.Unlock()
	return result, err
}

// OnUpdate delegates to the underlying store's OnUpdate. Listeners registered
// here receive notifications for all writes, including those that arrive via
// sync. Note: the cache's own invalidation listener is separate — registering
// your own listener does not affect cache behavior.
func (c *StoreCache) OnUpdate(fn core.UpdateListener) func() {
	return c.ss.OnUpdate(fn)
}

// Sync delegates to the underlying store's Sync. The sync protocol is not
// cached — payloads pass through directly. However, items received via
// SyncIn will trigger OnUpdate, which invalidates affected cache entries.
func (c *StoreCache) Sync(ctx context.Context, peerID string, incoming *core.SyncPayload) (*core.SyncPayload, error) {
	return c.ss.Sync(ctx, peerID, incoming)
}

var _ core.SyncStore = (*StoreCache)(nil)
