package core

import (
	"encoding/json"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	storemd "github.com/readmedotmd/store.md"
)

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

type StoreSync struct {
	store        storemd.Store
	timeOffset   int64 // nanoseconds
	writeMu      gosync.Mutex // serializes setItem to make conflict resolution atomic
	mu           gosync.RWMutex
	listeners    map[uint64]UpdateListener
	nextListenID atomic.Uint64
}

func New(store storemd.Store, timeOffset ...int64) *StoreSync {
	offset := int64(10_000_000_000) // 10 seconds
	if len(timeOffset) > 0 {
		offset = timeOffset[0]
	}
	return &StoreSync{
		store:      store,
		timeOffset: offset,
		listeners:  make(map[uint64]UpdateListener),
	}
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
// Returns NotFoundError if the key has been deleted (tombstoned).
func (s *StoreSync) Get(key string) (string, error) {
	item, err := s.getItem(key)
	if err != nil {
		return "", err
	}
	if item.Deleted {
		return "", storemd.NotFoundError
	}
	return item.Value, nil
}

// Set implements storemd.Store. Writes a value with an empty app.
func (s *StoreSync) Set(key, value string) error {
	return s.SetItem("", key, value)
}

// Delete implements storemd.Store. Writes a tombstone that syncs to all peers.
// Returns NotFoundError if the key does not exist.
func (s *StoreSync) Delete(key string) error {
	existing, err := s.getItem(key)
	if err != nil {
		return err
	}
	if existing.Deleted {
		return storemd.NotFoundError
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
func (s *StoreSync) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	viewArgs := storemd.ListArgs{
		Prefix: ViewKey(args.Prefix),
		Limit:  args.Limit,
	}
	if args.StartAfter != "" {
		viewArgs.StartAfter = ViewKey(args.StartAfter)
	}

	list, err := s.store.List(viewArgs)
	if err != nil {
		return nil, err
	}

	result := make([]storemd.KeyValuePair, 0, len(list))
	for _, kv := range list {
		item, err := s.getValue(kv.Value)
		if err != nil {
			if err == storemd.NotFoundError {
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
// Returns NotFoundError if the key has been deleted (tombstoned).
func (s *StoreSync) GetItem(key string) (*SyncStoreItem, error) {
	item, err := s.getItem(key)
	if err != nil {
		return nil, err
	}
	if item.Deleted {
		return nil, storemd.NotFoundError
	}
	return item, nil
}

// getItem returns the raw SyncStoreItem including tombstones.
func (s *StoreSync) getItem(key string) (*SyncStoreItem, error) {
	valueID, err := s.store.Get(ViewKey(key))
	if err != nil {
		return nil, err
	}
	return s.getValue(valueID)
}

func (s *StoreSync) getValue(id string) (*SyncStoreItem, error) {
	raw, err := s.store.Get(ValueKey(id))
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
func (s *StoreSync) ListItems(prefix, startAfter string, limit int) ([]SyncStoreItem, error) {
	args := storemd.ListArgs{
		Prefix: ViewKey(prefix),
		Limit:  limit,
	}
	if startAfter != "" {
		args.StartAfter = ViewKey(startAfter)
	}

	list, err := s.store.List(args)
	if err != nil {
		return nil, err
	}

	items := make([]SyncStoreItem, 0, len(list))
	for _, kv := range list {
		item, err := s.getValue(kv.Value)
		if err != nil {
			if err == storemd.NotFoundError {
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
func (s *StoreSync) SetItem(app, key, value string) error {
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
	s.writeMu.Lock()

	writeTime := time.Now().UnixNano() + s.timeOffset

	// Check if existing item is newer (use getItem to see tombstones too)
	existing, err := s.getItem(item.Key)
	if err != nil && err != storemd.NotFoundError {
		s.writeMu.Unlock()
		return err
	}
	if existing != nil && (existing.Timestamp > item.Timestamp || existing.ID == item.ID) {
		s.writeMu.Unlock()
		return nil
	}

	item.WriteTimestamp = writeTime
	queueID := QueueID(writeTime, item.ID, item.Key)

	encoded, err := json.Marshal(item)
	if err != nil {
		s.writeMu.Unlock()
		return err
	}

	if err := s.store.Set(QueueKey(queueID), item.ID); err != nil {
		s.writeMu.Unlock()
		return err
	}
	if err := s.store.Set(ViewKey(item.Key), item.ID); err != nil {
		s.writeMu.Unlock()
		return err
	}
	if err := s.store.Set(ValueKey(item.ID), string(encoded)); err != nil {
		s.writeMu.Unlock()
		return err
	}

	// Spawn async GC to clean up all old values and queue entries for this key.
	go s.gcKey(item.Key, item.ID)

	// Validate write time after performing writes (we want TOCTOU, else we will miss sync items)
	if time.Now().UnixNano() > writeTime {
		s.writeMu.Unlock()
		return fmt.Errorf("write time is in the past")
	}

	s.writeMu.Unlock()

	// Notify outside the lock — listeners may call back into the store.
	current, err := s.getItem(item.Key)
	if err != nil {
		return err
	}
	s.notifyListeners(*current)

	return nil
}

// gcKey scans for all queue entries referencing the given key and removes
// any that don't belong to the current value ID. This cleans up orphaned
// values and queue entries that may have been missed by previous writes.
func (s *StoreSync) gcKey(key, currentID string) {
	// Scan all queue entries to find ones referencing this key.
	// Queue keys are: %sync%queue%{writeTime}%{uuid}%{key}
	suffix := "%" + key
	var startAfter string
	for {
		list, err := s.store.List(storemd.ListArgs{
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
				s.store.Delete(kv.Key)
				s.store.Delete(ValueKey(kv.Value))
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
func (s *StoreSync) SyncIn(peerID string, payload SyncPayload) error {
	for _, item := range payload.Items {
		if err := s.setItem(item); err != nil {
			return err
		}
	}
	return nil
}

const MaxSyncOutLimit = 1000

// SyncOut returns queued items for a peer since its last sync cursor.
func (s *StoreSync) SyncOut(peerID string, limit int) (*SyncPayload, error) {
	if limit <= 0 || limit > MaxSyncOutLimit {
		limit = MaxSyncOutLimit
	}

	var lastSyncTimestamp int64

	raw, err := s.store.Get(LastSyncOutKey(peerID))
	if err != nil && err != storemd.NotFoundError {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &lastSyncTimestamp); err != nil {
			return nil, err
		}
	}

	payload, err := s.syncOut(lastSyncTimestamp, limit)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return nil, err
	}
	if err := s.store.Set(LastSyncOutKey(peerID), string(encoded)); err != nil {
		return nil, err
	}

	return payload, nil
}

func (s *StoreSync) syncOut(timestamp int64, limit int) (*SyncPayload, error) {
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

	list, err := s.store.List(args)
	if err != nil {
		return nil, err
	}

	items := make([]SyncStoreItem, 0, len(list))
	lastSyncTimestamp := timestamp

	for _, kv := range list {
		item, err := s.getValue(kv.Value)
		if err != nil {
			if err == storemd.NotFoundError {
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
func (s *StoreSync) Sync(peerID string, incoming *SyncPayload) (*SyncPayload, error) {
	if incoming != nil && len(incoming.Items) > 0 {
		if err := s.SyncIn(peerID, *incoming); err != nil {
			return nil, err
		}
	}

	payload, err := s.SyncOut(peerID, MaxSyncOutLimit)
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
	GetItem(key string) (*SyncStoreItem, error)
	SetItem(app, key, value string) error
	ListItems(prefix, startAfter string, limit int) ([]SyncStoreItem, error)
	OnUpdate(fn UpdateListener) func()

	// Sync exchanges data with a peer.
	// nil incoming = initiate a sync exchange.
	// Non-nil incoming = process received data and prepare response.
	// Returns nil when the exchange is complete (no more data to send).
	Sync(peerID string, incoming *SyncPayload) (*SyncPayload, error)
}

var _ SyncStore = (*StoreSync)(nil)
