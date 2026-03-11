package fingerprintsync

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/sync"
)

const (
	// NumBuckets is the number of fingerprint buckets.
	NumBuckets = 256

	// scanPageSize is the batch size for view key scans.
	scanPageSize = 1000
)

// Fingerprints holds computed bucket fingerprints.
type Fingerprints struct {
	Buckets [NumBuckets]uint64
}

// StoreFingerprint is a SyncStore implementation with integrated fingerprint
// reconciliation. It uses the same queue-based sync as StoreSync for the fast
// path, and adds on-demand fingerprint computation for consistency checks.
type StoreFingerprint struct {
	store        storemd.Store
	timeOffset   int64
	mu           gosync.RWMutex
	listeners    map[uint64]sync.UpdateListener
	nextListenID atomic.Uint64
}

// New creates a StoreFingerprint backed by the given store.
func New(store storemd.Store, timeOffset ...int64) *StoreFingerprint {
	offset := int64(10_000_000_000) // 10 seconds
	if len(timeOffset) > 0 {
		offset = timeOffset[0]
	}
	return &StoreFingerprint{
		store:      store,
		timeOffset: offset,
		listeners:  make(map[uint64]sync.UpdateListener),
	}
}

// --- Listener management ---

func (s *StoreFingerprint) OnUpdate(fn sync.UpdateListener) func() {
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

func (s *StoreFingerprint) notifyListeners(item sync.SyncStoreItem) {
	s.mu.RLock()
	listeners := make([]sync.UpdateListener, 0, len(s.listeners))
	for _, fn := range s.listeners {
		listeners = append(listeners, fn)
	}
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(item)
	}
}

// --- Store interface ---

func (s *StoreFingerprint) Get(key string) (string, error) {
	item, err := s.getItem(key)
	if err != nil {
		return "", err
	}
	if item.Deleted {
		return "", storemd.NotFoundError
	}
	return item.Value, nil
}

func (s *StoreFingerprint) Set(key, value string) error {
	return s.SetItem("", key, value)
}

func (s *StoreFingerprint) Delete(key string) error {
	existing, err := s.getItem(key)
	if err != nil {
		return err
	}
	if existing.Deleted {
		return storemd.NotFoundError
	}
	item := sync.SyncStoreItem{
		App:       existing.App,
		Key:       key,
		Value:     "",
		Timestamp: time.Now().UnixNano(),
		ID:        uuid.New().String(),
		Deleted:   true,
	}
	return s.setItem(item)
}

func (s *StoreFingerprint) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	viewArgs := storemd.ListArgs{
		Prefix: sync.ViewKey(args.Prefix),
		Limit:  args.Limit,
	}
	if args.StartAfter != "" {
		viewArgs.StartAfter = sync.ViewKey(args.StartAfter)
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

// --- Sync item management ---

func (s *StoreFingerprint) GetItem(key string) (*sync.SyncStoreItem, error) {
	item, err := s.getItem(key)
	if err != nil {
		return nil, err
	}
	if item.Deleted {
		return nil, storemd.NotFoundError
	}
	return item, nil
}

func (s *StoreFingerprint) getItem(key string) (*sync.SyncStoreItem, error) {
	valueID, err := s.store.Get(sync.ViewKey(key))
	if err != nil {
		return nil, err
	}
	return s.getValue(valueID)
}

func (s *StoreFingerprint) getValue(id string) (*sync.SyncStoreItem, error) {
	raw, err := s.store.Get(sync.ValueKey(id))
	if err != nil {
		return nil, err
	}
	var item sync.SyncStoreItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *StoreFingerprint) ListItems(prefix, startAfter string, limit int) ([]sync.SyncStoreItem, error) {
	args := storemd.ListArgs{
		Prefix: sync.ViewKey(prefix),
		Limit:  limit,
	}
	if startAfter != "" {
		args.StartAfter = sync.ViewKey(startAfter)
	}

	list, err := s.store.List(args)
	if err != nil {
		return nil, err
	}

	items := make([]sync.SyncStoreItem, 0, len(list))
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

func (s *StoreFingerprint) SetItem(app, key, value string) error {
	item := sync.SyncStoreItem{
		App:       app,
		Key:       key,
		Value:     value,
		Timestamp: time.Now().UnixNano(),
		ID:        uuid.New().String(),
	}
	return s.setItem(item)
}

func (s *StoreFingerprint) setItem(item sync.SyncStoreItem) error {
	writeTime := time.Now().UnixNano() + s.timeOffset

	existing, err := s.getItem(item.Key)
	if err != nil && err != storemd.NotFoundError {
		return err
	}
	if existing != nil && existing.Timestamp > item.Timestamp {
		return nil
	}

	item.WriteTimestamp = writeTime
	queueID := sync.QueueID(writeTime, item.ID, item.Key)

	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}

	if err := s.store.Set(sync.QueueKey(queueID), item.ID); err != nil {
		return err
	}
	if err := s.store.Set(sync.ViewKey(item.Key), item.ID); err != nil {
		return err
	}
	if err := s.store.Set(sync.ValueKey(item.ID), string(encoded)); err != nil {
		return err
	}

	if time.Now().UnixNano() > writeTime {
		return fmt.Errorf("write time is in the past")
	}

	current, err := s.getItem(item.Key)
	if err != nil {
		return err
	}
	s.notifyListeners(*current)

	return nil
}

// --- Sync protocol ---

func (s *StoreFingerprint) SyncIn(peerID string, payload sync.SyncPayload) error {
	for _, item := range payload.Items {
		if err := s.setItem(item); err != nil {
			return err
		}
	}

	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return err
	}
	return s.store.Set(sync.LastSyncInKey(peerID), string(encoded))
}

