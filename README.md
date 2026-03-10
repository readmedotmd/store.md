<p align="center">
  <img src=".github/banner.svg" alt="store.md" width="900"/>
</p>

<p align="center">
  <code>go get github.com/readmedotmd/store.md</code>
</p>

---

## What is this?

A **single key-value interface** with **seven swappable backends** and a **built-in sync engine** for peer-to-peer replication.

Write your code against one interface. Swap the backend without changing a line.

```go
type Store interface {
    Get(key string) (value string, err error)
    Set(key, value string) (err error)
    Delete(key string) (err error)
    List(args ListArgs) (result []KeyValuePair, err error)
}
```

---

## Backends

<table>
<tr>
<td width="150"><strong>BBolt</strong></td>
<td>Embedded B+ tree. Single file. Zero config.</td>
<td><code>bbolt/</code></td>
</tr>
<tr>
<td><strong>Badger</strong></td>
<td>LSM-tree. SSD-optimized. High throughput.</td>
<td><code>badger/</code></td>
</tr>
<tr>
<td><strong>SQL</strong></td>
<td>Any SQL database. Tested with SQLite (pure Go).</td>
<td><code>sql/</code></td>
</tr>
<tr>
<td><strong>S3</strong></td>
<td>AWS S3, MinIO, R2. Cloud-native storage.</td>
<td><code>s3/</code></td>
</tr>
<tr>
<td><strong>MongoDB</strong></td>
<td>Document store. Atlas or self-hosted.</td>
<td><code>mongodb/</code></td>
</tr>
<tr>
<td><strong>Redis</strong></td>
<td>In-memory. Fast reads. Shared state.</td>
<td><code>redis/</code></td>
</tr>
<tr>
<td><strong>IndexedDB</strong></td>
<td>Browser-native. WASM. Offline-first.</td>
<td><code>indexeddb/</code></td>
</tr>
</table>

> [Detailed backend documentation &rarr;](./docs/implementations.md)

---

## Quick Start

```bash
go get github.com/readmedotmd/store.md
go get github.com/readmedotmd/store.md/bbolt
```

```go
package main

import (
    "fmt"

    storemd "github.com/readmedotmd/store.md"
    "github.com/readmedotmd/store.md/bbolt"
)

func main() {
    store, _ := bbolt.New("data.db")
    defer store.Close()

    store.Set("hello", "world")

    val, err := store.Get("hello")
    if err == storemd.NotFoundError {
        fmt.Println("not found")
        return
    }
    fmt.Println(val) // world
}
```

> [Full getting started guide &rarr;](./docs/getting-started.md)

---

## Sync Engine

Built-in peer-to-peer synchronization with **last-write-wins conflict resolution** and **per-peer incremental cursors**.

```go
import "github.com/readmedotmd/store.md/sync"

ss := sync.New(store)

// Write data
ss.SetItem("myapp", "config", `{"theme":"dark"}`)

// Send to a peer
payload, _ := ss.SyncOut("peer-2", "")

// Receive from a peer
ss.SyncIn("peer-1", payload)
```

Every write is recorded in an ordered queue. `SyncOut` returns only items added since the last sync to that peer. `SyncIn` applies items with conflict resolution — older data never overwrites newer data.

> [Sync documentation &rarr;](./docs/sync.md)

---

## Sync Server

WebSocket server for syncing stores over the network. Accepts any `sync.SyncStore` (both `StoreSync` and `StoreMessage`). Run standalone or plug into an existing HTTP server.

```go
import (
    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/sync"
    "github.com/readmedotmd/store.md/server"
)

store, _ := bbolt.New("data.db")
ss := sync.New(store)

srv := server.New(ss, server.TokenAuth(map[string]string{
    "secret-token-1": "peer-1",
    "secret-token-2": "peer-2",
}))

srv.ListenAndServe(":8080")
```

Or mount on an existing mux:

```go
mux.Handle("/sync", srv)
```

The server delegates all sync protocol handling to the client adapter. When a peer pushes data, the server automatically broadcasts a `sync_update` notification to all other connected peers.

### Multi-Store Server

Run multiple independent stores on a single server. Each store ID maps to its own `SyncStore` — peers connecting to different store IDs are completely isolated.

```go
import (
    "fmt"
    gosync "sync"

    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/sync"
    "github.com/readmedotmd/store.md/server"
)

var (
    mu     gosync.Mutex
    stores = map[string]sync.SyncStore{}
)

resolver := func(storeID string) (sync.SyncStore, error) {
    mu.Lock()
    defer mu.Unlock()
    if ss, ok := stores[storeID]; ok {
        return ss, nil
    }
    store, err := bbolt.New(fmt.Sprintf("data-%s.db", storeID))
    if err != nil {
        return nil, err
    }
    ss := sync.New(store)
    stores[storeID] = ss
    return ss, nil
}

srv := server.NewMulti(resolver, server.TokenAuth(map[string]string{
    "secret-token-1": "peer-1",
    "secret-token-2": "peer-2",
}))

srv.ListenAndServe(":8080")
```

Clients connect with the store ID in the URL path:

```
ws://localhost:8080/project-alpha   ← isolated store
ws://localhost:8080/project-beta    ← isolated store
```

---

## Client Adapter

The `client` package connects a local `SyncStore` to one or more remote sync servers over WebSocket. It handles pushing local changes and pulling remote changes automatically.

```go
import (
    "net/http"
    "time"

    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/client"
    "github.com/readmedotmd/store.md/sync"
)

store, _ := bbolt.New("local.db")
ss := sync.New(store)

c := client.New(ss, client.WithInterval(5*time.Second))
defer c.Close()

header := http.Header{}
header.Set("Authorization", "Bearer secret-token-1")

c.Connect("my-peer-id", "ws://localhost:8080", header)
```

The adapter supports multiple simultaneous connections, broadcasts updates between peers, and handles both client-side and server-side sync protocol roles through a shared `Connection` interface.

> [Client adapter documentation &rarr;](./docs/client.md)

---

## Testing

Every backend passes the same **14-test generic suite**. Use it for your own implementations:

```go
func TestMyStore(t *testing.T) {
    storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
        return mystore.New()
    })
}
```

```bash
go test ./...
```

> [Testing documentation &rarr;](./docs/testing.md)

---

<p align="center">
  <sub>Built by <a href="https://github.com/readmedotmd">readmedotmd</a></sub>
</p>
