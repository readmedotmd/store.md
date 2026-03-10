# Client Adapter

The `client` package is a sync adapter that connects a local `SyncStore` to one or more remote peers over WebSocket. It handles the full sync protocol for both client-side and server-side roles.

## Connection Interface

All sync communication goes through the `Connection` interface:

```go
type Connection interface {
    PeerID() string
    ReadMessage() (Message, error)
    WriteMessage(msg Message) error
    Close() error
}
```

Two implementations are provided:

- **`Dial`** — creates a client-side connection by dialing a WebSocket server
- **`NewConn`** — wraps an already-upgraded `*websocket.Conn` (used by the server package)

## Quick Start

```go
import (
    "net/http"

    "github.com/readmedotmd/store.md/bbolt"
    "github.com/readmedotmd/store.md/client"
    "github.com/readmedotmd/store.md/sync"
)

store, _ := bbolt.New("local.db")
defer store.Close()
ss := sync.New(store)

c := client.New(ss, client.WithInterval(5*time.Second))
defer c.Close()

header := http.Header{}
header.Set("Authorization", "Bearer my-token")

// Connect to a remote sync server
err := c.Connect("my-peer-id", "ws://localhost:8080", header)
```

## Active vs Passive Connections

The adapter supports two types of connections:

### Active (Client-side)

Created via `Connect` or by calling `Dial` + `AddConnection` with the active flag. Active connections:

- Send an initial `sync_request` on connect
- Run a periodic pull loop (configurable interval)
- Automatically push local changes via `OnUpdate`

```go
// Dial + connect in one call
c.Connect("peer-id", "ws://server:8080", header)
```

### Passive (Server-side)

Created via `AddConnection`. Passive connections:

- Only respond to incoming `sync_request` and `sync_push` messages
- Do **not** initiate pulls or push local changes automatically
- Broadcast `sync_update` notifications to other connections when new data arrives

```go
// Used by the server package internally
conn := client.NewConn(ws, peerID)
c.AddConnection(conn)
```

## Multiple Connections

A single adapter can manage multiple connections simultaneously. This enables:

- Connecting to multiple servers from one client
- Accepting multiple peers on the server side
- Mesh topologies where a node is both client and server

```go
c := client.New(ss)

// Connect to two different servers
c.Connect("peer-a", "ws://server-a:8080", headerA)
c.Connect("peer-b", "ws://server-b:8080", headerB)
```

Data written locally is pushed to all active connections. Data received from one connection is broadcast (via `sync_update`) to all other connections.

## Protocol

The adapter handles both sides of the sync protocol in a single read loop:

| Message | Direction | Behavior |
|---------|-----------|----------|
| `sync_request` | Incoming | Calls `SyncOut` and responds with `sync_response` |
| `sync_push` | Incoming | Calls `SyncIn`, responds with `sync_ack`, broadcasts `sync_update` to others |
| `sync_response` | Incoming | Calls `SyncIn` to apply received data |
| `sync_ack` | Incoming | No-op (push acknowledged) |
| `sync_update` | Incoming | Sends a `sync_request` to pull new data |

## Options

```go
// Set the pull interval (default: 5 seconds)
client.WithInterval(10 * time.Second)

// Limit items per sync request (default: 0 = no limit)
client.WithLimit(100)
```

## How the Server Uses It

The `server` package creates a `client.Client` (adapter) per store. When a WebSocket connection is accepted, the server wraps it with `client.NewConn` and adds it as a passive connection:

```go
// Inside server.ServeHTTP (simplified)
conn := client.NewConn(ws, peerID)
adapter.AddConnection(conn)
```

The adapter handles all protocol logic — the server itself has no sync protocol code. This means the same adapter code handles syncing on both the client and server side.
