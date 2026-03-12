# Client Adapter

The `sync/client` package is a sync adapter that connects a local `SyncStore` to one or more remote peers over WebSocket. It handles the full sync protocol for both client-side and server-side roles.

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

Three implementations are provided:

- **`Dial`** — creates a client-side WebSocket connection (default)
- **`NewConn`** — wraps an already-upgraded `*websocket.Conn` (used by the server package)
- **`HTTPDialer`** — creates an HTTP POST-based connection for environments without WebSocket

## Quick Start

```go
import (
    "net/http"

    "github.com/readmedotmd/store.md/backend/bbolt"
    "github.com/readmedotmd/store.md/sync/client"
    "github.com/readmedotmd/store.md/sync/core"
)

store, _ := bbolt.New("local.db")
defer store.Close()
ss := core.New(store)
defer ss.Close()

c := client.New(ss)
defer c.Close()

header := http.Header{}
header.Set("Authorization", "Bearer my-token")

// Connect to a remote sync server
err := c.Connect("my-peer-id", "ws://localhost:8080", header)
```

## Pluggable Dialers

The client uses a `Dialer` to create outbound connections. By default it uses WebSocket, but you can switch to HTTP or provide a custom dialer:

```go
// Dialer type
type Dialer func(peerID, url string, header http.Header) (Connection, error)
```

### HTTP Dialer

Use `HTTPDialer` to sync over HTTP POST when WebSocket is not available. The server must have HTTP transport enabled (`srv.EnableHTTP()`).

```go
c := client.New(ss, client.WithDialer(client.HTTPDialer()))
err := c.Connect("peer-id", "http://server:8080/sync", header)
```

HTTP connections poll the server at a configurable interval (default 5s) to discover server-pushed changes:

```go
// Poll every 2 seconds
c := client.New(ss, client.WithDialer(client.HTTPDialer(2 * time.Second)))
```

### Custom Dialer

Implement the `Dialer` function signature to use any transport:

```go
myDialer := func(peerID, url string, header http.Header) (client.Connection, error) {
    // Create and return a Connection implementation
}
c := client.New(ss, client.WithDialer(myDialer))
```

## Active vs Passive Connections

The adapter supports two types of connections:

### Active (Client-side)

Created via `Connect` or by calling `Dial` + `AddConnection` with the active flag. Active connections:

- Initiate a sync exchange on connect
- Automatically push local changes to all peers via `OnUpdate`

```go
// Dial + connect in one call
c.Connect("peer-id", "ws://server:8080", header)
```

### Passive (Server-side)

Created via `AddConnection`. Passive connections:

- Initiate a sync exchange on connect to pull any existing data
- Receive pushed items from the remote peer
- Push items to the remote peer when local data changes

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

Data written locally is pushed to all connections. Data received from one connection is pushed to all other connections.

## Protocol

The adapter uses a single message type:

| Message | Direction | Behavior |
|---------|-----------|----------|
| `sync` | Both | Carries a `SyncPayload` with items the peer hasn't seen. Calls `store.Sync(peerID, payload)` and sends the response if non-nil. |

Sync is entirely event-driven — there is no polling. When local data changes (`OnUpdate`), the adapter immediately pushes the new items to all connected peers by calling `initiateSync` on each connection. When items arrive from a peer, they are applied locally and then pushed to all other connections.

### Structured Logging

The client adapter supports structured logging via `slog`. Pass a logger to capture connection events, sync errors, and protocol messages:

```go
c := client.New(ss, client.WithLogger(slog.Default()))
```

### Connection Limits

The sync server enforces connection limits. If the limit is reached, new connections are rejected. Clients should handle connection errors and implement backoff/retry logic.

## How the Server Uses It

The `server` package creates a `client.Client` (adapter) per store. When a WebSocket connection is accepted, the server wraps it with `client.NewConn` and adds it as a passive connection:

```go
// Inside server.ServeHTTP (simplified)
conn := client.NewConn(ws, peerID)
adapter.AddConnection(conn)
```

The adapter handles all protocol logic — the server itself has no sync protocol code. This means the same adapter code handles syncing on both the client and server side.
