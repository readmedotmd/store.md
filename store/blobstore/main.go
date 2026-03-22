// Package blobstore provides a generic blob storage layer on top of store.md.
// Blobs are stored as JSON records under the "blobs/" key prefix. Callers
// receive an opaque string ID that can be used to retrieve or delete the blob
// later. The store may be shared with other users of the same storemd.Store
// because all keys are namespaced under the prefix.
package blobstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	storemd "github.com/readmedotmd/store.md"
)

const keyPrefix = "blobs/"

// Record is a stored blob with its full base64-encoded data.
type Record struct {
	ID          string    `json:"id"`
	ContentType string    `json:"content_type"`
	Data        string    `json:"data"` // base64-encoded bytes
	Size        int       `json:"size"` // original byte length
	CreatedAt   time.Time `json:"created_at"`
}

// Meta is a Record without the blob data, used for listing.
type Meta struct {
	ID          string    `json:"id"`
	ContentType string    `json:"content_type"`
	Size        int       `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store wraps a storemd.Store to provide blob storage operations.
type Store struct {
	s storemd.Store
}

// New returns a Store backed by s. All blobs are stored under the "blobs/"
// key prefix, so s may be shared with other users of the same store.
func New(s storemd.Store) *Store {
	return &Store{s: s}
}

// Put stores raw blob bytes and returns the assigned blob ID.
// contentType should be a valid MIME type such as "image/png" or "application/octet-stream".
func (bs *Store) Put(ctx context.Context, data []byte, contentType string) (string, error) {
	id := fmt.Sprintf("blob_%d_%04x", time.Now().UnixMilli(), rand.Intn(0x10000))
	rec := Record{
		ID:          id,
		ContentType: contentType,
		Data:        base64.StdEncoding.EncodeToString(data),
		Size:        len(data),
		CreatedAt:   time.Now(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("blobstore: marshal: %w", err)
	}
	if err := bs.s.Set(ctx, keyPrefix+id, string(b)); err != nil {
		return "", fmt.Errorf("blobstore: set: %w", err)
	}
	return id, nil
}

// Get retrieves a full blob record (including base64 data) by ID.
func (bs *Store) Get(ctx context.Context, id string) (Record, error) {
	val, err := bs.s.Get(ctx, keyPrefix+id)
	if err != nil {
		return Record{}, fmt.Errorf("blobstore: get %s: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal([]byte(val), &rec); err != nil {
		return Record{}, fmt.Errorf("blobstore: unmarshal %s: %w", id, err)
	}
	return rec, nil
}

// GetBytes decodes and returns the raw bytes for a blob ID.
func (bs *Store) GetBytes(ctx context.Context, id string) ([]byte, string, error) {
	rec, err := bs.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	b, err := base64.StdEncoding.DecodeString(rec.Data)
	if err != nil {
		return nil, "", fmt.Errorf("blobstore: decode %s: %w", id, err)
	}
	return b, rec.ContentType, nil
}

// Meta returns blob metadata for an ID without fetching the blob data.
func (bs *Store) Meta(ctx context.Context, id string) (Meta, error) {
	rec, err := bs.Get(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	return Meta{
		ID:          rec.ID,
		ContentType: rec.ContentType,
		Size:        rec.Size,
		CreatedAt:   rec.CreatedAt,
	}, nil
}

// List returns metadata for all stored blobs in insertion order.
// No blob data is included.
func (bs *Store) List(ctx context.Context) ([]Meta, error) {
	pairs, err := bs.s.List(ctx, storemd.ListArgs{Prefix: keyPrefix})
	if err != nil {
		return nil, fmt.Errorf("blobstore: list: %w", err)
	}
	metas := make([]Meta, 0, len(pairs))
	for _, p := range pairs {
		var rec Record
		if err := json.Unmarshal([]byte(p.Value), &rec); err != nil {
			continue // skip corrupt entries
		}
		metas = append(metas, Meta{
			ID:          rec.ID,
			ContentType: rec.ContentType,
			Size:        rec.Size,
			CreatedAt:   rec.CreatedAt,
		})
	}
	return metas, nil
}

// Delete removes a blob by ID.
func (bs *Store) Delete(ctx context.Context, id string) error {
	if err := bs.s.Delete(ctx, keyPrefix+id); err != nil {
		return fmt.Errorf("blobstore: delete %s: %w", id, err)
	}
	return nil
}
