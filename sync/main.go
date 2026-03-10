package sync

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

func viewKey(key string) string {
	return "%sync%view%" + key
}

func valueKey(id string) string {
	return "%sync%value%" + id
}

func queueKey(id string) string {
	return "%sync%queue%" + id
}

func queueID(writeTime int64, id, key string) string {
	return fmt.Sprintf("%d%%%s%%%s", writeTime, id, key)
}

func lastSyncInKey(peerID string) string {
	return "%sync%lastsyncin%" + peerID
}

func lastSyncOutKey(peerID string) string {
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
		Prefix: viewKey(args.Prefix),
		Limit:  args.Limit,
	}
	if args.StartAfter != "" {
		viewArgs.StartAfter = viewKey(args.StartAfter)
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
	valueID, err := s.store.Get(viewKey(key))
	if err != nil {
		return nil, err
	}
	return s.getValue(valueID)
}

func (s *StoreSync) getValue(id string) (*SyncStoreItem, error) {
	raw, err := s.store.Get(valueKey(id))
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
		Prefix: viewKey(prefix),
		Limit:  limit,
	}
	if startAfter != "" {
		args.StartAfter = viewKey(startAfter)
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
	writeTime := time.Now().UnixNano() + s.timeOffset

	// Check if existing item is newer (use getItem to see tombstones too)
	existing, err := s.getItem(item.Key)
	if err != nil && err != storemd.NotFoundError {
		return err
	}
	if existing != nil && existing.Timestamp > item.Timestamp {
		return nil
	}

	item.WriteTimestamp = writeTime
	queueID := queueID(writeTime, item.ID, item.Key)

	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}

	if err := s.store.Set(queueKey(queueID), item.ID); err != nil {
		return err
	}
	if err := s.store.Set(viewKey(item.Key), item.ID); err != nil {
		return err
	}
	if err := s.store.Set(valueKey(item.ID), string(encoded)); err != nil {
		return err
	}

	// Validate write time after performing writes (we want TOCTOU, else we will miss sync items)
	if time.Now().UnixNano() > writeTime {
		return fmt.Errorf("write time is in the past")
	}

	// Notify with the current view, not the item we just wrote — another
	// concurrent write may have already updated the view to a newer value.
	// Use getItem to include tombstones so delete notifications work.
	current, err := s.getItem(item.Key)
	if err != nil {
		return err
	}
	s.notifyListeners(*current)

	return nil
}

type SyncPayload struct {
	Items             []SyncStoreItem `json:"items"`
	LastSyncTimestamp int64           `json:"lastSyncTimestamp"`
}

func (s *StoreSync) SyncIn(peerID string, payload SyncPayload) error {
	for _, item := range payload.Items {
		if err := s.setItem(item); err != nil {
			return err
		}
	}

	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return err
	}
	return s.store.Set(lastSyncInKey(peerID), string(encoded))
}

func (s *StoreSync) SyncOut(peerID string, limit int) (*SyncPayload, error) {
	var lastSyncTimestamp int64

	raw, err := s.store.Get(lastSyncOutKey(peerID))
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
	if err := s.store.Set(lastSyncOutKey(peerID), string(encoded)); err != nil {
		return nil, err
	}

	return payload, nil
}

func (s *StoreSync) syncOut(timestamp int64, limit int) (*SyncPayload, error) {
	now := time.Now().UnixNano()

	args := storemd.ListArgs{
		Prefix:     queueKey(""),
		// Use ~ instead of % as the suffix after the timestamp. Queue entries are
		// keyed as {writeTime}%{uuid}%{key}, so using % would produce a StartAfter
		// that is lexicographically before entries with the same timestamp (because
		// {ts}% < {ts}%{uuid}%...), causing them to be included again. The ~ char
		// sorts after all alphanumeric and % characters, ensuring we skip past all
		// entries with the given timestamp.
		StartAfter: queueKey(fmt.Sprintf("%d~", timestamp)),
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
		Items:             items,
		LastSyncTimestamp:  lastSyncTimestamp,
	}, nil
}

// SyncStore is the interface implemented by both StoreSync and StoreMessage.
type SyncStore interface {
	storemd.Store
	GetItem(key string) (*SyncStoreItem, error)
	SetItem(app, key, value string) error
	ListItems(prefix, startAfter string, limit int) ([]SyncStoreItem, error)
	OnUpdate(fn UpdateListener) func()
	SyncIn(peerID string, payload SyncPayload) error
	SyncOut(peerID string, limit int) (*SyncPayload, error)
}

// Ensure StoreSync implements SyncStore at compile time.
var _ SyncStore = (*StoreSync)(nil)
