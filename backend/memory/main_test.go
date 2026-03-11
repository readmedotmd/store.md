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
