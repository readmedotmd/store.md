# Sync Store

The `sync/core` package wraps any `Store` implementation with a synchronization layer. It enables peer-to-peer data replication with timestamp-based conflict resolution.

## How It Works

When you write a value through `StoreSync`, three keys are written to the underlying store:

| Key | Purpose |
|-----|---------|
| `%sync%view%{key}` | Maps user key to the latest value ID |
| `%sync%value%{id}` | Stores the full item as JSON |
| `%sync%queue%{writeTime}%{uuid}%{key}` | Ordered queue entry for sync |

The queue enables efficient incremental sync — peers track their cursor position and only fetch new entries.

## SyncStore Interface

The `sync/core` package defines a `SyncStore` interface that captures the full sync-capable API. All sync store implementations (`StoreSync`, `StoreMessage`) implement it:

```go
type SyncStore interface {
    storemd.Store
    GetItem(ctx context.Context, key string) (*SyncStoreItem, error)
    SetItem(ctx context.Context, app, key, value string) error
    ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]SyncStoreItem, error)
    OnUpdate(fn UpdateListener) func()
    Sync(ctx context.Context, peerID string, incoming *SyncPayload) (*SyncPayload, error)
    Close() error
}
```

The `Sync` method drives the sync protocol. The server and client packages work with any `SyncStore`. Call `Close()` when done to release resources and stop background processing.

## Store Interface

`StoreSync` implements both `storemd.Store` and `SyncStore`, so it can be used anywhere either interface is expected:

```go
ctx := context.Background()
ss := core.New(store)
defer ss.Close()
var _ storemd.Store = ss    // compiles
var _ core.SyncStore = ss   // compiles

ss.Set(ctx, "greeting", "hello world")    // Store interface
val, _ := ss.Get(ctx, "greeting")         // returns string
```

## Quick Start

```go
import (
    "context"

    "github.com/readmedotmd/store.md/backend/bbolt"
    "github.com/readmedotmd/store.md/sync/core"
)

store, _ := bbolt.New("data.db")
defer store.Close()

ss := core.New(store)
defer ss.Close()

ctx := context.Background()

// Write with sync metadata
ss.SetItem(ctx, "myapp", "greeting", "hello world")

// Read full item
item, err := ss.GetItem(ctx, "greeting")
// item.App, item.Key, item.Value, item.Timestamp, item.ID

// List items
items, _ := ss.ListItems(ctx, "", "", 0)
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

`Delete` writes a **tombstone** — a `SyncStoreItem` with `Deleted: true` and an empty value. Tombstones sync like any other write and propagate to all peers.

```go
ctx := context.Background()
ss.SetItem(ctx, "app", "config", "value")
ss.Delete(ctx, "config")

_, err := ss.Get(ctx, "config")       // returns ErrNotFound
_, err = ss.GetItem(ctx, "config")    // returns ErrNotFound
items, _ := ss.List(ctx, storemd.ListArgs{})  // excludes "config"
```

`Get`, `GetItem`, `List`, and `ListItems` all filter out tombstoned items. Deleting a key that doesn't exist (or is already deleted) returns `ErrNotFound`.

Tombstones participate in conflict resolution — a delete with an older timestamp won't overwrite a newer write, and vice versa.

## Syncing Between Peers

The `Sync` method handles both sending and receiving data in a single call:

```go
ctx := context.Background()

// Initiate a sync exchange (no incoming data)
payload, err := store1.Sync(ctx, "peer-2-id", nil)
// payload contains items to send to the peer

// Process received data and respond
response, err := store2.Sync(ctx, "peer-1-id", payload)
// response contains items to send back (nil when done)
```

Each call to `Sync` with incoming data applies items through conflict resolution — older items won't overwrite newer local data. When `Sync` returns nil, the exchange is complete.

### Full Example

```go
ctx := context.Background()

store1, _ := bbolt.New("node1.db")
store2, _ := bbolt.New("node2.db")
sync1 := core.New(store1)
sync2 := core.New(store2)
defer sync1.Close()
defer sync2.Close()

// Node 1 writes data
sync1.SetItem(ctx, "app", "config", `{"theme":"dark"}`)

// Node 1 initiates sync
outgoing, _ := sync1.Sync(ctx, "node2", nil)

// Node 2 processes and responds
response, _ := sync2.Sync(ctx, "node1", outgoing)

// Node 1 processes response (if any)
if response != nil {
    sync1.Sync(ctx, "node2", response)
}

// Node 2 now has the data
item, _ := sync2.GetItem(ctx, "config")
fmt.Println(item.Value) // {"theme":"dark"}
```

## Update Listeners

Register callbacks that fire whenever an item is successfully written — via `SetItem` or `Sync`. Writes that are rejected by conflict resolution (older timestamp) do not trigger listeners.

```go
unsub := ss.OnUpdate(func(item core.SyncStoreItem) {
    fmt.Printf("updated: %s = %s\n", item.Key, item.Value)
})

ctx := context.Background()
ss.SetItem(ctx, "app", "config", "new-value")
// prints: updated: config = new-value

