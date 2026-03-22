# Ephemeral Messaging

The `sync/ephemeral` package provides targeted, non-persistent message passing built on top of StoreSync hooks. Messages target a specific peer, are never persisted on the receiver, and are deleted from the sender's store after delivery.

## How It Works

Ephemeral messaging is implemented entirely through three StoreSync hooks — no changes to the core sync protocol.

| Hook | Behavior |
|------|----------|
| `SyncOutFilter` | Only includes ephemeral items when syncing to the target peer |
| `SyncInFilter` | Dispatches to an in-memory handler; skips persistence |
| `PostSyncOut` | Deletes the item from the sender's store after delivery |

### Key Convention

Ephemeral messages use the key prefix `%eph%` followed by the target peer ID and a unique message ID:

```
%eph%{targetPeerID}%{messageID}
```

Non-ephemeral keys pass through all hooks unmodified.

### Message Lifecycle

```
Sender                          Receiver
  │                                │
  │  eph.Send(ctx, "B", ...)       │
  │  ┌───────────────────┐         │
  │  │ Write to sync     │         │
  │  │ queue (outbox)     │         │
  │  └───────────────────┘         │
  │                                │
  │  ── sync cycle ──────────────> │
  │  SyncOutFilter: include        │
  │  only for target peer          │
  │                                │
  │                   SyncInFilter: │
  │                   dispatch to   │
  │                   handler, skip │
  │                   persistence   │
  │                                │
  │  PostSyncOut: delete           │
  │  from sender's store           │
  │                                │
  │  Nothing remains on            │
  │  either side.                  │
```

## Backing Store

The choice of backing store controls durability of the sender's outbox:

| Backing store | Sender outbox | Use case |
|---------------|---------------|----------|
| `memory.New()` | RAM only — lost on restart | Transient signals, presence, typing indicators |
| `bbolt.New(...)` | Disk — survives restart | Alerts, notifications that must eventually arrive |

In both cases, messages are **never** persisted on the receiver.

## Setup

The Layer uses a two-phase initialization because hooks are registered at StoreSync construction time:

```go
import (
    "github.com/readmedotmd/store.md/backend/memory"
    "github.com/readmedotmd/store.md/sync/core"
    "github.com/readmedotmd/store.md/sync/ephemeral"
)

// 1. Create the layer
eph := ephemeral.New("my-node-id")

// 2. Create the StoreSync with the layer's hooks
ss := core.NewWithOptions(memory.New(), eph.Options()...)

// 3. Bind the StoreSync reference back to the layer
eph.Bind(ss)
```

## Sending

```go
ctx := context.Background()
err := eph.Send(ctx, "target-node", "chat", "hello world")
```

`Send` writes a JSON-encoded `Envelope` to the sync queue with an ephemeral key. The message is delivered to the target peer on the next sync cycle.

## Receiving

```go
eph.Handle("chat", func(msg ephemeral.Envelope) {
    fmt.Printf("from %s: %s\n", msg.From, msg.Data)
})
```

Handlers are registered by message type. One handler per type — registering a second handler for the same type replaces the first.

## Envelope

```go
type Envelope struct {
    ID        string `json:"id"`        // unique message ID (UUID)
    From      string `json:"from"`      // sender's node ID
    MsgType   string `json:"msgType"`   // message type (matches Handle)
    Data      string `json:"data"`      // payload
    CreatedAt int64  `json:"createdAt"` // UnixNano timestamp
}
```

## Deduplication

A bounded seen-set prevents duplicate handler invocations if a message is re-delivered. The default capacity is 10,000 entries with FIFO eviction.

```go
eph := ephemeral.New("my-node", ephemeral.WithSeenCapacity(50000))
```

## Wiring with Client and Server

The ephemeral layer works with the existing client and server packages. The client's sync loop automatically delivers ephemeral messages as part of normal sync traffic.

```go
import (
    "net/http"

    "github.com/readmedotmd/store.md/backend/memory"
    "github.com/readmedotmd/store.md/sync/client"
    "github.com/readmedotmd/store.md/sync/core"
    "github.com/readmedotmd/store.md/sync/ephemeral"
)

// Set up ephemeral layer with in-memory store
eph := ephemeral.New("my-node")
ss := core.NewWithOptions(memory.New(), eph.Options()...)
eph.Bind(ss)

// Register handlers
eph.Handle("ping", func(msg ephemeral.Envelope) {
    fmt.Println("got ping from", msg.From)
})

// Connect to a server — ephemeral messages flow through normal sync
c := client.New(ss)
defer c.Close()

header := http.Header{}
header.Set("Authorization", "Bearer my-token")
c.Connect("server-peer", "ws://localhost:8080", header)

// Send ephemeral message to a specific peer
eph.Send(ctx, "other-node", "ping", "are you there?")
```

## Comparison with Message Store

| | Ephemeral | Message Store |
|-|-----------|--------------|
| Direction | One-way (fire-and-forget) | Request/response |
| Persistence | Never on receiver; deleted on sender after delivery | Persisted on both sides |
| Response | No | Yes (sender blocks until response) |
| Offline support | Configurable via backing store | Always (sync-based) |
| Use case | Notifications, signals, presence | RPC, commands, queries |
