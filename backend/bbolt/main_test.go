package bbolt

import (
	"path/filepath"
	"testing"

	storemd "github.com/readmedotmd/store.md"
)

func TestBBoltStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		s, err := New(dbPath)
		if err != nil {
			t.Fatalf("failed to create bbolt store: %v", err)
		}
		t.Cleanup(func() {
			s.Close()
		})
		return s
	})
}

func TestBBoltStore_Clear(t *testing.T) {
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		s, err := New(dbPath)
		if err != nil {
			t.Fatalf("failed to create bbolt store: %v", err)
		}
		t.Cleanup(func() {
			s.Close()
		})
		return s
	})
}
