# Getting Started

## Installation

```bash
go get github.com/readmedotmd/store.md
```

Then import the backend you need:

```bash
# Pick one (or more)
go get github.com/readmedotmd/store.md/bbolt
go get github.com/readmedotmd/store.md/badger
go get github.com/readmedotmd/store.md/sql
go get github.com/readmedotmd/store.md/s3
go get github.com/readmedotmd/store.md/mongodb
go get github.com/readmedotmd/store.md/redis
```

## The Store Interface

Every backend implements the same interface:

```go
type Store interface {
    Get(key string) (value string, err error)
    Set(key, value string) (err error)
    Delete(key string) (err error)
    List(args ListArgs) (result []KeyValuePair, err error)
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

When a key doesn't exist, `Get` and `Delete` return `storemd.NotFoundError`:

```go
val, err := store.Get("missing-key")
if err == storemd.NotFoundError {
    // key does not exist
}
```

## Basic Usage

```go
package main

import (
    "fmt"
    "log"

    storemd "github.com/readmedotmd/store.md"
    "github.com/readmedotmd/store.md/bbolt"
)

func main() {
    store, err := bbolt.New("my.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()

    // Set a value
    store.Set("user:1:name", "Alice")
    store.Set("user:2:name", "Bob")

    // Get a value
    name, err := store.Get("user:1:name")
    if err == storemd.NotFoundError {
        fmt.Println("not found")
        return
    }
    fmt.Println(name) // Alice

    // List with prefix
    users, _ := store.List(storemd.ListArgs{
        Prefix: "user:",
    })
    for _, u := range users {
        fmt.Printf("%s = %s\n", u.Key, u.Value)
    }

    // Paginate
    page1, _ := store.List(storemd.ListArgs{Limit: "10"})
    if len(page1) == 10 {
        lastKey := page1[len(page1)-1].Key
        page2, _ := store.List(storemd.ListArgs{
            Limit:      "10",
            StartAfter: lastKey,
        })
        _ = page2
    }

    // Delete
    store.Delete("user:1:name")
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

This runs 14 tests covering all interface methods, edge cases, ordering, and pagination.

## Next Steps

- [Implementations](./implementations.md) — details on each backend
- [Sync Store](./sync.md) — peer-to-peer synchronization layer
- [Message Store](./message.md) — request/response messaging over sync
- [Client Adapter](./client.md) — WebSocket sync adapter for connecting to servers
- [Testing](./testing.md) — the generic test suite
