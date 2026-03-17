package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	storemd "github.com/readmedotmd/store.md"
)

// MaxSyncInItems is the maximum number of items allowed in a single SyncIn payload.
const MaxSyncInItems = 5000

// MaxClockSkew is the maximum allowed clock skew for item timestamps.
const MaxClockSkew = 5 * time.Minute

// ErrStaleWrite is returned when setItem skips a write because the existing
// item has a newer timestamp or the same ID (duplicate).
var ErrStaleWrite = errors.New("write skipped: existing item is newer or duplicate")

// UpdateListener is a callback invoked when an item is successfully written.
type UpdateListener func(item SyncStoreItem)

// Sync Hooks
//
// StoreSync supports three optional hooks that allow upper layers to customize
// sync behavior without modifying the core protocol. Hooks are registered via
// functional options (WithSyncOutFilter, WithSyncInFilter, WithPostSyncOut)
// and multiple hooks of the same type can be stacked — they are evaluated in
// registration order with short-circuit semantics for filters.
//
// These hooks enable patterns like:
//   - Access control: filter which items are sent to which peers
//   - Ephemeral messaging: skip persistence on the receiver, clean up after delivery
//   - Audit logging: observe what was synced to each peer
//
// See the sync/ephemeral package for a complete example of building a layer
// on top of these hooks.

// SyncOutFilter decides whether an item should be included in a SyncOut
// payload for a given peer. Return false to skip the item. The sync cursor
// advances regardless, so filtered items won't block subsequent items.
// Multiple filters are evaluated in order; the first false short-circuits.
type SyncOutFilter func(item SyncStoreItem, peerID string) bool

// SyncInFilter decides how an incoming item should be handled during SyncIn.
// Return true to persist normally via setItem. Return false to skip
// persistence — listeners registered via OnUpdate are still notified, so
// upper layers can handle the item in memory without writing to the store.
// Multiple filters are evaluated in order; the first false short-circuits.
type SyncInFilter func(item SyncStoreItem, peerID string) (persist bool)

// PostSyncOut is called after a successful SyncOut with the items that were
// included in the outgoing payload and the target peer ID. Use for cleanup
// (e.g., deleting ephemeral items after confirmed delivery) or audit logging.
// Multiple callbacks are all invoked in registration order.
type PostSyncOut func(ctx context.Context, items []SyncStoreItem, peerID string)

// PreSetItem is called before an item is written to the store.
// It can modify the item (e.g., add signatures) before storage.
// The item's Timestamp and ID are already set when this hook is called.
// Multiple hooks are called in order; each hook receives the (possibly modified)
// item from the previous hook.
type PreSetItem func(item *SyncStoreItem)

// OnSyncComplete is called after a Sync exchange finishes. It receives the
// peer ID, the number of items received (in), and the number of items sent (out).
// Multiple callbacks are invoked in registration order.
type OnSyncComplete func(peerID string, itemsIn, itemsOut int)

// SyncStoreItem represents a single item in the sync store. Each item has
// a logical key, a unique ID (UUID), a wall-clock Timestamp used for
// last-writer-wins conflict resolution, and a WriteTimestamp used for
// queue-based sync ordering.
type SyncStoreItem struct {
	App            string `json:"app"`
	Key            string `json:"key"`
	Value          string `json:"value"`
	Timestamp      int64  `json:"timestamp"`
	ID             string `json:"id"`
	WriteTimestamp int64  `json:"writeTimestamp"`
	Deleted        bool   `json:"deleted,omitempty"`
	// Optional: sender's public key for signature verification (access control plugins)
	PublicKey string `json:"publicKey,omitempty"`
	// Optional: signature of the item (access control plugins)
	Signature string `json:"signature,omitempty"`
}

// gcRequest is a request to garbage-collect old values for a key.
type gcRequest struct {
	key, currentID string
}

// Option configures a StoreSync.
type Option func(*StoreSync)

// WithTimeOffset sets the write-time offset in nanoseconds.
func WithTimeOffset(offset int64) Option {
	return func(s *StoreSync) { s.timeOffset = offset }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *StoreSync) { s.logger = l }
}

