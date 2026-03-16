// Chat client using gui.md for UI.
//
// Build with:
//
//	GOOS=js GOARCH=wasm go build -o chat.wasm

//go:build js && wasm
// +build js,wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"syscall/js"
	"time"

	"github.com/readmedotmd/store.md/backend/indexeddb"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/client"
	"github.com/readmedotmd/store.md/sync/core"

	gui "github.com/readmedotmd/gui.md"
	"github.com/readmedotmd/gui.md/dom"
)

// ChatMessage represents a chat message
type ChatMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

var (
	peerID    string
	idbStore  *indexeddb.StoreIndexedDB
	memStore  *memory.StoreMemory
	syncStore *core.StoreSync
	app       *dom.App

	// Connection management
	connMu     sync.Mutex
	wantConn   bool
	syncClient *client.Client
	wsConn     *nativeWebSocket

	// UI state
	messages    []ChatMessage
	inputText   string
	statusText  string
	statusClass string
)

func main() {
	peerID = fmt.Sprintf("user-%d", time.Now().UnixNano())
	statusText = "Initializing..."
	statusClass = "syncing"

	container := js.Global().Get("document").Call("getElementById", "app")
	if container.IsUndefined() || container.IsNull() {
		body := js.Global().Get("document").Get("body")
		container = js.Global().Get("document").Call("createElement", "div")
		container.Set("id", "app")
		body.Call("appendChild", container)
	}

	app = dom.NewApp(container, func() gui.Node {
		return App()
	})

	go initStore()
	app.Run()
}

func initStore() {
	var err error
	idbStore, err = indexeddb.New("chat-gui-db-v8")
	if err != nil {
		setStatus("Storage error: "+err.Error(), "disconnected")
		return
	}

	// Use memory store for sync (not IndexedDB directly) to avoid WASM deadlocks
	// IndexedDB operations in goroutines can cause deadlocks in WASM/JS environment
	memStore = memory.New()
	syncStore = core.New(memStore)

	syncStore.OnUpdate(func(item core.SyncStoreItem) {
		if len(item.Key) > 4 && item.Key[:4] == "msg/" {
			var msg ChatMessage
			if err := json.Unmarshal([]byte(item.Value), &msg); err == nil {
				go func(m ChatMessage) {
					data, _ := json.Marshal(m)
					ctx := context.Background()
					idbStore.Set(ctx, "msg/"+m.ID, string(data))
				}(msg)

				addMessageUnique(msg)
			}
		}
	})

	loadFromIndexedDB()
	setStatus("Ready", "disconnected")
}

func addMessageUnique(msg ChatMessage) {
	for _, existing := range messages {
		if existing.ID == msg.ID {
			return
		}
	}
	messages = append(messages, msg)
	app.Render()
}

func loadFromIndexedDB() {
	go func() {
		ctx := context.Background()
		pairs, err := idbStore.List(ctx, struct {
			Prefix     string
			StartAfter string
			Limit      int
		}{Prefix: "msg/"})

		if err != nil {
			return
		}

		for _, p := range pairs {
			var msg ChatMessage
			if err := json.Unmarshal([]byte(p.Value), &msg); err == nil {
				messages = append(messages, msg)
				syncStore.SetItem(ctx, "chat", p.Key, p.Value)
			}
		}
		app.Render()
	}()
}

func connectOnce() {
	connMu.Lock()
	if wsConn != nil || !wantConn {
		connMu.Unlock()
		return
	}
	connMu.Unlock()

	wsURL := "ws://" + js.Global().Get("location").Get("host").String() + "/sync?peer=" + peerID
	log("Starting connection to: " + wsURL)

	newWs, err := dialWebSocket(peerID, wsURL, http.Header{})
	if err != nil {
		log("WebSocket failed: " + err.Error())
		setStatus("Connection failed", "disconnected")
		
		// Retry
		go func() {
			time.Sleep(3 * time.Second)
			connMu.Lock()
			stillWant := wantConn
			connMu.Unlock()
			if stillWant {
				connectOnce()
			}
		}()
		return
	}

	// Create client and add connection
	newClient := client.New(syncStore)
	if err := newClient.AddConnection(newWs); err != nil {
		log("AddConnection failed: " + err.Error())
		newWs.Close()
		setStatus("Connection failed", "disconnected")
		return
	}

	// Success
	connMu.Lock()
	wsConn = newWs.(*nativeWebSocket)
	syncClient = newClient
	connMu.Unlock()
	
	setStatus("Connected", "connected")
	app.Render()

	// Monitor connection
	go monitorConnection()
}

