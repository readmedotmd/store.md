// Package memory provides an in-memory Store implementation backed by a
// sorted map. Data does not persist across restarts.
package memory

import (
	"sort"
	"strings"
	"sync"

	storemd "github.com/readmedotmd/store.md"
)

// StoreMemory is a thread-safe in-memory key-value store.
type StoreMemory struct {
	mu   sync.RWMutex
	data map[string]string
}

// New creates an empty in-memory store.
func New() *StoreMemory {
	return &StoreMemory{data: make(map[string]string)}
}

func (s *StoreMemory) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", storemd.NotFoundError
	}
	return v, nil
}

func (s *StoreMemory) Set(key, value string) error {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
	return nil
}

func (s *StoreMemory) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return storemd.NotFoundError
	}
	delete(s.data, key)
	return nil
}

func (s *StoreMemory) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect and sort keys.
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		if args.Prefix != "" && !strings.HasPrefix(k, args.Prefix) {
			continue
		}
		if args.StartAfter != "" && k <= args.StartAfter {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if args.Limit > 0 && len(keys) > args.Limit {
		keys = keys[:args.Limit]
	}

	result := make([]storemd.KeyValuePair, len(keys))
	for i, k := range keys {
		result[i] = storemd.KeyValuePair{Key: k, Value: s.data[k]}
	}
	return result, nil
}
