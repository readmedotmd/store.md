# Pluggable Transport System

The sync system supports three transport methods for connecting clients and servers: **WebSocket**, **HTTP POST**, and **Server-Sent Events (SSE)**. All three can run simultaneously on the same server, and a single client can connect to different servers using different transports.

## Two Server Types

The transport system works with two server types that share the same transports:

| | **Server** | **InboxServer** |
|---|---|---|
| Backend | `core.SyncStore` | `store.Store` (plain KV) |
| Sync model | Full bidirectional sync with conflict resolution | Store-and-forward relay |
| Data handling | Reads, merges, and resolves conflicts | Stores opaquely, never reads payloads |
| Use case | Server needs to read/query the data | Relay for end-to-end encrypted data |
| Cleanup | Automatic via sync cursors | Manual via `Cleanup()` |

Both implement the `PeerHandler` interface, which is how transports interact with the server:

```go
type PeerHandler interface {
    // Single request-response exchange (used by HTTP POST transport).
    HandleMessage(ctx, peerID, msg) (resp, ack, err)

    // Persistent bidirectional connection (used by WebSocket, SSE transports).
    ServeConnection(peerID, conn) error
}
```

Transports call `HandleMessage` for stateless exchanges (HTTP POST) or `ServeConnection` for persistent connections (WebSocket, SSE). The server type determines what happens with the data.

## Architecture Overview

```
┌────────────────────────────────────┐
│    Server or InboxServer           │
│                                    │
│  ┌─────────┐ ┌──────┐ ┌─────┐     │
│  │WebSocket│ │ HTTP │ │ SSE │     │
│  │Transport│ │Trans.│ │Trans│     │
│  └────┬────┘ └──┬───┘ └──┬──┘     │
│       └─────────┼────────┘        │
│           PeerHandler              │
│       ┌─────────┼────────┐        │
│  SyncStore  or  store.Store        │
└────────────────────────────────────┘
       ▲          ▲         ▲
       │ws://     │http://  │sse://
┌──────┴──┐ ┌────┴───┐ ┌───┴────┐
│Client A │ │Client B│ │Client C│
│(WS Dial)│ │(HTTP)  │ │(SSE)   │
└─────────┘ └────────┘ └────────┘
```

## Transport Comparison

| Feature | WebSocket | HTTP POST | SSE |
|---------|-----------|-----------|-----|
| Direction | Bidirectional | Request-response | Server push + POST |
| Connection | Persistent | Stateless (polling) | Persistent read, POST write |
| Browser support | Native API | Fetch/XHR | EventSource API |
| Firewall friendly | Sometimes blocked | Always works | Always works |
| Latency | Lowest | Depends on poll interval | Low (server push) |
| Overhead | Lowest | Highest (per-request) | Medium |

## Server Examples

### Basic: Single Transport (WebSocket)

WebSocket is enabled by default when you create a server:

```go
package main

import (
    "log"
    "net/http"

    "github.com/readmedotmd/store.md/backend/memory"
    "github.com/readmedotmd/store.md/sync/core"
    "github.com/readmedotmd/store.md/sync/server"
)

func main() {
    store := memory.New()
    ss := core.New(store)
    defer ss.Close()

    // Token-based auth: map tokens to peer IDs.
    tokens := map[string]string{
        "secret-token-alice": "alice",
        "secret-token-bob":   "bob",
    }
    srv := server.New(ss, server.TokenAuth(tokens))
    srv.SetAllowedOrigins([]string{"https://myapp.example.com"})

    log.Fatal(http.ListenAndServe(":8080", srv))
}
```

Clients connect via `ws://localhost:8080` with `Authorization: Bearer secret-token-alice`.

### All Three Transports via Auto-Dispatch

Enable all transports on the same endpoint. The server inspects each request to route it to the right transport:

```go
srv := server.New(ss, server.TokenAuth(tokens))
srv.EnableHTTP()  // Accept POST requests
srv.EnableSSE()   // Accept SSE streams + SSE POST

log.Fatal(http.ListenAndServe(":8080", srv))
```

The server auto-detects:
- WebSocket upgrade requests → WebSocket transport
- `GET` with `Accept: text/event-stream` → SSE transport
- `POST` with `X-Transport: sse` → SSE transport (feeds into active SSE session)
- `POST` (plain) → HTTP transport

### Route-Based Dispatch (Recommended for Production)

Mount each transport on its own route for explicit control. This integrates cleanly with any Go HTTP mux:

