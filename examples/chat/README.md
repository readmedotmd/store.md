# Chat Example - Browser → Server → All Browsers

A real-time chat application demonstrating **store.md** with:
- **gui.md** for reactive UI
- **IndexedDB** for browser persistence  
- **BBolt** for server persistence
- **WebSocket sync** for real-time messaging

## Architecture

```
┌─────────────────┐         WebSocket          ┌─────────────────┐
│   Browser A     │ ◄───────────────────────►  │     Server      │
│  ┌───────────┐  │                          │  ┌───────────┐  │
│  │  gui.md   │  │                          │  │   BBolt   │  │
│  │   (UI)    │  │                          │  │  (store)  │  │
│  └─────┬─────┘  │                          │  └─────┬─────┘  │
│  ┌─────┴─────┐  │                          │  ┌─────┴─────┐  │
│  │ IndexedDB │  │                          │  │   sync    │  │
│  │ (persist) │  │                          │  │  (broadcast)│  │
│  └───────────┘  │                          │  └───────────┘  │
└─────────────────┘                          └─────────────────┘
         │                                             │
         │         WebSocket                           │
         │ ◄─────────────────────────────────────────► │
         │                                             │
┌─────────────────┐                                    │
│   Browser B     │ ◄─────────────────────────────────┘
│  ┌───────────┐  │         (broadcast)
│  │  gui.md   │  │
│  │   (UI)    │  │
│  └─────┬─────┘  │
│  ┌─────┴─────┐  │
│  │ IndexedDB │  │
│  │ (persist) │  │
│  └───────────┘  │
└─────────────────┘
```

## Features

- **Real-time messaging** via WebSocket sync protocol
- **Persistent storage** - messages survive browser refresh (IndexedDB) and server restart (BBolt)
- **Reactive UI** - built with gui.md virtual DOM
- **Connect/Disconnect toggle** - join/leave chat without losing messages
- **Cross-browser** - works in Chrome, Firefox, Safari

## Running the Example

### 1. Start the Server

```bash
cd examples/chat
make
```

Or manually:
```bash
cd client
GOOS=js GOARCH=wasm go build -o ../server/client/chat.wasm

cd ../server
go run main.go
```

### 2. Open in Browser

Open `http://localhost:8090` in multiple browser tabs or different browsers.

### 3. Chat!

1. Click **"Connect"** to join the chat
2. Type a message and click **Send** (or press Enter)
3. See messages appear in all connected browsers
4. Click **"Disconnect"** to leave (messages stay in IndexedDB)

## Browser Compatibility

| Browser | Status | Notes |
|---------|--------|-------|
| Chrome  | ✅ Full support | Best experience |
| Edge    | ✅ Full support | Chromium-based |
| Firefox | ✅ Works | See [Firefox Notes](#firefox-notes) below |
| Safari  | ✅ Should work | Not extensively tested |

### Firefox Notes

Firefox has stricter WebSocket/WASM callback handling. The example uses a workaround:
- JS callbacks are kept in a global registry and never released
- This prevents "call to released function" crashes
- Trade-off: small memory leak (4 callbacks per connection)

## Project Structure

```
examples/chat/
├── client/
│   ├── main.go          # Main app with gui.md UI
│   └── websocket.go     # Firefox-compatible WebSocket wrapper
├── server/
│   └── main.go          # HTTP server + WebSocket sync
├── Makefile             # Build automation
└── README.md            # This file
```

## Key Code Patterns

### UI with gui.md

```go
func App() gui.Node {
    return gui.Div(gui.Class("app"))(
        Header(),
        MessageList(),
        InputArea(),
    )
}
```

### Sync Store Setup

```go
memStore := memory.New()
syncStore := core.New(memStore)

syncStore.OnUpdate(func(item core.SyncStoreItem) {
    // Handle incoming messages
})
```

### WebSocket Connection

```go
wsURL := "ws://localhost:8090/sync?peer=" + peerID
conn, err := dialWebSocket(peerID, wsURL, nil)
if err != nil {
    // Handle error
}

client := client.New(syncStore)
client.AddConnection(conn)
```

## Troubleshooting

### "Connection failed" error

- Check server is running: `curl http://localhost:8090/health`
- Check browser console for detailed errors
- Try refreshing the page

### Messages not syncing

- Ensure both browsers clicked **Connect**
- Check browser console for WebSocket errors
- Server logs show all connections

### Firefox "call to released function"

This is a known Firefox/WASM issue. The example includes a workaround that keeps callbacks alive. Refresh the page if you see this error.

## Architecture Details

### Why Memory Store for Sync?

The sync client uses an in-memory store (`memory.New()`) instead of IndexedDB directly:

1. **Avoids WASM deadlocks** - IndexedDB operations in goroutines can deadlock in WASM
2. **Better performance** - sync operations are fast in memory
3. **IndexedDB for persistence** - messages are still saved to IndexedDB on updates

### Message Flow

1. User types message → UI updates
2. `sendMessage()` saves to:
   - IndexedDB (persistence)
   - Memory store (triggers sync)
3. Sync client broadcasts to server
4. Server broadcasts to other clients
5. Other clients receive → save to IndexedDB → update UI

## License

MIT - Same as store.md