// WithSyncOutFilter adds a filter applied during SyncOut. If any filter
// returns false for an item, the item is excluded from the payload but the
// sync cursor still advances past it.
func WithSyncOutFilter(fn SyncOutFilter) Option {
	return func(s *StoreSync) { s.syncOutFilters = append(s.syncOutFilters, fn) }
}

// WithSyncInFilter adds a filter applied during SyncIn. If any filter
// returns false, the item is not persisted but listeners are still notified.
func WithSyncInFilter(fn SyncInFilter) Option {
	return func(s *StoreSync) { s.syncInFilters = append(s.syncInFilters, fn) }
}

// WithPostSyncOut adds a callback invoked after SyncOut with the items
// that were included in the outgoing payload.
func WithPostSyncOut(fn PostSyncOut) Option {
	return func(s *StoreSync) { s.postSyncOuts = append(s.postSyncOuts, fn) }
}

// WithPreSetItem adds a hook called before an item is written.
// Use this to modify items before storage (e.g., add signatures).
func WithPreSetItem(fn PreSetItem) Option {
	return func(s *StoreSync) { s.preSetItems = append(s.preSetItems, fn) }
}

// WithOnSyncComplete adds a callback invoked after each Sync exchange with
// the number of items received and sent. Use for observability or metrics.
func WithOnSyncComplete(fn OnSyncComplete) Option {
	return func(s *StoreSync) { s.onSyncCompletes = append(s.onSyncCompletes, fn) }
}

// WithGCWorkers sets the number of background GC workers (default 4).
// Set to 0 to disable background GC.
func WithGCWorkers(n int) Option {
	return func(s *StoreSync) { s.gcWorkers = n }
}

// WithMaxClockSkew sets the maximum allowed clock skew for item timestamps.
// Items with timestamps further in the future than this are rejected.
// Default is MaxClockSkew (5 minutes).
func WithMaxClockSkew(d time.Duration) Option {
	return func(s *StoreSync) { s.maxClockSkew = d }
}

// StoreSync wraps a key-value store with queue-based sync support.
// It tracks writes in a time-ordered queue, supports per-peer sync cursors,
// and resolves conflicts using last-writer-wins timestamps.
type StoreSync struct {
	store        storemd.Store
	timeOffset   int64 // nanoseconds
	maxClockSkew time.Duration
	mu           gosync.RWMutex
	listeners    map[uint64]UpdateListener
	nextListenID atomic.Uint64
	logger       *slog.Logger

	// Hooks
	syncOutFilters []SyncOutFilter
	syncInFilters  []SyncInFilter
	postSyncOuts   []PostSyncOut
	preSetItems    []PreSetItem
	onSyncCompletes []OnSyncComplete

	gcWorkers int // number of GC workers (default 4)

	// Bounded GC worker pool
	gcCh   chan gcRequest
	done   chan struct{}
	wg     gosync.WaitGroup
	ctx    context.Context    // cancelled on Close
	cancel context.CancelFunc
}