```go
srv := server.New(ss, server.TokenAuth(tokens))

mux := http.NewServeMux()
mux.Handle("/api/sync/ws", srv.Handler(srv.WebSocket()))
mux.Handle("/api/sync/sse", srv.Handler(srv.SSE()))
mux.Handle("/api/sync/http", srv.Handler(srv.HTTP()))

// Mix with your own application routes.
mux.HandleFunc("/api/users", handleUsers)
mux.Handle("/", http.FileServer(http.Dir("static")))

log.Fatal(http.ListenAndServe(":8080", mux))
```

### Plugging into an Existing Backend

The `Handler()` method returns a standard `http.Handler`, so it works with any router:

```go
// With gorilla/mux
r := mux.NewRouter()
r.Handle("/sync/ws", srv.Handler(srv.WebSocket()))
r.Handle("/sync/sse", srv.Handler(srv.SSE()))

// With chi
r := chi.NewRouter()
r.Handle("/sync/ws", srv.Handler(srv.WebSocket()))
r.Handle("/sync/sse", srv.Handler(srv.SSE()))

// With gin (wrap as http.Handler)
router := gin.Default()
router.Any("/sync/ws", gin.WrapH(srv.Handler(srv.WebSocket())))
router.Any("/sync/sse", gin.WrapH(srv.Handler(srv.SSE())))
```

### Multi-Store Server

Serve multiple independent stores from one server. Each store has its own data and sync state:

```go
stores := map[string]*core.StoreSync{
    "project-alpha": core.New(memory.New()),
    "project-beta":  core.New(memory.New()),
}

resolver := func(storeID string) (core.SyncStore, error) {
    ss, ok := stores[storeID]
    if !ok {
        return nil, fmt.Errorf("unknown store: %s", storeID)
    }
    return ss, nil
}

srv := server.NewMulti(resolver, server.TokenAuth(tokens))

mux := http.NewServeMux()
// Clients connect to /sync/project-alpha, /sync/project-beta, etc.
mux.Handle("/sync/", srv)

// Or with route-based transport dispatch:
mux.Handle("/ws/", srv.Handler(srv.WebSocket()))   // /ws/project-alpha
mux.Handle("/sse/", srv.Handler(srv.SSE()))         // /sse/project-alpha
```

### Server with Hooks

Use hooks for custom authorization, logging, or connection management:

```go
srv := server.New(ss, server.TokenAuth(tokens))

// Reject connections based on custom logic.
srv.OnPeerConnect(func(peerID string, r *http.Request) error {
    if isBanned(peerID) {
        return fmt.Errorf("peer is banned") // → 403 Forbidden
    }
    log.Printf("peer %s connected from %s", peerID, r.RemoteAddr)
    return nil
})

// Log disconnections.
srv.OnPeerDisconnect(func(peerID string) {
    log.Printf("peer %s disconnected", peerID)
})

// Limit concurrent connections.
srv.SetMaxConnsPerPeer(3)   // Max 3 tabs/devices per user
srv.SetMaxTotalConns(500)   // Max 500 total connections
```

## InboxServer Examples

InboxServer is a store-and-forward relay that uses a plain `store.Store` instead of `SyncStore`. It stores items opaquely — no decryption or conflict resolution — and forwards them to other peers.

### Basic Inbox Server

```go
package main

import (
    "log"
    "net/http"

    "github.com/readmedotmd/store.md/backend/memory"
    "github.com/readmedotmd/store.md/sync/server"
)

func main() {
    // Uses a plain store — no SyncStore, no sync layer overhead.
    store := memory.New()

    tokens := map[string]string{
        "alice-token": "alice",
        "bob-token":   "bob",
    }
    inbox := server.NewInbox(store, server.TokenAuth(tokens))
    inbox.SetAllowedOrigins([]string{"https://myapp.example.com"})

    log.Fatal(http.ListenAndServe(":8080", inbox))
}
```

