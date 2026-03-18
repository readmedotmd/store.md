package blobstore_test

import (
	"context"
	"testing"

	memstore "github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/blobstore"
)

func TestPutGet(t *testing.T) {
	ctx := context.Background()
	bs := blobstore.New(memstore.New())

	data := []byte{0x89, 0x50, 0x4e, 0x47} // PNG magic bytes
	id, err := bs.Put(ctx, data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id == "" {
		t.Fatal("Put returned empty ID")
	}

	rec, err := bs.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ID != id {
		t.Errorf("ID mismatch: got %q want %q", rec.ID, id)
	}
	if rec.ContentType != "image/png" {
		t.Errorf("ContentType: got %q want %q", rec.ContentType, "image/png")
	}
	if rec.Size != len(data) {
		t.Errorf("Size: got %d want %d", rec.Size, len(data))
	}
	if rec.Data == "" {
		t.Error("Data is empty")
	}
}

func TestGetBytes(t *testing.T) {
	ctx := context.Background()
	bs := blobstore.New(memstore.New())

	original := []byte("arbitrary blob data")
	id, err := bs.Put(ctx, original, "application/octet-stream")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ct, err := bs.GetBytes(ctx, id)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if ct != "application/octet-stream" {
		t.Errorf("ContentType: got %q want %q", ct, "application/octet-stream")
	}
	if string(got) != string(original) {
		t.Errorf("bytes mismatch: got %q want %q", got, original)
	}
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	bs := blobstore.New(memstore.New())

	data := []byte("some blob")
	id, err := bs.Put(ctx, data, "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := bs.Meta(ctx, id)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.ID != id {
		t.Errorf("ID: got %q want %q", meta.ID, id)
	}
	if meta.Size != len(data) {
		t.Errorf("Size: got %d want %d", meta.Size, len(data))
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	bs := blobstore.New(memstore.New())

	ids := make([]string, 3)
	for i := range ids {
		id, err := bs.Put(ctx, []byte{byte(i)}, "application/octet-stream")
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		ids[i] = id
	}

	metas, err := bs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("List count: got %d want 3", len(metas))
	}
	// Data field must not be present in metadata listing
	for _, m := range metas {
		if m.ID == "" {
			t.Error("Meta has empty ID")
		}
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	bs := blobstore.New(memstore.New())

	id, err := bs.Put(ctx, []byte("blob"), "application/octet-stream")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := bs.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := bs.Get(ctx, id); err == nil {
		t.Error("Get after Delete should return error")
	}
}
