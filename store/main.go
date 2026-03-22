package storemd

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a key does not exist in the store.
var ErrNotFound = errors.New("NOT_FOUND")

// NotFoundError is an alias for backward compatibility.
// Deprecated: use ErrNotFound with errors.Is() instead.
var NotFoundError = ErrNotFound

type KeyValuePair struct {
	Key   string
	Value string
}

type ListArgs struct {
	Prefix     string
	StartAfter string // full key, including prefix
	Limit      int    // 0 means no limit
}

// Store is the core key-value interface. All implementations must be safe
// for concurrent use by multiple goroutines.
type Store interface {
	Get(ctx context.Context, key string) (value string, err error)
	Set(ctx context.Context, key, value string) (err error)
	// SetIfNotExists writes the key only if it does not already exist.
	// Returns true if the write succeeded (key was new), false if the key already existed.
	SetIfNotExists(ctx context.Context, key, value string) (bool, error)
	Delete(ctx context.Context, key string) (err error)
	List(ctx context.Context, args ListArgs) (result []KeyValuePair, err error)
	// Close releases any resources held by the store. Implementations that
	// don't hold resources (e.g. in-memory) may return nil.
	Close() error
}