Clients connect using `InboxClient` (see [InboxClient Examples](#inboxclient-examples)) or a raw WebSocket.

### Inbox with All Transports

```go
inbox := server.NewInbox(store, server.TokenAuth(tokens))
inbox.EnableHTTP()  // Accept HTTP POST polling
inbox.EnableSSE()   // Accept SSE streams

// Auto-dispatch: WS upgrades → WebSocket, SSE → SSE, POST → HTTP.
log.Fatal(http.ListenAndServe(":8080", inbox))
```

### Inbox with Route-Based Dispatch

```go
inbox := server.NewInbox(store, server.TokenAuth(tokens))

mux := http.NewServeMux()
mux.Handle("/ws", inbox.Handler(inbox.WebSocket()))
mux.Handle("/sse", inbox.Handler(inbox.SSE()))
mux.Handle("/http", inbox.Handler(inbox.HTTP()))

log.Fatal(http.ListenAndServe(":8080", mux))
```

### Inbox with Persistent Storage

```go
// Use BBolt for durable message storage across server restarts.
store, _ := bbolt.New("inbox.db")
defer store.Close()

inbox := server.NewInbox(store, server.TokenAuth(tokens))
```

### Inbox with Hooks and Cleanup

```go
inbox := server.NewInbox(store, server.TokenAuth(tokens))

inbox.OnPeerConnect(func(peerID string, r *http.Request) error {
    log.Printf("peer %s connected", peerID)
    return nil
})

inbox.OnPeerDisconnect(func(peerID string) {
    log.Printf("peer %s disconnected", peerID)
})

inbox.SetMaxConnsPerPeer(3)
inbox.SetMaxTotalConns(500)

// Periodically clean up delivered items.
go func() {
    for range time.Tick(5 * time.Minute) {
        inbox.Cleanup(context.Background())
    }
}()
```

### When to Use InboxServer vs Server

Use **InboxServer** when:
- The server is a relay/proxy for end-to-end encrypted data
- You don't need the server to read, query, or merge item payloads
- You want minimal server-side overhead (no sync layer, no GC workers)
- Items should be deleted after delivery

Use **Server** when:
- The server needs to read and query synced data
- You need conflict resolution (last-writer-wins)
- You want full bidirectional sync with per-key views
- Multiple clients share a canonical data set

## InboxClient Examples

`InboxClient` connects to an `InboxServer` without requiring a local `SyncStore`. It sends and receives raw items via a callback — ideal for relays, chat, or end-to-end encrypted messaging where the client doesn't need cursor-based sync.

### Basic InboxClient (WebSocket)

```go
ic := client.NewInbox(func(peerID string, items []core.SyncStoreItem) {
    for _, item := range items {
        fmt.Printf("received %s = %s\n", item.Key, item.Value)
    }
})
defer ic.Close()

header := http.Header{}
header.Set("Authorization", "Bearer alice-token")

ic.Connect("server", "ws://localhost:8080", header)

// Send items to the inbox server for relay to other peers.
ic.Send([]core.SyncStoreItem{
    {App: "chat", Key: "msg-1", Value: "Hello!", ID: "uuid-1"},
})
```

### InboxClient with HTTP Polling

```go
ic := client.NewInbox(onReceive,
    client.WithInboxDialer(client.HTTPDialer(2 * time.Second)),
)
defer ic.Close()

ic.Connect("server", "http://localhost:8080", header)
```

### InboxClient with SSE

```go
ic := client.NewInbox(onReceive,
    client.WithInboxDialer(client.SSEDialer()),
)
defer ic.Close()

ic.Connect("server", "http://localhost:8080/sse", header)
```

### InboxClient with Multiple Transports

Connect to different servers using different transports from a single client:

```go
ic := client.NewInbox(onReceive,
    client.WithInboxDialerForScheme("sse", client.SSEDialer()),
    client.WithInboxDialerForScheme("http", client.HTTPDialer()),
)
defer ic.Close()

ic.Connect("relay-a", "ws://relay-a:8080", headerA)
ic.Connect("relay-b", "sse://relay-b:8080/sse", headerB)
ic.Connect("relay-c", "http://relay-c:8080/http", headerC)
```

### InboxClient with Reconnection

```go
ic := client.NewInbox(onReceive,
    client.WithInboxReconnect(1*time.Second, 30*time.Second),
    client.WithInboxHeaderProvider(func(peerID, url string) (http.Header, error) {
        token, err := refreshToken()
        if err != nil {
            return nil, err
        }
        h := http.Header{}
        h.Set("Authorization", "Bearer "+token)
        return h, nil
    }),
    client.OnInboxConnectError(func(peerID string, err error) bool {
        if errors.Is(err, client.ErrAuthFailed) {
            return false // stop retrying
        }
        return true
    }),
)
```

### When to Use InboxClient vs Client

| | **Client** | **InboxClient** |
|---|---|---|
| Requires | `core.SyncStore` | Callback function |
| Sync model | Full bidirectional sync with cursors | Fire-and-forget send + callback receive |
| Local storage | Syncs items into local store | No local store — items delivered to callback |
| Use case | Apps that need offline-capable local replicas | Relays, chat, E2E encrypted messaging |
| Connects to | `Server` or `InboxServer` | `InboxServer` |

## Client Examples

### WebSocket (Default)

```go
store := memory.New()
ss := core.New(store)
defer ss.Close()

c := client.New(ss)
defer c.Close()

header := http.Header{}
header.Set("Authorization", "Bearer secret-token-alice")

err := c.Connect("server-peer", "ws://localhost:8080/ws", header)
if err != nil {
    log.Fatal(err)
}

// Data syncs automatically. Write locally and it appears on the server:
ss.SetItem(ctx, "notes", "note-1", "Hello from Alice")
```

### HTTP POST (Polling)

Use when WebSocket is unavailable (corporate proxies, serverless, etc.):

```go
c := client.New(ss, client.WithDialer(client.HTTPDialer(2 * time.Second)))
defer c.Close()

err := c.Connect("server-peer", "http://localhost:8080/http", header)
```

The client polls the server every 2 seconds for changes. Writes are sent immediately via POST.

### SSE (Server-Sent Events)

Server pushes changes instantly; client sends via POST:

```go
c := client.New(ss, client.WithDialer(client.SSEDialer()))
defer c.Close()

err := c.Connect("server-peer", "http://localhost:8080/sse", header)
```

### Multiple Transports from One Client

Connect to different servers using different transports:

```go
c := client.New(ss,
    client.WithDialerForScheme("sse", client.SSEDialer()),
    client.WithDialerForScheme("http", client.HTTPDialer()),
    // Default dialer (WebSocket) handles ws:// and wss://
)
defer c.Close()

// WebSocket to server A
c.Connect("server-a", "ws://server-a:8080/ws", headerA)

// SSE to server B (sse:// is rewritten to http:// automatically)
c.Connect("server-b", "sse://server-b:8080/sse", headerB)

// HTTP polling to server C
c.Connect("server-c", "http://server-c:8080/http", headerC)

// All three connections sync to the same local store.
// Data from any server appears locally and is relayed to the others.
```

### Custom URL Schemes

The `sse://` and `sses://` schemes are convenience aliases that map to `http://` and `https://` when dialing:

```go
c := client.New(ss,
    client.WithDialerForScheme("sse", client.SSEDialer()),
)

// These are equivalent:
c.Connect("peer", "sse://host:8080/sse", header)   // → dials http://host:8080/sse
c.Connect("peer", "sses://host:443/sse", header)    // → dials https://host:443/sse
```

### Reconnection with Token Refresh

For long-lived connections that need fresh auth tokens on reconnect:

```go
c := client.New(ss,
    client.WithReconnect(1*time.Second, 30*time.Second),
    client.WithHeaderProvider(func(peerID, url string) (http.Header, error) {
        token, err := refreshToken()
        if err != nil {
            return nil, err
        }
        h := http.Header{}
        h.Set("Authorization", "Bearer "+token)
        return h, nil
    }),
    client.OnConnectError(func(peerID string, err error) bool {
        if errors.Is(err, client.ErrAuthFailed) {
            log.Println("Auth failed, stopping retries")
            return false // stop reconnecting
        }
        return true // keep retrying transient errors
    }),
)
```

## Browser (WASM) Client

In WASM, the client uses the browser's native WebSocket API. Browser WebSockets don't support custom headers, so auth typically goes in the URL:

```go
//go:build js && wasm

import (
    "github.com/readmedotmd/store.md/backend/indexeddb"
    "github.com/readmedotmd/store.md/sync/client"
    "github.com/readmedotmd/store.md/sync/core"
)

func main() {
    store, _ := indexeddb.New("myapp")
    ss := core.New(store)

    c := client.New(ss)

    // In WASM, Dial uses the browser's WebSocket API.
    // Auth via query param since browser WS doesn't support custom headers.
    c.Connect("browser-peer", "ws://localhost:8080/ws?token=secret", nil)

    // Keep the WASM alive.
    select {}
}
```

The server authenticates browsers via query parameter using `TokenAuthWithParam`:

```go
tokens := map[string]string{
    "alice-token": "alice",
    "bob-token":   "bob",
}

// Checks Authorization header first, falls back to ?token= query param.
auth := server.TokenAuthWithParam("token", tokens)
srv := server.New(ss, auth)
```

Build the WASM binary:
```bash
GOOS=js GOARCH=wasm go build -o app.wasm
```

## Full Example: Notes App

A complete example with a Go server and Go client syncing notes:

### Server

```go
package main

import (
    "log"
    "net/http"

    "github.com/readmedotmd/store.md/backend/bbolt"
    "github.com/readmedotmd/store.md/sync/core"
    "github.com/readmedotmd/store.md/sync/server"
)

func main() {
    store, _ := bbolt.New("notes.db")
    defer store.Close()

    ss := core.New(store)
    defer ss.Close()

    tokens := map[string]string{
        "alice-token": "alice",
        "bob-token":   "bob",
    }
    srv := server.New(ss, server.TokenAuth(tokens))
    srv.SetAllowedOrigins([]string{"*"})

    // Route-based dispatch — each transport on its own path.
    mux := http.NewServeMux()
    mux.Handle("/ws", srv.Handler(srv.WebSocket()))
    mux.Handle("/sse", srv.Handler(srv.SSE()))
    mux.Handle("/http", srv.Handler(srv.HTTP()))

    log.Println("Notes server on :8080")
    log.Fatal(http.ListenAndServe(":8080", mux))
}
```

### Client A (Desktop, WebSocket)

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "time"

    "github.com/readmedotmd/store.md/backend/bbolt"
    "github.com/readmedotmd/store.md/sync/client"
    "github.com/readmedotmd/store.md/sync/core"
)

func main() {
    store, _ := bbolt.New("alice-notes.db")
    defer store.Close()

    ss := core.New(store)
    defer ss.Close()

    header := http.Header{}
    header.Set("Authorization", "Bearer alice-token")

    c := client.New(ss,
        client.WithReconnect(1*time.Second, 30*time.Second),
    )
    defer c.Close()

    if err := c.Connect("server", "ws://localhost:8080/ws", header); err != nil {
        panic(err)
    }

    // Write a note — it syncs to the server automatically.
    ctx := context.Background()
    ss.SetItem(ctx, "notes", "meeting-2024", "Discuss transport layer refactor")

    // Read notes (including those synced from other clients).
    time.Sleep(time.Second)
    val, _ := ss.Get(ctx, "meeting-2024")
    fmt.Println("Note:", val)

    select {} // keep running
}
```

### Client B (IoT device, HTTP polling)

```go
func main() {
    store := memory.New()
    ss := core.New(store)
    defer ss.Close()

    header := http.Header{}
    header.Set("Authorization", "Bearer bob-token")

    c := client.New(ss,
        client.WithDialer(client.HTTPDialer(10 * time.Second)),
    )
    defer c.Close()

    c.Connect("server", "http://localhost:8080/http", header)

    // Polls every 10 seconds. Alice's notes appear automatically.
    time.Sleep(15 * time.Second)
    val, _ := ss.Get(context.Background(), "meeting-2024")
    fmt.Println("Synced note:", val)
}
```

### Client C (Web dashboard, SSE)

```go
func main() {
    store := memory.New()
    ss := core.New(store)
    defer ss.Close()

    header := http.Header{}
    header.Set("Authorization", "Bearer bob-token")

    c := client.New(ss,
        client.WithDialer(client.SSEDialer()),
    )
    defer c.Close()

    c.Connect("server", "http://localhost:8080/sse", header)

    // Server pushes Alice's notes instantly via SSE.
    time.Sleep(time.Second)
    val, _ := ss.Get(context.Background(), "meeting-2024")
    fmt.Println("SSE synced note:", val)
}
```

## SSE Wire Format

SSE events use the `sync` event type:

```
event: sync
data: {"type":"sync","payload":{"items":[{"app":"notes","key":"meeting","value":"...","ts":1234567890}]}}

```

Each event is a JSON-encoded `client.Message`. The client parses `data:` lines, joins multi-line data, and unmarshals the JSON.

## PeerHandler Interface

The `PeerHandler` interface decouples transports from sync semantics. Transports handle the network protocol (WebSocket upgrade, HTTP request/response, SSE event stream), while the handler determines what to do with the data.

```go
type PeerHandler interface {
    HandleMessage(ctx context.Context, peerID string, msg client.Message) (client.Message, func(), error)
    ServeConnection(peerID string, conn client.Connection) error
}
```

**HandleMessage** processes a single request-response exchange. Returns the response message and an `ack` function. The transport calls `ack()` after successfully delivering the response — this advances the sync cursor so items aren't re-sent. If delivery fails, the cursor stays put and items are retried next time.

**ServeConnection** manages a persistent bidirectional connection. It reads incoming messages, processes them, and pushes new items to the peer as they arrive from other peers. Blocks until the connection closes.

Both `Server` and `InboxServer` implement this interface, which is why the same three transports work with either server type.

## SessionTransport (SSE Implementation Detail)

SSE uses two HTTP request types: a long-lived GET for the event stream and short POSTs for client-to-server messages. The `SessionTransport` interface tells the server that SSE POST requests belong to an existing session and should not:

- Acquire a new connection slot
- Trigger `OnPeerConnect` hooks
- Trigger `OnPeerDisconnect` hooks

This prevents SSE POST requests from counting against per-peer connection limits. Only the initial GET stream acquires a connection slot. The same behavior applies to both `Server` and `InboxServer`.
