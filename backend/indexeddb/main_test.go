//go:build js && wasm

package indexeddb

import (
	"fmt"
	"syscall/js"
	"testing"

	storemd "github.com/readmedotmd/store.md"
)

func TestStoreIndexedDB(t *testing.T) {
	var counter int
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		counter++
		dbName := fmt.Sprintf("test_db_%d", counter)
		store, err := New(dbName)
		if err != nil {
			t.Fatalf("failed to create IndexedDB store: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
			// Delete the database after the test to keep things clean.
			js.Global().Get("indexedDB").Call("deleteDatabase", dbName)
		})
		return store
	})
}

func TestStoreIndexedDB_Clear(t *testing.T) {
	var counter int
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		counter++
		dbName := fmt.Sprintf("test_clear_db_%d", counter)
		store, err := New(dbName)
		if err != nil {
			t.Fatalf("failed to create IndexedDB store: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
			js.Global().Get("indexedDB").Call("deleteDatabase", dbName)
		})
		return store
	})
}
