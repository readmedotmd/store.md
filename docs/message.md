# Message Store

The `sync/message` package adds request/response messaging on top of the sync store. Messages are transported through the sync layer — they sync between peers like any other data.

## How It Works

Each `StoreMessage` has a unique ID. To send a message, you specify the target store's ID, a message type, and a payload. The target's registered handler processes the request and returns a response.

Under the hood, messages are written as sync store items with reserved key prefixes:

| Key | Purpose |
|-----|---------|
| `%msg%req%{targetID}%{messageID}` | Request addressed to a target |
| `%msg%res%{messageID}` | Response correlated to a request |

Both requests and responses carry an `Envelope`:

```go
type Envelope struct {
    MessageID string `json:"messageID"`
    SenderID  string `json:"senderID"`
    Type      string `json:"type"`
    Data      string `json:"data"`
    Error     string `json:"error,omitempty"`
}
```

## Store Interface

`StoreMessage` implements the `core.SyncStore` interface (which embeds `storemd.Store`), so it can be used anywhere a `Store` or `SyncStore` is expected — including the server and client adapter packages.

```go
var _ core.SyncStore = (*StoreMessage)(nil)
```

## Quick Start

```go
import (
    "context"
    "fmt"

    "github.com/readmedotmd/store.md/backend/bbolt"
    "github.com/readmedotmd/store.md/sync/core"
    "github.com/readmedotmd/store.md/sync/message"
)

store, _ := bbolt.New("data.db")
defer store.Close()
ss := core.New(store)
defer ss.Close()

// Create two message stores on the same sync store
alice := message.New(ss, "alice")
defer alice.Close()
bob := message.New(ss, "bob")
defer bob.Close()

// Register a handler on Bob
bob.Handle("greet", func(msg message.Envelope) (string, error) {
    return "hello " + msg.Data, nil
})

// Alice sends a message to Bob and gets a response
ctx := context.Background()
resp, err := alice.Send(ctx, "bob", "greet", "world")
fmt.Println(resp) // hello world
```

## API

### New

```go
func New(syncStore *core.StoreSync, id string) *StoreMessage
```

Creates a message store with the given ID. The ID should be a known constant so other stores can address it.

### Send

```go
func (m *StoreMessage) Send(ctx context.Context, targetID, msgType, data string) (string, error)
```

Sends a message to `targetID` and blocks until a response arrives or `ctx` is cancelled. Returns the response data or an error.

### Handle

```go
func (m *StoreMessage) Handle(msgType string, handler Handler)
```

Registers a handler for the given message type. One handler per type. When a matching request arrives, the handler is called and its return value is sent back as the response.

```go
type Handler func(msg Envelope) (responseData string, err error)
```

If the handler returns an error, the sender's `Send` call returns that error.

### OnMessage

```go
func (m *StoreMessage) OnMessage(fn MessageListener) func()
```

Registers a listener that fires only for incoming requests (not regular data writes). Returns an unsubscribe function.

```go
type MessageListener func(msg Envelope)
```

### Close

```go
func (m *StoreMessage) Close()
```

Unsubscribes from the sync store and cancels any pending `Send` calls. Always call `Close()` when the message store is no longer needed to prevent resource leaks.

## Cross-Store Messaging

Messages work across separate stores connected via sync. When Store A sends a message, it writes a request item to the sync store. After syncing, the request reaches Store B, which processes it and writes a response. After another sync round, the response reaches Store A.

```go
// Two stores on separate backends
store1, _ := bbolt.New("node1.db")
store2, _ := bbolt.New("node2.db")
ss1 := core.New(store1)
ss2 := core.New(store2)
defer ss1.Close()
defer ss2.Close()

a := message.New(ss1, "a")
defer a.Close()
b := message.New(ss2, "b")
defer b.Close()

b.Handle("ping", func(msg message.Envelope) (string, error) {
    return "pong", nil
})

// Send from A (writes request to ss1)
// Then sync ss1 <-> ss2 to deliver the request and response
```

## Listener Isolation

- `OnMessage` listeners fire **only** for incoming requests addressed to this store
- `OnUpdate` (from the sync layer) fires for **all** writes, including message traffic
- Regular data writes (`Set`, `SetItem`) do **not** trigger `OnMessage` listeners
