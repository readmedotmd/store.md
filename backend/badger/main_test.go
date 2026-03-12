package badger

import (
	"testing"

	storemd "github.com/readmedotmd/store.md"
)

func TestBadgerStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatalf("failed to create badger store: %v", err)
		}
		t.Cleanup(func() {
			s.Close()
		})
		return s
	})
}

func TestBadgerStore_SetIfNotExists(t *testing.T) {
	storemd.RunSetIfNotExistsTests(t, func(t *testing.T) storemd.Store {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatalf("failed to create badger store: %v", err)
		}
		t.Cleanup(func() {
			s.Close()
		})
		return s
	})
}

func TestBadgerStore_Clear(t *testing.T) {
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatalf("failed to create badger store: %v", err)
		}
		t.Cleanup(func() {
			s.Close()
		})
		return s
	})
}
