package imagestore_test

import (
	"context"
	"testing"

	memstore "github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/imagestore"
)

func TestPutGet(t *testing.T) {
	ctx := context.Background()
	is := imagestore.New(memstore.New())

	data := []byte{0x89, 0x50, 0x4e, 0x47} // PNG magic bytes
	id, err := is.Put(ctx, data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id == "" {
		t.Fatal("Put returned empty ID")
	}

	rec, err := is.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ID != id {
		t.Errorf("ID mismatch: got %q want %q", rec.ID, id)
	}
	if rec.MimeType != "image/png" {
		t.Errorf("MimeType: got %q want %q", rec.MimeType, "image/png")
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
	is := imagestore.New(memstore.New())

	original := []byte("fake jpeg data")
	id, err := is.Put(ctx, original, "image/jpeg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, mime, err := is.GetBytes(ctx, id)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime: got %q want %q", mime, "image/jpeg")
	}
	if string(got) != string(original) {
		t.Errorf("bytes mismatch: got %q want %q", got, original)
	}
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	is := imagestore.New(memstore.New())

	data := []byte("screenshot data")
	id, err := is.Put(ctx, data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := is.Meta(ctx, id)
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
	is := imagestore.New(memstore.New())

	ids := make([]string, 3)
	for i := range ids {
		id, err := is.Put(ctx, []byte{byte(i)}, "image/png")
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		ids[i] = id
	}

	metas, err := is.List(ctx)
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
	is := imagestore.New(memstore.New())

	id, err := is.Put(ctx, []byte("img"), "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := is.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := is.Get(ctx, id); err == nil {
		t.Error("Get after Delete should return error")
	}
}
