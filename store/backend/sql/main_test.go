package sqlstore

import (
	"database/sql"
	"path/filepath"
	"testing"

	storemd "github.com/readmedotmd/store.md"
	_ "modernc.org/sqlite"
)

func TestSQLStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		dbPath := filepath.Join(t.TempDir(), "test.db")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open sqlite: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		store, err := New(db)
		if err != nil {
			t.Fatalf("failed to create store: %v", err)
		}
		return store
	})
}

func TestSQLStore_SetIfNotExists(t *testing.T) {
	storemd.RunSetIfNotExistsTests(t, func(t *testing.T) storemd.Store {
		dbPath := filepath.Join(t.TempDir(), "test.db")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open sqlite: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		store, err := New(db)
		if err != nil {
			t.Fatalf("failed to create store: %v", err)
		}
		return store
	})
}

func TestSQLStore_Clear(t *testing.T) {
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		dbPath := filepath.Join(t.TempDir(), "test.db")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open sqlite: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		store, err := New(db)
		if err != nil {
			t.Fatalf("failed to create store: %v", err)
		}
		return store
	})
}
