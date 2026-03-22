//go:build js && wasm
// +build js,wasm

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"syscall/js"
	"time"

	"github.com/readmedotmd/store.md/sync/client"
)

// nativeWebSocket wraps the browser's native WebSocket API
// WARNING: In Firefox, we NEVER release js.Func callbacks to avoid
// "call to released function" errors. This is a memory leak but prevents crashes.
type nativeWebSocket struct {
	ws       js.Value
	peerID   string
	msgCh    chan client.Message
	errCh    chan error
	closed   bool
	closeMu  sync.Mutex
}

func (c *nativeWebSocket) PeerID() string { return c.peerID }

func (c *nativeWebSocket) ReadMessage() (client.Message, error) {
	select {
	case msg := <-c.msgCh:
		return msg, nil
	case err := <-c.errCh:
		return client.Message{}, err
	}
}

func (c *nativeWebSocket) WriteMessage(msg client.Message) error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return fmt.Errorf("connection closed")
	}
	c.closeMu.Unlock()
	
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	c.ws.Call("send", string(data))
	return nil
}

func (c *nativeWebSocket) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	c.closeMu.Unlock()
	
	c.ws.Call("close")
	return nil
}

// Global callback registry to prevent GC and "released function" errors
var callbackRegistry = struct {
	sync.Mutex
	callbacks []js.Func
}{}

func registerCallback(cb js.Func) {
	callbackRegistry.Lock()
	callbackRegistry.callbacks = append(callbackRegistry.callbacks, cb)
	callbackRegistry.Unlock()
}

// dialWebSocket creates a WebSocket connection
// NOTE: Due to Firefox WASM/WebSocket bugs, we intentionally leak callbacks
func dialWebSocket(peerID, url string, header http.Header) (client.Connection, error) {
	js.Global().Get("console").Call("log", "[WebSocket] Creating connection to:", url)
	
	ws := js.Global().Get("WebSocket").New(url)
	
	conn := &nativeWebSocket{
		ws:     ws,
		peerID: peerID,
		msgCh:  make(chan client.Message, 64),
		errCh:  make(chan error, 2),
	}

	openCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	var once sync.Once

	onOpen := js.FuncOf(func(this js.Value, args []js.Value) any {
		js.Global().Get("console").Call("log", "[WebSocket] Connection opened")
		once.Do(func() { close(openCh) })
		return nil
	})

	onMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		data := args[0].Get("data").String()
		var msg client.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			select {
			case conn.errCh <- fmt.Errorf("unmarshal: %w", err):
			default:
			}
			return nil
		}
		select {
		case conn.msgCh <- msg:
		default:
		}
		return nil
	})

	onError := js.FuncOf(func(this js.Value, args []js.Value) any {
		js.Global().Get("console").Call("log", "[WebSocket] Error occurred")
		once.Do(func() { 
			select {
			case errCh <- fmt.Errorf("websocket error"):
			default:
			}
		})
		return nil
	})

	onClose := js.FuncOf(func(this js.Value, args []js.Value) any {
		js.Global().Get("console").Call("log", "[WebSocket] Connection closed")
		conn.closeMu.Lock()
		wasClosed := conn.closed
		conn.closed = true
		conn.closeMu.Unlock()
		
		if !wasClosed {
			select {
			case conn.errCh <- fmt.Errorf("websocket closed"):
			default:
			}
		}
		return nil
	})

	// Register callbacks to prevent GC and "released function" errors in Firefox
	registerCallback(onOpen)
	registerCallback(onMessage)
	registerCallback(onError)
	registerCallback(onClose)

	ws.Call("addEventListener", "open", onOpen)
	ws.Call("addEventListener", "message", onMessage)
	ws.Call("addEventListener", "error", onError)
	ws.Call("addEventListener", "close", onClose)

	// Wait for connection with timeout
	select {
	case <-openCh:
		return conn, nil
	case err := <-errCh:
		ws.Call("close")
		return nil, fmt.Errorf("dial: %w", err)
	case <-time.After(10 * time.Second):
		ws.Call("close")
		return nil, fmt.Errorf("dial: timeout")
	}
}