func (s *StoreFingerprint) SyncOut(peerID string, limit int) (*sync.SyncPayload, error) {
	if limit <= 0 || limit > sync.MaxSyncOutLimit {
		limit = sync.MaxSyncOutLimit
	}

	var lastSyncTimestamp int64

	raw, err := s.store.Get(sync.LastSyncOutKey(peerID))
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

	// Include fingerprints in the payload so the peer can detect discrepancies.
	fp, err := s.ComputeFingerprints()
	if err == nil {
		payload.Fingerprints = fp.Buckets[:]
	}

	encoded, err := json.Marshal(payload.LastSyncTimestamp)
	if err != nil {
		return nil, err
	}
	if err := s.store.Set(sync.LastSyncOutKey(peerID), string(encoded)); err != nil {
		return nil, err
	}

	return payload, nil
}

func (s *StoreFingerprint) syncOut(timestamp int64, limit int) (*sync.SyncPayload, error) {
	now := time.Now().UnixNano()

	args := storemd.ListArgs{
		Prefix:     sync.QueueKey(""),
		StartAfter: sync.QueueKey(fmt.Sprintf("%d~", timestamp)),
		Limit:      limit,
	}

	list, err := s.store.List(args)
	if err != nil {
		return nil, err
	}

	items := make([]sync.SyncStoreItem, 0, len(list))
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

	return &sync.SyncPayload{
		Items:            items,
		LastSyncTimestamp: lastSyncTimestamp,
	}, nil
}

// --- Fingerprint computation ---

// ComputeFingerprints scans all view keys and computes XOR fingerprints
// per bucket. This is O(n) where n is the number of keys.
func (s *StoreFingerprint) ComputeFingerprints() (*Fingerprints, error) {
	var fp Fingerprints
	viewPrefix := sync.ViewKey("")
	startAfter := ""

	for {
		list, err := s.store.List(storemd.ListArgs{
			Prefix:     viewPrefix,
			StartAfter: startAfter,
			Limit:      scanPageSize,
		})
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			break
		}

		for _, kv := range list {
			userKey := kv.Key[len(viewPrefix):]
			valueID := kv.Value

			bucket := bucketFor(userKey)
			fp.Buckets[bucket] ^= itemHash(userKey, valueID)
		}

		startAfter = list[len(list)-1].Key
		if len(list) < scanPageSize {
			break
		}
	}

	return &fp, nil
}

// DiffBuckets returns the bucket indices where local and remote fingerprints differ.
func DiffBuckets(local, remote *Fingerprints) []int {
	var diff []int
	for i := 0; i < NumBuckets; i++ {
		if local.Buckets[i] != remote.Buckets[i] {
			diff = append(diff, i)
		}
	}
	return diff
}

// ItemsForBuckets returns all SyncStoreItems whose keys hash into one of the
// specified buckets.
func (s *StoreFingerprint) ItemsForBuckets(buckets []int) ([]sync.SyncStoreItem, error) {
	if len(buckets) == 0 {
		return nil, nil
	}

	bucketSet := make(map[int]struct{}, len(buckets))
	for _, b := range buckets {
		bucketSet[b] = struct{}{}
	}

	viewPrefix := sync.ViewKey("")
	var items []sync.SyncStoreItem
	startAfter := ""

	for {
		list, err := s.store.List(storemd.ListArgs{
			Prefix:     viewPrefix,
			StartAfter: startAfter,
			Limit:      scanPageSize,
		})
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			break
		}

		for _, kv := range list {
			userKey := kv.Key[len(viewPrefix):]
			if _, ok := bucketSet[bucketFor(userKey)]; !ok {
				continue
			}

			item, err := s.getValue(kv.Value)
			if err != nil {
				if err == storemd.NotFoundError {
					continue
				}
				return nil, err
			}
			items = append(items, *item)
		}

		startAfter = list[len(list)-1].Key
		if len(list) < scanPageSize {
			break
		}
	}

	return items, nil
}

// --- Hash helpers ---

func bucketFor(userKey string) int {
	h := fnv.New64a()
	h.Write([]byte(userKey))
	return int(h.Sum64() % NumBuckets)
}

func itemHash(userKey, valueID string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(userKey))
	h.Write([]byte{0})
	h.Write([]byte(valueID))
	return h.Sum64()
}

var _ sync.SyncStore = (*StoreFingerprint)(nil)
