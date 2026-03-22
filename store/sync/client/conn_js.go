//go:build js && wasm

package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"syscall/js"
)

// jsConn implements Connection using the browser's native WebSocket API.
type jsConn struct {
	ws     js.Value
	peerID string
	msgCh  chan Message
	errCh  chan error
	done   chan struct{}
	once   sync.Once

	// prevent GC of JS callbacks
	onMessage js.Func
	onError   js.Func
	onClose   js.Func
}

func (c *jsConn) PeerID() string { return c.peerID }

func (c *jsConn) ReadMessage() (Message, error) {
	select {
	case msg := <-c.msgCh:
		return msg, nil
	case err := <-c.errCh:
		return Message{}, err
	case <-c.done:
		return Message{}, fmt.Errorf("connection closed")
	}
}

func (c *jsConn) WriteMessage(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	select {
	case <-c.done:
		return fmt.Errorf("connection closed")
	default:
	}
	c.ws.Call("send", string(data))
	return nil
}

func (c *jsConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.ws.Call("close")
		c.onMessage.Release()
		c.onError.Release()
		c.onClose.Release()
	})
	return nil
}

func (c *jsConn) release() {
	c.once.Do(func() {
		close(c.done)
		c.onMessage.Release()
		c.onError.Release()
		c.onClose.Release()
	})
}

// Dial creates a client-side Connection using the browser's WebSocket API.
// Note: browser WebSockets do not support custom HTTP headers; the header
// parameter is accepted for API compatibility but ignored.
func Dial(peerID, url string, header http.Header) (Connection, error) {
	ws := js.Global().Get("WebSocket").New(url)

	conn := &jsConn{
		ws:     ws,
		peerID: peerID,
		msgCh:  make(chan Message, 64),
		errCh:  make(chan error, 1),
		done:   make(chan struct{}),
	}

	openCh := make(chan struct{})
	errCh := make(chan error, 1)

	onOpen := js.FuncOf(func(this js.Value, args []js.Value) any {
		close(openCh)
		return nil
	})

	conn.onMessage = js.FuncOf(func(this js.Value, args []js.Value) any {
		data := args[0].Get("data").String()
		var msg Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			select {
			case conn.errCh <- fmt.Errorf("unmarshal: %w", err):
			default:
			}
			return nil
		}
		select {
		case conn.msgCh <- msg:
		case <-conn.done:
		}
		return nil
	})

	conn.onError = js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case conn.errCh <- fmt.Errorf("websocket error"):
		default:
		}
		select {
		case errCh <- fmt.Errorf("websocket error"):
		default:
		}
		return nil
	})

	conn.onClose = js.FuncOf(func(this js.Value, args []js.Value) any {
		closeErr := closeError(args)
		select {
		case conn.errCh <- closeErr:
		default:
		}
		return nil
	})

	ws.Call("addEventListener", "open", onOpen)
	ws.Call("addEventListener", "message", conn.onMessage)
	ws.Call("addEventListener", "error", conn.onError)
	ws.Call("addEventListener", "close", conn.onClose)

	// Wait for connection to open or fail.
	select {
	case <-openCh:
		onOpen.Release()
		return conn, nil
	case err := <-errCh:
		onOpen.Release()
		conn.release()
		return nil, fmt.Errorf("dial: %w", err)
	}
}

// closeError inspects the CloseEvent's code and returns ErrAuthFailed for
// auth-related close codes (4401, 1008), so OnConnectError hooks can
// distinguish auth failures from transient network errors.
func closeError(args []js.Value) error {
	if len(args) > 0 {
		code := args[0].Get("code").Int()
		if code == 4401 || code == 1008 {
			return fmt.Errorf("websocket closed (code %d): %w", code, ErrAuthFailed)
		}
		return fmt.Errorf("websocket closed (code %d)", code)
	}
	return fmt.Errorf("websocket closed")
}
