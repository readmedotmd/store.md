// Package imagestore provides a dedicated image storage layer on top of
// store.md. Images are stored as JSON records under the "images/" key prefix.
// Messages can reference images by ID without embedding the raw base64 data,
// keeping message payloads small while the blobs live in one place.
package imagestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	storemd "github.com/readmedotmd/store.md"
)

const keyPrefix = "images/"

// Record is a stored image with its full base64-encoded data.
type Record struct {
	ID        string    `json:"id"`
	MimeType  string    `json:"mime_type"`
	Data      string    `json:"data"` // base64-encoded bytes
	Size      int       `json:"size"` // original byte length
	CreatedAt time.Time `json:"created_at"`
}

// Meta is a Record without the image data, used for listing.
type Meta struct {
	ID        string    `json:"id"`
	MimeType  string    `json:"mime_type"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// Store wraps a storemd.Store to provide image-specific operations.
type Store struct {
	s storemd.Store
}

// New returns a Store backed by s. All images are stored under the "images/"
// key prefix, so s may be shared with other users of the same store.
func New(s storemd.Store) *Store {
	return &Store{s: s}
}

// Put stores raw image bytes and returns the assigned image ID.
// mimeType should be a valid MIME type such as "image/png" or "image/jpeg".
func (is *Store) Put(ctx context.Context, data []byte, mimeType string) (string, error) {
	id := fmt.Sprintf("img_%d_%04x", time.Now().UnixMilli(), rand.Intn(0x10000))
	rec := Record{
		ID:        id,
		MimeType:  mimeType,
		Data:      base64.StdEncoding.EncodeToString(data),
		Size:      len(data),
		CreatedAt: time.Now(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("imagestore: marshal: %w", err)
	}
	if err := is.s.Set(ctx, keyPrefix+id, string(b)); err != nil {
		return "", fmt.Errorf("imagestore: set: %w", err)
	}
	return id, nil
}

// Get retrieves a full image record (including base64 data) by ID.
func (is *Store) Get(ctx context.Context, id string) (Record, error) {
	val, err := is.s.Get(ctx, keyPrefix+id)
	if err != nil {
		return Record{}, fmt.Errorf("imagestore: get %s: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal([]byte(val), &rec); err != nil {
		return Record{}, fmt.Errorf("imagestore: unmarshal %s: %w", id, err)
	}
	return rec, nil
}

// GetBytes decodes and returns the raw image bytes for an image ID.
func (is *Store) GetBytes(ctx context.Context, id string) ([]byte, string, error) {
	rec, err := is.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	b, err := base64.StdEncoding.DecodeString(rec.Data)
	if err != nil {
		return nil, "", fmt.Errorf("imagestore: decode %s: %w", id, err)
	}
	return b, rec.MimeType, nil
}

// Meta returns image metadata for an ID without fetching the blob data.
func (is *Store) Meta(ctx context.Context, id string) (Meta, error) {
	rec, err := is.Get(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	return Meta{
		ID:        rec.ID,
		MimeType:  rec.MimeType,
		Size:      rec.Size,
		CreatedAt: rec.CreatedAt,
	}, nil
}

// List returns metadata for all stored images in insertion order.
// No blob data is included.
func (is *Store) List(ctx context.Context) ([]Meta, error) {
	pairs, err := is.s.List(ctx, storemd.ListArgs{Prefix: keyPrefix})
	if err != nil {
		return nil, fmt.Errorf("imagestore: list: %w", err)
	}
	metas := make([]Meta, 0, len(pairs))
	for _, p := range pairs {
		var rec Record
		if err := json.Unmarshal([]byte(p.Value), &rec); err != nil {
			continue // skip corrupt entries
		}
		metas = append(metas, Meta{
			ID:        rec.ID,
			MimeType:  rec.MimeType,
			Size:      rec.Size,
			CreatedAt: rec.CreatedAt,
		})
	}
	return metas, nil
}

// Delete removes an image by ID.
func (is *Store) Delete(ctx context.Context, id string) error {
	if err := is.s.Delete(ctx, keyPrefix+id); err != nil {
		return fmt.Errorf("imagestore: delete %s: %w", id, err)
	}
	return nil
}
