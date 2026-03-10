# Sync Store

The `sync` package wraps any `Store` implementation with a synchronization layer. It enables peer-to-peer data replication with timestamp-based conflict resolution.

## How It Works

When you write a value through `StoreSync`, three keys are written to the underlying store:

| Key | Purpose |
|-----|---------|
| `%sync%view%{key}` | Maps user key to the latest value ID |
| `%sync%value%{id}` | Stores the full item as JSON |
| `%sync%queue%{writeTime}%{uuid}%{key}` | Ordered queue entry for sync |

The queue enables efficient incremental sync — peers track their cursor position and only fetch new entries.

## SyncStore Interface

The `sync` package defines a `SyncStore` interface that captures the full sync-capable API. Both `StoreSync` and `StoreMessage` implement it:

```go
type SyncStore interface {
    storemd.Store
    GetItem(key string) (*SyncStoreItem, error)
    SetItem(app, key, value string) error
    ListItems(prefix, startAfter string, limit int) ([]SyncStoreItem, error)
    OnUpdate(fn UpdateListener) func()
    SyncIn(peerID string, payload SyncPayload) error
    SyncOut(peerID string, limit int) (*SyncPayload, error)
}
```

This interface is used by the server and client adapter packages, allowing them to work with either a `StoreSync` or a `StoreMessage` interchangeably.

## Store Interface

`StoreSync` implements both `storemd.Store` and `SyncStore`, so it can be used anywhere either interface is expected:

```go
ss := sync.New(store)
var _ storemd.Store = ss    // compiles
var _ sync.SyncStore = ss   // compiles

ss.Set("greeting", "hello world")    // Store interface
val, _ := ss.Get("greeting")         // returns string
```

## Quick Start

```go
import (
    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/sync"
)

store, _ := bbolt.New("data.db")
defer store.Close()

ss := sync.New(store)

// Write with sync metadata
ss.SetItem("myapp", "greeting", "hello world")

// Read full item
item, err := ss.GetItem("greeting")
// item.App, item.Key, item.Value, item.Timestamp, item.ID

// List items
items, _ := ss.ListItems("", "", "")
```

## SyncStoreItem

Each stored item contains:

```go
type SyncStoreItem struct {
    App            string `json:"app"`
    Key            string `json:"key"`
    Value          string `json:"value"`
    Timestamp      int64  `json:"timestamp"`      // creation time (UnixNano)
    ID             string `json:"id"`              // UUID
    WriteTimestamp int64  `json:"writeTimestamp"`   // queue ordering time (UnixNano)
    Deleted        bool   `json:"deleted,omitempty"` // tombstone flag
}
```

## Conflict Resolution

When setting a value, the sync store checks if an existing item has a newer `Timestamp`. If it does, the write is silently skipped. **Last-write-wins** based on creation timestamp.

## Deletes

`Delete` writes a **tombstone** — a `SyncStoreItem` with `Deleted: true` and an empty value. Tombstones enter the sync queue like any other write and propagate to all peers via `SyncOut`/`SyncIn`.

```go
ss.SetItem("app", "config", "value")
ss.Delete("config")

_, err := ss.Get("config")       // returns NotFoundError
_, err = ss.GetItem("config")    // returns NotFoundError
items, _ := ss.List(storemd.ListArgs{})  // excludes "config"
```

`Get`, `GetItem`, `List`, and `ListItems` all filter out tombstoned items. Deleting a key that doesn't exist (or is already deleted) returns `NotFoundError`.

Tombstones participate in conflict resolution — a delete with an older timestamp won't overwrite a newer write, and vice versa.

## Syncing Between Peers

### SyncOut — Send data to a peer

```go
payload, err := store1.SyncOut("peer-2-id", "100") // limit 100 items
// payload.Items contains the items to send
// payload.LastSyncTimestamp is the cursor position
```

`SyncOut` tracks per-peer cursors internally. Each call returns only items added since the last sync to that peer.

### SyncIn — Receive data from a peer

```go
err := store2.SyncIn("peer-1-id", payload)
```

Each item goes through the same conflict resolution — older items won't overwrite newer local data.

### Full Example

```go
store1, _ := bbolt.New("node1.db")
store2, _ := bbolt.New("node2.db")
sync1 := sync.New(store1)
sync2 := sync.New(store2)

// Node 1 writes data
sync1.SetItem("app", "config", `{"theme":"dark"}`)

// Node 1 sends to Node 2
payload, _ := sync1.SyncOut("node2", "")
sync2.SyncIn("node1", *payload)

// Node 2 now has the data
item, _ := sync2.GetItem("config")
fmt.Println(item.Value) // {"theme":"dark"}
```

## Update Listeners

Register callbacks that fire whenever an item is successfully written — via `SetItem` or `SyncIn`. Writes that are rejected by conflict resolution (older timestamp) do not trigger listeners.

```go
unsub := ss.OnUpdate(func(item sync.SyncStoreItem) {
    fmt.Printf("updated: %s = %s\n", item.Key, item.Value)
})

ss.SetItem("app", "config", "new-value")
// prints: updated: config = new-value

// Stop listening
unsub()
```

`OnUpdate` returns an unsubscribe function. Multiple listeners can be registered and they are called in registration order. Listeners are called synchronously after the write completes — avoid blocking operations inside them.

---

## Time Offset

The `StoreSync` constructor accepts an optional time offset (in nanoseconds):

```go
ss := sync.New(store, int64(10 * time.Second)) // default: 10 seconds
```

The offset is added to `time.Now()` to compute `WriteTimestamp`. This creates a buffer window — items written within the offset period are considered "in-flight" and won't advance the sync cursor until their write time has passed. This prevents items from being skipped during concurrent writes.

## Per-Peer Cursors

SyncIn and SyncOut maintain independent timestamp cursors per peer:

- `%sync%lastsyncin%{peerID}` — tracks the last timestamp received from a peer
- `%sync%lastsyncout%{peerID}` — tracks the last timestamp sent to a peer

This means syncing with peer A doesn't affect the cursor for peer B.
