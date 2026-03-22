package storemd_test

import (
	"testing"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
)

func TestMemoryStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		return memory.New()
	})
}

func TestMemoryStore_SetIfNotExists(t *testing.T) {
	storemd.RunSetIfNotExistsTests(t, func(t *testing.T) storemd.Store {
		return memory.New()
	})
}