func monitorConnection() {
	for {
		time.Sleep(2 * time.Second)
		
		connMu.Lock()
		ws := wsConn
		shouldBeConnected := wantConn
		connMu.Unlock()

		if !shouldBeConnected {
			return
		}

		if ws == nil || ws.closed {
			connMu.Lock()
			wsConn = nil
			syncClient = nil
			connMu.Unlock()
			
			setStatus("Reconnecting...", "syncing")
			app.Render()
			
			time.Sleep(1 * time.Second)
			connectOnce()
			return
		}
	}
}

func disconnect() {
	wantConn = false
	
	connMu.Lock()
	ws := wsConn
	client := syncClient
	wsConn = nil
	syncClient = nil
	connMu.Unlock()

	if client != nil {
		go client.Close()
	}
	if ws != nil {
		go ws.Close()
	}

	setStatus("Disconnected", "disconnected")
	app.Render()
}

func toggleConnection() {
	connMu.Lock()
	isConnected := wsConn != nil
	connMu.Unlock()

	if isConnected {
		disconnect()
	} else {
		wantConn = true
		setStatus("Connecting...", "syncing")
		app.Render()
		go connectOnce()
	}
}

func isConnected() bool {
	connMu.Lock()
	defer connMu.Unlock()
	return wsConn != nil && !wsConn.closed
}

func sendMessage() {
	if inputText == "" {
		return
	}

	content := inputText
	inputText = ""
	app.Render()

	go func() {
		msg := ChatMessage{
			ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
			From:      peerID,
			Content:   content,
			Timestamp: time.Now().UnixMilli(),
		}

		data, _ := json.Marshal(msg)

		ctx := context.Background()
		idbStore.Set(ctx, "msg/"+msg.ID, string(data))
		syncStore.SetItem(ctx, "chat", "msg/"+msg.ID, string(data))

		addMessageUnique(msg)
	}()
}

func setStatus(text, class string) {
	statusText = text
	statusClass = class
}

func log(msg string) {
	js.Global().Get("console").Call("log", "[Chat] "+msg)
}

// UI Components

func App() gui.Node {
	return gui.Div(gui.Class("app"))(
		Header(),
		MessageList(),
		InputArea(),
	)
}

func Header() gui.Node {
	connected := isConnected()

	label := "Connect"
	if connected {
		label = "Disconnect"
	}

	return gui.Header(gui.Class("header"))(
		gui.H1()(gui.Text("store.md Chat")),
		gui.Div(gui.Class("status-bar"))(
			gui.Span(gui.Class(statusClass))(gui.Text(statusText)),
			gui.Button(
				gui.Class("conn-btn"),
				gui.On("click", func(e gui.Event) {
					toggleConnection()
				}),
			)(gui.Text(label)),
		),
	)
}

func MessageList() gui.Node {
	children := make([]gui.Node, 0, len(messages))
	for _, msg := range messages {
		children = append(children, MessageItem(msg))
	}
	return gui.Div(
		gui.Class("messages"),
		gui.Id("message-list"),
	)(children...)
}

func MessageItem(msg ChatMessage) gui.Node {
	msgClass := "message other"
	if msg.From == peerID {
		msgClass = "message own"
	}
	timeStr := time.UnixMilli(msg.Timestamp).Format("15:04")

	return gui.Div(gui.Class(msgClass))(
		gui.Div(gui.Class("from"))(gui.Text(msg.From)),
		gui.Div(gui.Class("content"))(gui.Text(msg.Content)),
		gui.Div(gui.Class("time"))(gui.Text(timeStr)),
	)
}

func InputArea() gui.Node {
	return gui.Div(gui.Class("input-area"))(
		gui.Input(
			gui.Class("input"),
			gui.Placeholder("Type a message..."),
			gui.Value(inputText),
			gui.On("input", func(e gui.Event) {
				inputText = e.Value
				app.Render()
			}),
			gui.On("keydown", func(e gui.Event) {
				if e.Key == "Enter" {
					sendMessage()
				}
			}),
		)(),
		gui.Button(
			gui.Class("send-btn"),
			gui.On("click", func(e gui.Event) {
				sendMessage()
			}),
		)(gui.Text("Send")),
	)
}
