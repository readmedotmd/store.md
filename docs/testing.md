# Testing

## Generic Test Suite

The `storemd.RunStoreTests` function provides a reusable test suite for any `Store` implementation. It covers all interface methods, including concurrent access tests to verify thread safety.

### Usage

```go
import (
    "testing"
    storemd "github.com/readmedotmd/store.md"
)

func TestMyStore(t *testing.T) {
    storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
        // Return a fresh, empty store instance for each sub-test
        return createMyStore(t)
    })
}
```

The factory function is called once per sub-test, so each test gets an isolated store.

### What's Tested

| Test | What it verifies |
|------|-----------------|
| `Get_NotFound` | Returns `ErrNotFound` for missing keys |
| `SetAndGet` | Basic write then read |
| `Set_Overwrite` | Writing to an existing key replaces the value |
| `Delete` | Deleting a key makes it not found |
| `Delete_NotFound` | Deleting a missing key returns `ErrNotFound` |
| `List_Empty` | Empty store returns empty slice |
| `List_All` | Returns all stored items |
| `List_Prefix` | Filters by key prefix |
| `List_StartAfter` | Excludes keys <= the given key |
| `List_Limit` | Caps the number of results |
| `List_Pagination` | Two pages with no overlap |
| `List_OrderedByKey` | Results are sorted lexicographically |
| `List_PrefixWithStartAfterAndLimit` | All three List params combined |
| `ConcurrentAccess` | Concurrent reads and writes are safe |

### Requirements for Passing

Your implementation must:

1. Return `storemd.ErrNotFound` from `Get` and `Delete` when the key doesn't exist (checked via `errors.Is`)
2. Return results from `List` sorted lexicographically by key
3. Return an empty slice (not nil) from `List` when there are no results
4. Support `Prefix`, `StartAfter`, and `Limit` in `ListArgs` (Limit is a string, parse to int)
5. Treat `StartAfter` as exclusive (don't include the exact key)

## Running Tests

```bash
# Run all tests (some backends need Docker)
go test ./...

# Run specific backend tests
go test ./bbolt/...
go test ./sql/...

# Run sync store tests
go test ./sync/...

# Verbose output
go test ./... -v
```

### Docker-Dependent Tests

These backends use [dockertest](https://github.com/ory/dockertest) and require Docker:

- `mongodb/` — MongoDB 7
- `redis/` — Redis 7
- `s3/` — MinIO (falls back to local binary)

### No Docker Required

- `bbolt/` — embedded, uses temp files
- `badger/` — embedded, uses temp dirs
- `sql/` — uses SQLite (pure Go, no CGO)
- `sync/` — uses bbolt internally
