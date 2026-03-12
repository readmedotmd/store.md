package memory

import (
	"testing"

	storemd "github.com/readmedotmd/store.md"
)

func TestMemoryStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		return New()
	})
}

func TestMemoryStore_SetIfNotExists(t *testing.T) {
	storemd.RunSetIfNotExistsTests(t, func(t *testing.T) storemd.Store {
		return New()
	})
}

func TestMemoryStore_Clear(t *testing.T) {
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		return New()
	})
}
