# Getting Started

## Installation

```bash
go get github.com/readmedotmd/store.md
```

Then import the backend you need:

```bash
# Pick one (or more)
go get github.com/readmedotmd/store.md/backend/bbolt
go get github.com/readmedotmd/store.md/backend/badger
go get github.com/readmedotmd/store.md/backend/sql
go get github.com/readmedotmd/store.md/backend/s3
go get github.com/readmedotmd/store.md/backend/mongodb
go get github.com/readmedotmd/store.md/backend/redis
```

## The Store Interface

Every backend implements the same interface:

```go
type Store interface {
    Get(ctx context.Context, key string) (value string, err error)
    Set(ctx context.Context, key, value string) (err error)
    Delete(ctx context.Context, key string) (err error)
    List(ctx context.Context, args ListArgs) (result []KeyValuePair, err error)
}
```

`List` supports filtering and pagination:

```go
type ListArgs struct {
    Prefix     string // filter keys by prefix
    StartAfter string // cursor for pagination (exclusive)
    Limit      string // max number of results
}
```

## Error Handling

When a key doesn't exist, `Get` and `Delete` return `storemd.ErrNotFound`. Use `errors.Is` for comparison:

```go
val, err := store.Get(ctx, "missing-key")
if errors.Is(err, storemd.ErrNotFound) {
    // key does not exist
}
```

## Basic Usage

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"

    storemd "github.com/readmedotmd/store.md"
    "github.com/readmedotmd/store.md/backend/bbolt"
)

func main() {
    store, err := bbolt.New("my.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()

    ctx := context.Background()

    // Set a value
    store.Set(ctx, "user:1:name", "Alice")
    store.Set(ctx, "user:2:name", "Bob")

    // Get a value
    name, err := store.Get(ctx, "user:1:name")
    if errors.Is(err, storemd.ErrNotFound) {
        fmt.Println("not found")
        return
    }
    fmt.Println(name) // Alice

    // List with prefix
    users, _ := store.List(ctx, storemd.ListArgs{
        Prefix: "user:",
    })
    for _, u := range users {
        fmt.Printf("%s = %s\n", u.Key, u.Value)
    }

    // Paginate
    page1, _ := store.List(ctx, storemd.ListArgs{Limit: "10"})
    if len(page1) == 10 {
        lastKey := page1[len(page1)-1].Key
        page2, _ := store.List(ctx, storemd.ListArgs{
            Limit:      "10",
            StartAfter: lastKey,
        })
        _ = page2
    }

    // Delete
    store.Delete(ctx, "user:1:name")
}
```

## Testing Your Implementation

If you're building a custom `Store` backend, use the generic test suite:

```go
package mystore

import (
    "testing"

    storemd "github.com/readmedotmd/store.md"
)

func TestMyStore(t *testing.T) {
    storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
        return NewMyStore() // return a fresh instance
    })
}
```

This runs the full test suite covering all interface methods, edge cases, ordering, pagination, and concurrent access.

## Next Steps

- [Implementations](./implementations.md) — details on each backend
- [Sync Store](./sync.md) — peer-to-peer synchronization layer
- [Message Store](./message.md) — request/response messaging over sync
- [Client Adapter](./client.md) — WebSocket sync adapter for connecting to servers
- [Testing](./testing.md) — the generic test suite