// Stop listening
unsub()
```

`OnUpdate` returns an unsubscribe function. Multiple listeners can be registered and they are called in registration order. Listeners are called synchronously after the write completes — avoid blocking operations inside them.

---

## Hooks

StoreSync supports hooks that allow customizing sync behavior without modifying the core protocol:

### PreSetItem Hook

Called before an item is written to the store. Can modify the item (e.g., add signatures) before storage. The item's Timestamp, ID, and WriteTimestamp are already set when this hook is called.

```go
ss := core.NewWithOptions(store,
    core.WithPreSetItem(func(item *core.SyncStoreItem) {
        // Add signature
        item.PublicKey = myPublicKey
        item.Signature = sign(item)
    }),
)
```

Multiple hooks are called in order; each hook receives the (possibly modified) item from the previous hook.

### SyncOutFilter Hook

Decides whether an item should be included in a SyncOut payload for a given peer:

```go
ss := core.NewWithOptions(store,
    core.WithSyncOutFilter(func(item core.SyncStoreItem, peerID string) bool {
        // Only send "chat" app to peer1
        if peerID == "peer1" {
            return item.App == "chat"
        }
        return true
    }),
)
```

### SyncInFilter Hook

Decides how an incoming item should be handled during SyncIn:

```go
ss := core.NewWithOptions(store,
    core.WithSyncInFilter(func(item core.SyncStoreItem, peerID string) bool {
        // Reject items without signatures
        return item.Signature != ""
    }),
)
```

Return `true` to persist normally, `false` to skip persistence (listeners are still notified).

### PostSyncOut Hook

Called after a successful SyncOut with the items that were included:

```go
ss := core.NewWithOptions(store,
    core.WithPostSyncOut(func(ctx context.Context, items []core.SyncStoreItem, peerID string) {
        // Log what was sent
        log.Printf("Sent %d items to %s", len(items), peerID)
    }),
)
```

---

## Time Offset

The `StoreSync` constructor accepts an optional time offset (in nanoseconds):

```go
ss := core.New(store, int64(10 * time.Second)) // default: 10 seconds
```

The offset is added to `time.Now()` to compute `WriteTimestamp`. This creates a buffer window — items written within the offset period are considered "in-flight" and won't advance the sync cursor until their write time has passed. This prevents items from being skipped during concurrent writes.

## Per-Peer Cursors

SyncOut maintains per-peer timestamp cursors:

- `%sync%lastsyncout%{peerID}` — tracks the last timestamp sent to a peer

This means syncing with peer A doesn't affect the cursor for peer B.

---

## Sync Limits

### MaxSyncInItems

Each `Sync` call accepts a maximum of 5000 items per payload. Payloads exceeding this limit are rejected. This prevents a single sync exchange from consuming excessive memory or processing time.

### MaxClockSkew

Incoming items with timestamps more than 5 minutes in the future are rejected. This prevents clock manipulation attacks where a peer could set far-future timestamps to always win conflict resolution.

---

## Pluggable Transports

The sync server supports pluggable transports for handling different network protocols. WebSocket is enabled by default; HTTP POST can be added for environments where WebSocket is not available.

### Transport Interface

```go
type Transport interface {
    CanHandle(r *http.Request) bool
    Serve(w http.ResponseWriter, r *http.Request, peerID string, store storesync.SyncStore, adapter *client.Client, logger *slog.Logger) error
}
```

Two built-in transports are provided:

| Transport | Protocol | Behavior |
|-----------|----------|----------|
| `WebSocketTransport` | WebSocket | Persistent bidirectional connection. Blocks until the connection closes. |
| `HTTPTransport` | HTTP POST | Stateless request-response. Each POST carries one sync message and receives one response. |

### Enabling HTTP Transport

```go
srv := server.New(store, auth)
srv.EnableHTTP() // accept both WebSocket and HTTP POST
```

Clients send sync messages as HTTP POST requests with a JSON body:

```
POST /sync HTTP/1.1
Authorization: Bearer my-token
Content-Type: application/json

{"type":"sync","payload":{"items":[...]}}
```

### Custom Transports

Implement the `Transport` interface and register it with `AddTransport`:

```go
srv.AddTransport(myCustomTransport)
```

Transports are tried in registration order; the first whose `CanHandle` returns true is used.

---

## Security

### Use TLS in Production

Always use `ListenAndServeTLS` for production deployments to encrypt sync traffic:

```go
srv.ListenAndServeTLS(":8443", "cert.pem", "key.pem")
```

### Configure CORS Origins

Never use `"*"` as a CORS origin in production. Configure allowed origins explicitly to prevent unauthorized cross-origin access.

### Token Authentication with Expiry

Use `TokenAuthWithExpiry` instead of `TokenAuth` for token rotation support. Expired tokens are rejected, reducing the window of exposure if a token is compromised.

### Connection Limits

The sync server enforces connection limits to protect against resource exhaustion from excessive simultaneous connections.

### Timestamp Validation

Items with future timestamps beyond the `MaxClockSkew` threshold (5 minutes) are rejected during sync, preventing clock manipulation attacks that could exploit last-write-wins conflict resolution.