// New creates a StoreSync with the given store and optional write-time offset
// in nanoseconds. The offset pushes WriteTimestamp into the future to create a
// buffer window for concurrent writers sharing the same underlying store.
// Default offset is 10 seconds. Use NewWithOptions for additional configuration.
func New(store storemd.Store, timeOffset ...int64) *StoreSync {
	offset := int64(10_000_000_000) // 10 seconds
	if len(timeOffset) > 0 {
		offset = timeOffset[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &StoreSync{
		store:        store,
		timeOffset:   offset,
		maxClockSkew: MaxClockSkew,
		gcWorkers:    4,
		listeners:    make(map[uint64]UpdateListener),
		logger:       slog.Default(),
		gcCh:         make(chan gcRequest, 256),
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
	s.startGCWorkers(s.gcWorkers)
	return s
}

// NewWithOptions creates a StoreSync with functional options.
func NewWithOptions(store storemd.Store, opts ...Option) *StoreSync {
	ctx, cancel := context.WithCancel(context.Background())
	s := &StoreSync{
		store:        store,
		timeOffset:   10_000_000_000, // 10 seconds default
		maxClockSkew: MaxClockSkew,
		gcWorkers:    4,
		listeners:    make(map[uint64]UpdateListener),
		logger:       slog.Default(),
		gcCh:         make(chan gcRequest, 256),
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
	for _, o := range opts {
		o(s)
	}
	s.startGCWorkers(s.gcWorkers)
	return s
}

// startGCWorkers launches n goroutines that process gcRequests from the
// bounded gcCh channel. Each worker cleans up orphaned queue/value entries.
func (s *StoreSync) startGCWorkers(n int) {
	for i := range n {
		s.wg.Add(1)
		go func(id int) {
			defer s.wg.Done()
			s.logger.Debug("gc worker started", "worker", id)
			for {
				select {
				case <-s.done:
					s.logger.Debug("gc worker stopping", "worker", id)
					return
				case req, ok := <-s.gcCh:
					if !ok {
						return
					}
					s.gcKey(s.ctx, req.key, req.currentID)
				}
			}
		}(i)
	}
}

// RawStore returns the underlying store, bypassing the sync layer.
// Use with care — direct writes bypass conflict resolution and sync queuing.
func (s *StoreSync) RawStore() storemd.Store {
	return s.store
}

// Close stops GC workers and waits for them to finish.
func (s *StoreSync) Close() error {
	close(s.done)
	s.cancel()
	s.drainGC()
	s.wg.Wait()
	s.logger.Debug("store sync closed")
	return nil
}

// drainGC discards pending GC requests so workers can exit promptly.
func (s *StoreSync) drainGC() {
	for {
		select {
		case <-s.gcCh:
		default:
			return
		}
	}
}

// OnUpdate registers a listener that is called whenever an item is written
// (via Set or SyncIn). Returns an unsubscribe function.
func (s *StoreSync) OnUpdate(fn UpdateListener) func() {
	id := s.nextListenID.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

// notifyListeners snapshots the listener map under RLock and invokes each
// callback outside the lock, so listeners can safely call OnUpdate/unsubscribe.
func (s *StoreSync) notifyListeners(item SyncStoreItem) {
	s.mu.RLock()
	snapshot := make([]UpdateListener, 0, len(s.listeners))
	for _, fn := range s.listeners {
		snapshot = append(snapshot, fn)
	}
	s.mu.RUnlock()
	for _, fn := range snapshot {
		fn(item)
	}
}

// ViewKey returns the store key that maps a logical key to its current value ID.
func ViewKey(key string) string {
	return "%sync%view%" + key
}

// ValueKey returns the store key for a value entry, keyed by unique item ID.
func ValueKey(id string) string {
	return "%sync%value%" + id
}

// QueueKey returns the store key for a queue entry, keyed by queue ID.
func QueueKey(id string) string {
	return "%sync%queue%" + id
}

// QueueID builds the composite queue ID from writeTime, item ID, and key.
// The format is {writeTime}%{id}%{key}, which sorts lexicographically by time.
func QueueID(writeTime int64, id, key string) string {
	return fmt.Sprintf("%d%%%s%%%s", writeTime, id, key)
}

// LastSyncOutKey returns the store key for a peer's sync-out cursor.
func LastSyncOutKey(peerID string) string {
	return "%sync%lastsyncout%" + peerID
}

// Get implements storemd.Store. Returns the value string for the given key.
// Returns ErrNotFound if the key has been deleted (tombstoned).
func (s *StoreSync) Get(ctx context.Context, key string) (string, error) {
	item, err := s.getItem(ctx, key)
	if err != nil {
		return "", err
	}
	if item.Deleted {
		return "", storemd.ErrNotFound
	}
	return item.Value, nil
}

// Set implements storemd.Store. Writes a value with an empty app.
func (s *StoreSync) Set(ctx context.Context, key, value string) error {
	return s.SetItem(ctx, "", key, value)
}

// SetIfNotExists writes the key only if it does not already exist in the sync view.
// Returns true if the write succeeded (key was new), false if the key already existed.
// Only records the write in the sync operation log if the key was actually created.
//
// Atomicity is delegated to the underlying store's SetIfNotExists on the view key,
// so this is safe even when multiple StoreSync instances share the same database.
//
// Implementation note: value and queue entries are written speculatively before
// the atomic view key claim. If the claim fails, these entries are cleaned up
// immediately. In rare crash scenarios between the speculative write and cleanup,
// orphaned entries will be removed by the GC workers.
func (s *StoreSync) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	item := SyncStoreItem{
		App:       "",
		Key:       key,
		Value:     value,
		Timestamp: time.Now().UnixNano(),
		ID:        uuid.New().String(),
	}

	writeTime := time.Now().UnixNano() + s.timeOffset
	item.WriteTimestamp = writeTime

	encoded, err := json.Marshal(item)
	if err != nil {
		return false, err
	}

	// Write the value and queue entry first (these are keyed by unique ID, so no conflict).
	queueID := QueueID(writeTime, item.ID, item.Key)
	if err := s.store.Set(ctx, ValueKey(item.ID), string(encoded)); err != nil {
		return false, err
	}
	if err := s.store.Set(ctx, QueueKey(queueID), item.ID); err != nil {
		s.store.Delete(ctx, ValueKey(item.ID))
		return false, err
	}

	// Atomically claim the view key. The underlying store's SetIfNotExists
	// provides database-level atomicity (e.g. SET NX, INSERT ON CONFLICT DO NOTHING,
	// bbolt transaction), so this is safe across multiple processes.
	created, err := s.store.SetIfNotExists(ctx, ViewKey(key), item.ID)
	if err != nil {
		s.store.Delete(ctx, QueueKey(queueID))
		s.store.Delete(ctx, ValueKey(item.ID))
		return false, err
	}

	if !created {
		// View key already existed — another writer claimed it. Check if it's
		// a tombstone (deleted item); if so, we can't use SetIfNotExists to
		// reclaim it, so fall back to SetItem which overwrites.
		existing, getErr := s.getItem(ctx, key)
		if getErr == nil && existing.Deleted {
			s.store.Delete(ctx, QueueKey(queueID))
			s.store.Delete(ctx, ValueKey(item.ID))
			if err := s.SetItem(ctx, "", key, value); err != nil {
				return false, err
			}
			return true, nil
		}
		// Key genuinely exists and is not deleted. Clean up our speculative writes.
		s.store.Delete(ctx, QueueKey(queueID))
		s.store.Delete(ctx, ValueKey(item.ID))
		return false, nil
	}

	// Validate write time after performing writes — same check as setItem.
	// If the write took so long that writeTime is now in the past, a SyncOut cursor
	// may have already advanced past this entry, causing the item to be missed.
	if time.Now().UnixNano() > writeTime {
		// Roll back: remove all three keys so we don't leave partial state.
		s.store.Delete(ctx, ViewKey(key))
		s.store.Delete(ctx, QueueKey(queueID))
		s.store.Delete(ctx, ValueKey(item.ID))
		return false, fmt.Errorf("write time is in the past")
	}

	// Send GC request to bounded worker pool (non-blocking).
	select {
	case s.gcCh <- gcRequest{key: item.Key, currentID: item.ID}:
	default:
	}

	s.notifyListeners(item)
	return true, nil
}

// Delete implements storemd.Store. Writes a tombstone that syncs to all peers.
// Returns ErrNotFound if the key does not exist.
func (s *StoreSync) Delete(ctx context.Context, key string) error {
	existing, err := s.getItem(ctx, key)
	if err != nil {
		return err
	}
	if existing.Deleted {
		return storemd.ErrNotFound
	}
	item := SyncStoreItem{
		App:       existing.App,
		Key:       key,
		Value:     "",
		Timestamp: time.Now().UnixNano(),
		ID:        uuid.New().String(),
		Deleted:   true,
	}
	return s.setItem(ctx, item, s.notifyListeners)
}

// List implements storemd.Store. Lists keys through the sync view layer.
func (s *StoreSync) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	viewArgs := storemd.ListArgs{
		Prefix: ViewKey(args.Prefix),
		Limit:  args.Limit,
	}
	if args.StartAfter != "" {
		viewArgs.StartAfter = ViewKey(args.StartAfter)
	}

	list, err := s.store.List(ctx, viewArgs)
	if err != nil {
		return nil, err
	}

	result := make([]storemd.KeyValuePair, 0, len(list))
	for _, kv := range list {
		item, err := s.getValue(ctx, kv.Value)
		if err != nil {
			if errors.Is(err, storemd.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if item.Deleted {
			continue
		}
		result = append(result, storemd.KeyValuePair{Key: item.Key, Value: item.Value})
	}
	return result, nil
}

// GetItem returns the full SyncStoreItem for the given key.
// Returns ErrNotFound if the key has been deleted (tombstoned).
func (s *StoreSync) GetItem(ctx context.Context, key string) (*SyncStoreItem, error) {
	item, err := s.getItem(ctx, key)
	if err != nil {
		return nil, err
	}
	if item.Deleted {
		return nil, storemd.ErrNotFound
	}
	return item, nil
}

// getItem returns the raw SyncStoreItem including tombstones.
func (s *StoreSync) getItem(ctx context.Context, key string) (*SyncStoreItem, error) {
	valueID, err := s.store.Get(ctx, ViewKey(key))
	if err != nil {
		return nil, err
	}
	return s.getValue(ctx, valueID)
}

// getValue deserializes a SyncStoreItem from the store by its unique value ID.
func (s *StoreSync) getValue(ctx context.Context, id string) (*SyncStoreItem, error) {
	raw, err := s.store.Get(ctx, ValueKey(id))
	if err != nil {
		return nil, err
	}
	var item SyncStoreItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// ListItems returns SyncStoreItems matching the given prefix, pagination, and limit.
func (s *StoreSync) ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]SyncStoreItem, error) {
	args := storemd.ListArgs{
		Prefix: ViewKey(prefix),
		Limit:  limit,
	}
	if startAfter != "" {
		args.StartAfter = ViewKey(startAfter)
	}

	list, err := s.store.List(ctx, args)
	if err != nil {
		return nil, err
	}

	items := make([]SyncStoreItem, 0, len(list))
	for _, kv := range list {
		item, err := s.getValue(ctx, kv.Value)
		if err != nil {
			if errors.Is(err, storemd.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if item.Deleted {
			continue
		}
		items = append(items, *item)
	}

	return items, nil
}

// SetItem writes a value through the sync layer with app, key, and value.
func (s *StoreSync) SetItem(ctx context.Context, app, key, value string) error {
	item := SyncStoreItem{
		App:       app,
		Key:       key,
		Value:     value,
		Timestamp: time.Now().UnixNano(),
		ID:        uuid.New().String(),
	}
	err := s.setItem(ctx, item, s.notifyListeners)
	if errors.Is(err, ErrStaleWrite) {
		s.logger.Debug("stale write skipped", "key", key, "id", item.ID)
		return nil
	}
	return err
}

// setItem is the core write path. It validates timestamps, applies PreSetItem
// hooks, writes queue/view/value keys, validates write time (TOCTOU), and
// notifies listeners. Returns ErrStaleWrite if the existing item is newer.
func (s *StoreSync) setItem(ctx context.Context, item SyncStoreItem, notify func(SyncStoreItem)) error {
	writeTime := time.Now().UnixNano() + s.timeOffset

	// Reject items with timestamps too far in the future (clock skew protection).
	// Account for timeOffset since peers using the same offset will generate
	// timestamps relative to their own clock + offset.
	maxTimestamp := time.Now().UnixNano() + s.timeOffset + int64(s.maxClockSkew)
	if item.Timestamp > maxTimestamp {
		return fmt.Errorf("item timestamp too far in the future (clock skew exceeds %v)", s.maxClockSkew)
	}

	// Check if existing item is newer (use getItem to see tombstones too)
	existing, err := s.getItem(ctx, item.Key)
	if err != nil && !errors.Is(err, storemd.ErrNotFound) {
		return err
	}
	if existing != nil && (existing.Timestamp > item.Timestamp || existing.ID == item.ID) {
		return ErrStaleWrite
	}

	item.WriteTimestamp = writeTime
	queueID := QueueID(writeTime, item.ID, item.Key)

	// Apply PreSetItem hooks - allow modification before storage
	for _, hook := range s.preSetItems {
		hook(&item)
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}

	if err := s.store.Set(ctx, QueueKey(queueID), item.ID); err != nil {
		return err
	}
	if err := s.store.Set(ctx, ViewKey(item.Key), item.ID); err != nil {
		s.store.Delete(ctx, QueueKey(queueID))
		return err
	}
	if err := s.store.Set(ctx, ValueKey(item.ID), string(encoded)); err != nil {
		s.store.Delete(ctx, QueueKey(queueID))
		// Restore ViewKey to previous value instead of deleting it,
		// otherwise an existing key would disappear on a failed write.
		if existing != nil {
			s.store.Set(ctx, ViewKey(item.Key), existing.ID)
		} else {
			s.store.Delete(ctx, ViewKey(item.Key))
		}
		return err
	}

	// Validate write time after performing writes (we want TOCTOU, else we will miss sync items).
	// This MUST happen before cleaning up the old value, because if validation
	// fails we need to rollback the view key to the old value ID — which requires
	// the old value entry to still exist.
	if time.Now().UnixNano() > writeTime {
		s.store.Delete(ctx, QueueKey(queueID))
		s.store.Delete(ctx, ValueKey(item.ID))
		if existing != nil {
			s.store.Set(ctx, ViewKey(item.Key), existing.ID)
		} else {
			s.store.Delete(ctx, ViewKey(item.Key))
		}
		return fmt.Errorf("write time is in the past")
	}

	// Clean up the old value inline if it's being replaced.
	// Done after validation so rollback can restore the old value.
	if existing != nil && existing.ID != item.ID {
		s.store.Delete(ctx, ValueKey(existing.ID))
	}

	// Send GC request to bounded worker pool (non-blocking).
	select {
	case s.gcCh <- gcRequest{key: item.Key, currentID: item.ID}:
	default:
		s.logger.Warn("gc channel full, skipping cleanup", "key", item.Key)
	}

	if notify != nil {
		notify(item)
	}
	return nil
}

// gcKey scans for all queue entries referencing the given key and removes
// any that don't belong to the current value ID. This cleans up orphaned
// values and queue entries that may have been missed by previous writes.
func (s *StoreSync) gcKey(ctx context.Context, key, currentID string) {
	// Scan all queue entries to find ones referencing this key.
	// Queue keys are: %sync%queue%{writeTime}%{uuid}%{key}
	suffix := "%" + key
	var startAfter string
	for {
		// Check if we should stop.
		select {
		case <-s.done:
			return
		default:
		}

		list, err := s.store.List(ctx, storemd.ListArgs{
			Prefix:     QueueKey(""),
			StartAfter: startAfter,
			Limit:      100,
		})
		if err != nil || len(list) == 0 {
			break
		}
		for _, kv := range list {
			startAfter = kv.Key
			// Check if this queue entry references our key.
			// Strip the queue prefix to get the queue ID portion.
			queueID := kv.Key[len(QueueKey("")):]
			if len(queueID) < len(suffix) {
				continue
			}
			if queueID[len(queueID)-len(suffix):] != suffix {
				continue
			}
			// This queue entry is for our key. Remove it if it points to an old value.
			if kv.Value != currentID {
				s.store.Delete(ctx, kv.Key)
				s.store.Delete(ctx, ValueKey(kv.Value))
			}
		}
		if len(list) < 100 {
			break
		}
	}
}

// SyncPayload is the wire format for sync exchanges. Items contains the
// sync data, and LastSyncTimestamp tracks the cursor position for incremental sync.
type SyncPayload struct {
	Items            []SyncStoreItem `json:"items"`
	LastSyncTimestamp int64          `json:"lastSyncTimestamp"`
}

// SyncIn applies incoming items from a peer.
func (s *StoreSync) SyncIn(ctx context.Context, peerID string, payload SyncPayload) error {
	if len(payload.Items) > MaxSyncInItems {
		return fmt.Errorf("sync payload too large: %d items (max %d)", len(payload.Items), MaxSyncInItems)
	}
	for _, item := range payload.Items {
		persist := true
		for _, filter := range s.syncInFilters {
			if !filter(item, peerID) {
				persist = false
				break
			}
		}
		if persist {
			if err := s.setItem(ctx, item, s.notifyListeners); err != nil && !errors.Is(err, ErrStaleWrite) {
				return err
			}
		} else {
			s.notifyListeners(item)
		}
	}
	return nil
}

const MaxSyncOutLimit = 1000

// SyncOut returns queued items for a peer since its last sync cursor.
// The cursor is NOT advanced until AckSyncOut is called with the returned
// payload's LastSyncTimestamp, ensuring items are not lost if delivery fails.
func (s *StoreSync) SyncOut(ctx context.Context, peerID string, limit int) (*SyncPayload, error) {
	if limit <= 0 || limit > MaxSyncOutLimit {
		limit = MaxSyncOutLimit
	}

	var lastSyncTimestamp int64

	raw, err := s.store.Get(ctx, LastSyncOutKey(peerID))
	if err != nil && !errors.Is(err, storemd.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &lastSyncTimestamp); err != nil {
			return nil, err
		}
	}

	return s.syncOut(ctx, lastSyncTimestamp, limit, peerID)
}

// AckSyncOut advances the sync cursor for a peer after the client has
// confirmed receipt of the payload. Call this after successfully delivering
// the SyncOut payload to the peer. Also invokes any PostSyncOut hooks.
func (s *StoreSync) AckSyncOut(ctx context.Context, peerID string, payload *SyncPayload) error {
	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return err
	}
	if err := s.store.Set(ctx, LastSyncOutKey(peerID), string(encoded)); err != nil {
		return err
	}

	if len(payload.Items) > 0 {
		for _, fn := range s.postSyncOuts {
			fn(ctx, payload.Items, peerID)
		}
	}
	return nil
}

// syncOut scans the queue from the given timestamp cursor, collects items
// up to limit, applies SyncOutFilter hooks, and returns the payload.
// The cursor in the returned payload only advances past matured WriteTimestamps.
func (s *StoreSync) syncOut(ctx context.Context, timestamp int64, limit int, peerID string) (*SyncPayload, error) {
	now := time.Now().UnixNano()

	args := storemd.ListArgs{
		Prefix: QueueKey(""),
		// Use ~ instead of % as the suffix after the timestamp. Queue entries are
		// keyed as {writeTime}%{uuid}%{key}, so using % would produce a StartAfter
		// that is lexicographically before entries with the same timestamp (because
		// {ts}% < {ts}%{uuid}%...), causing them to be included again. The ~ char
		// sorts after all alphanumeric and % characters, ensuring we skip past all
		// entries with the given timestamp.
		StartAfter: QueueKey(fmt.Sprintf("%d~", timestamp)),
		Limit:      limit,
	}

	list, err := s.store.List(ctx, args)
	if err != nil {
		return nil, err
	}

	items := make([]SyncStoreItem, 0, len(list))
	lastSyncTimestamp := timestamp

	for _, kv := range list {
		item, err := s.getValue(ctx, kv.Value)
		if err != nil {
			if errors.Is(err, storemd.ErrNotFound) {
				continue
			}
			return nil, err
		}

		// Advance cursor only for items whose WriteTimestamp has matured
		// (i.e. is no longer in the future). The time offset pushes WT
		// into the future to create a buffer window: if multiple StoreSync
		// instances share the same underlying store, a concurrent writer
		// may not have committed yet. Keeping the cursor behind future-WT
		// items ensures they are re-scanned until the window closes,
		// preventing missed items.
		//
		// NOTE: all StoreSync instances sharing a store must use the same
		// timeOffset value, otherwise items written with a larger offset
		// may mature after items with a smaller offset, causing cursor
		// gaps that skip items.
		if item.WriteTimestamp > lastSyncTimestamp && item.WriteTimestamp <= now {
			lastSyncTimestamp = item.WriteTimestamp
		}

		// Apply SyncOut filters — skip item if any filter rejects it.
		filtered := false
		for _, filter := range s.syncOutFilters {
			if !filter(*item, peerID) {
				filtered = true
				break
			}
		}
		if filtered {
			continue
		}

		items = append(items, *item)
	}

	return &SyncPayload{
		Items:            items,
		LastSyncTimestamp: lastSyncTimestamp,
	}, nil
}

// Sync implements SyncStore. It exchanges data with a peer using queue-based sync.
// Call with nil to initiate (returns queued items via SyncOut).
// Call with a payload to process incoming items (SyncIn) and return queued items (SyncOut).
// Returns nil when there is nothing more to send.
func (s *StoreSync) Sync(ctx context.Context, peerID string, incoming *SyncPayload) (*SyncPayload, error) {
	type itemKey struct {
		id        string
		timestamp int64
	}
	var incomingKeys map[itemKey]struct{}

	if incoming != nil && len(incoming.Items) > 0 {
		// Build a set of incoming (ID, Timestamp) pairs so we can filter
		// them from the SyncOut response. Using both fields ensures that
		// items rewritten by hooks (same ID, different timestamp) are
		// still sent back to the peer.
		incomingKeys = make(map[itemKey]struct{}, len(incoming.Items))
		for _, item := range incoming.Items {
			incomingKeys[itemKey{id: item.ID, timestamp: item.Timestamp}] = struct{}{}
		}

		if err := s.SyncIn(ctx, peerID, *incoming); err != nil {
			return nil, err
		}
	}

	payload, err := s.SyncOut(ctx, peerID, MaxSyncOutLimit)
	if err != nil {
		return nil, err
	}

	// Filter out items that the peer just sent us — don't echo them back.
	if len(incomingKeys) > 0 && len(payload.Items) > 0 {
		filtered := payload.Items[:0]
		for _, item := range payload.Items {
			if _, echo := incomingKeys[itemKey{id: item.ID, timestamp: item.Timestamp}]; !echo {
				filtered = append(filtered, item)
			}
		}
		payload.Items = filtered
	}

	// NOTE: the cursor is NOT advanced here. The caller must call
	// AckSyncOut after the peer has confirmed receipt of the payload.
	// Advancing the cursor before delivery confirmation would cause
	// items to be lost if the delivery fails.

	itemsIn := 0
	if incoming != nil {
		itemsIn = len(incoming.Items)
	}
	itemsOut := len(payload.Items)
	for _, fn := range s.onSyncCompletes {
		fn(peerID, itemsIn, itemsOut)
	}

	if len(payload.Items) == 0 {
		return nil, nil
	}

	return payload, nil
}

// SyncStore is the interface for all sync store implementations.
// The Sync method drives the sync protocol.
type SyncStore interface {
	storemd.Store
	GetItem(ctx context.Context, key string) (*SyncStoreItem, error)
	SetItem(ctx context.Context, app, key, value string) error
	ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]SyncStoreItem, error)
	OnUpdate(fn UpdateListener) func()

	// Sync exchanges data with a peer.
	// nil incoming = initiate a sync exchange.
	// Non-nil incoming = process received data and prepare response.
	// Returns nil when the exchange is complete (no more data to send).
	// IMPORTANT: Sync does NOT advance the sync cursor. The caller must
	// call AckSyncOut after the peer has confirmed receipt of the payload.
	Sync(ctx context.Context, peerID string, incoming *SyncPayload) (*SyncPayload, error)

	// AckSyncOut advances the sync cursor for a peer after confirmed delivery.
	// Must be called after the peer has acknowledged receipt of the SyncOut payload.
	AckSyncOut(ctx context.Context, peerID string, payload *SyncPayload) error

	// Close stops background workers and releases resources.
	Close() error
}

var _ SyncStore = (*StoreSync)(nil)
