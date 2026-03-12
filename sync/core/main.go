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

// UpdateListener is a callback invoked when an item is successfully written.
type UpdateListener func(item SyncStoreItem)

type SyncStoreItem struct {
	App            string `json:"app"`
	Key            string `json:"key"`
	Value          string `json:"value"`
	Timestamp      int64  `json:"timestamp"`
	ID             string `json:"id"`
	WriteTimestamp int64  `json:"writeTimestamp"`
	Deleted        bool   `json:"deleted,omitempty"`
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

type StoreSync struct {
	store        storemd.Store
	timeOffset   int64 // nanoseconds
	writeMu      gosync.Mutex // serializes setItem to make conflict resolution atomic
	mu           gosync.RWMutex
	listeners    map[uint64]UpdateListener
	nextListenID atomic.Uint64
	logger       *slog.Logger

	// Bounded GC worker pool
	gcCh chan gcRequest
	done chan struct{}
	wg   gosync.WaitGroup
}

func New(store storemd.Store, timeOffset ...int64) *StoreSync {
	offset := int64(10_000_000_000) // 10 seconds
	if len(timeOffset) > 0 {
		offset = timeOffset[0]
	}
	s := &StoreSync{
		store:      store,
		timeOffset: offset,
		listeners:  make(map[uint64]UpdateListener),
		logger:     slog.Default(),
		gcCh:       make(chan gcRequest, 256),
		done:       make(chan struct{}),
	}
	s.startGCWorkers(4)
	return s
}

// NewWithOptions creates a StoreSync with functional options.
func NewWithOptions(store storemd.Store, opts ...Option) *StoreSync {
	s := &StoreSync{
		store:      store,
		timeOffset: 10_000_000_000, // 10 seconds default
		listeners:  make(map[uint64]UpdateListener),
		logger:     slog.Default(),
		gcCh:       make(chan gcRequest, 256),
		done:       make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	s.startGCWorkers(4)
	return s
}

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
					s.gcKey(req.key, req.currentID)
				}
			}
		}(i)
	}
}

// Close stops GC workers and waits for them to finish.
func (s *StoreSync) Close() error {
	close(s.done)
	// Drain remaining GC requests so workers can exit.
	for {
		select {
		case <-s.gcCh:
		default:
			goto drained
		}
	}
drained:
	s.wg.Wait()
	s.logger.Debug("store sync closed")
	return nil
}

// OnUpdate registers a listener that is called whenever an item is written
// (via Set or SyncIn). Returns an unsubscribe function.
func (s *StoreSync) OnUpdate(fn UpdateListener) func() {
	id := s.nextListenID.Add(1)
	s.mu.Lock()
	s.listeners[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.listeners, id)
		s.mu.Unlock()
	}
}

func (s *StoreSync) notifyListeners(item SyncStoreItem) {
	s.mu.RLock()
	listeners := make([]UpdateListener, 0, len(s.listeners))
	for _, fn := range s.listeners {
		listeners = append(listeners, fn)
	}
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(item)
	}
}

func ViewKey(key string) string {
	return "%sync%view%" + key
}

func ValueKey(id string) string {
	return "%sync%value%" + id
}

func QueueKey(id string) string {
	return "%sync%queue%" + id
}

func QueueID(writeTime int64, id, key string) string {
	return fmt.Sprintf("%d%%%s%%%s", writeTime, id, key)
}

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
	return s.setItem(item)
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
	return s.setItem(item)
}

func (s *StoreSync) setItem(item SyncStoreItem) error {
	current, err := s.setItemLocked(item)
	if err != nil || current == nil {
		return err
	}
	s.notifyListeners(*current)
	return nil
}

func (s *StoreSync) setItemLocked(item SyncStoreItem) (*SyncStoreItem, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx := context.Background()
	writeTime := time.Now().UnixNano() + s.timeOffset

	// Reject items with timestamps too far in the future (clock skew protection).
	maxTimestamp := time.Now().UnixNano() + int64(MaxClockSkew)
	if item.Timestamp > maxTimestamp {
		return nil, fmt.Errorf("item timestamp too far in the future (clock skew exceeds %v)", MaxClockSkew)
	}

	// Check if existing item is newer (use getItem to see tombstones too)
	existing, err := s.getItem(ctx, item.Key)
	if err != nil && !errors.Is(err, storemd.ErrNotFound) {
		return nil, err
	}
	if existing != nil && (existing.Timestamp > item.Timestamp || existing.ID == item.ID) {
		return nil, nil
	}

	item.WriteTimestamp = writeTime
	queueID := QueueID(writeTime, item.ID, item.Key)

	encoded, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}

	if err := s.store.Set(ctx, QueueKey(queueID), item.ID); err != nil {
		return nil, err
	}
	if err := s.store.Set(ctx, ViewKey(item.Key), item.ID); err != nil {
		return nil, err
	}
	if err := s.store.Set(ctx, ValueKey(item.ID), string(encoded)); err != nil {
		return nil, err
	}

	// Clean up the old value inline if it's being replaced.
	if existing != nil && existing.ID != item.ID {
		s.store.Delete(ctx, ValueKey(existing.ID))
	}

	// Send GC request to bounded worker pool (non-blocking).
	select {
	case s.gcCh <- gcRequest{key: item.Key, currentID: item.ID}:
	default:
		// GC channel full — skip GC for this write, it will be caught later.
	}

	// Validate write time after performing writes (we want TOCTOU, else we will miss sync items)
	if time.Now().UnixNano() > writeTime {
		return nil, fmt.Errorf("write time is in the past")
	}

	// Re-read the current item for notification.
	current, err := s.getItem(ctx, item.Key)
	if err != nil {
		return nil, err
	}
	return current, nil
}

// gcKey scans for all queue entries referencing the given key and removes
// any that don't belong to the current value ID. This cleans up orphaned
// values and queue entries that may have been missed by previous writes.
func (s *StoreSync) gcKey(key, currentID string) {
	ctx := context.Background()
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
		if err := s.setItem(item); err != nil {
			return err
		}
	}
	return nil
}

const MaxSyncOutLimit = 1000

// SyncOut returns queued items for a peer since its last sync cursor.
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

	payload, err := s.syncOut(ctx, lastSyncTimestamp, limit)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return nil, err
	}
	if err := s.store.Set(ctx, LastSyncOutKey(peerID), string(encoded)); err != nil {
		return nil, err
	}

	return payload, nil
}

func (s *StoreSync) syncOut(ctx context.Context, timestamp int64, limit int) (*SyncPayload, error) {
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

		items = append(items, *item)

		if item.WriteTimestamp > lastSyncTimestamp && item.WriteTimestamp <= now {
			lastSyncTimestamp = item.WriteTimestamp
		}
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
	if incoming != nil && len(incoming.Items) > 0 {
		if err := s.SyncIn(ctx, peerID, *incoming); err != nil {
			return nil, err
		}
	}

	payload, err := s.SyncOut(ctx, peerID, MaxSyncOutLimit)
	if err != nil {
		return nil, err
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
	Sync(ctx context.Context, peerID string, incoming *SyncPayload) (*SyncPayload, error)

	// Close stops background workers and releases resources.
	Close() error
}

var _ SyncStore = (*StoreSync)(nil)
