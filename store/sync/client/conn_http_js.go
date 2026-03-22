//go:build js && wasm

package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	gosync "sync"
	"syscall/js"
	"time"

	storesync "github.com/readmedotmd/store.md/sync/core"
)

// httpConn implements Connection over HTTP POST requests using the browser's
// XMLHttpRequest API. It is the browser-compatible equivalent of the native
// httpConn in conn_http.go. Outgoing sync messages are buffered and sent with
// the next ReadMessage call. When no outgoing messages are pending, ReadMessage
// polls the server at a regular interval to discover server-initiated changes.
type httpConn struct {
	peerID       string
	url          string
	header       http.Header
	pollInterval time.Duration

	outgoing chan Message
	done     chan struct{}
	once     gosync.Once
}

func (c *httpConn) PeerID() string { return c.peerID }

func (c *httpConn) WriteMessage(msg Message) error {
	select {
	case c.outgoing <- msg:
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed")
	}
}

// ReadMessage waits for an outgoing message or polls the server after the
// poll interval. It sends the message via XMLHttpRequest and returns the response.
func (c *httpConn) ReadMessage() (Message, error) {
	var msg Message
	select {
	case msg = <-c.outgoing:
	case <-time.After(c.pollInterval):
		// Poll with empty sync payload to check for server-side changes.
		msg = Message{Type: "sync", Payload: &storesync.SyncPayload{}}
	case <-c.done:
		return Message{}, fmt.Errorf("connection closed")
	}
	return c.doPost(msg)
}

func (c *httpConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

type xhrResult struct {
	body string
	err  error
}

// doPost sends a sync message via XMLHttpRequest and returns the response.
func (c *httpConn) doPost(msg Message) (Message, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return Message{}, fmt.Errorf("marshal: %w", err)
	}

	ch := make(chan xhrResult, 1)
	xhr := js.Global().Get("XMLHttpRequest").New()
	xhr.Call("open", "POST", c.url, true)
	xhr.Call("setRequestHeader", "Content-Type", "application/json")
	for k, vs := range c.header {
		for _, v := range vs {
			xhr.Call("setRequestHeader", k, v)
		}
	}
	xhr.Set("timeout", 30000) // 30s timeout

	var onLoad, onError, onTimeout js.Func
	onLoad = js.FuncOf(func(this js.Value, args []js.Value) any {
		defer onLoad.Release()
		defer onError.Release()
		defer onTimeout.Release()
		status := xhr.Get("status").Int()
		respText := xhr.Get("responseText").String()
		if status == 401 || status == 403 {
			ch <- xhrResult{err: fmt.Errorf("server returned %d: %w", status, ErrAuthFailed)}
			return nil
		}
		if status != 200 {
			ch <- xhrResult{err: fmt.Errorf("server returned %d", status)}
			return nil
		}
		ch <- xhrResult{body: respText}
		return nil
	})
	onError = js.FuncOf(func(this js.Value, args []js.Value) any {
		defer onLoad.Release()
		defer onError.Release()
		defer onTimeout.Release()
		ch <- xhrResult{err: fmt.Errorf("network error")}
		return nil
	})
	onTimeout = js.FuncOf(func(this js.Value, args []js.Value) any {
		defer onLoad.Release()
		defer onError.Release()
		defer onTimeout.Release()
		ch <- xhrResult{err: fmt.Errorf("request timeout")}
		return nil
	})
	xhr.Set("onload", onLoad)
	xhr.Set("onerror", onError)
	xhr.Set("ontimeout", onTimeout)
	xhr.Call("send", string(data))

	result := <-ch
	if result.err != nil {
		return Message{}, result.err
	}

	var response Message
	if err := json.Unmarshal([]byte(result.body), &response); err != nil {
		return Message{}, fmt.Errorf("decode: %w", err)
	}
	return response, nil
}

// HTTPDialer returns a Dialer that creates HTTP POST-based connections using
// the browser's XMLHttpRequest API. It is the browser-compatible equivalent
// of the native HTTPDialer in conn_http.go.
// Poll interval controls how often the client checks for server-pushed changes
// when idle. Default is 5 seconds. The url passed to the dialer should use the
// http:// or https:// scheme.
func HTTPDialer(pollInterval ...time.Duration) Dialer {
	interval := 5 * time.Second
	if len(pollInterval) > 0 && pollInterval[0] > 0 {
		interval = pollInterval[0]
	}
	return func(peerID, url string, header http.Header) (Connection, error) {
		h := make(http.Header)
		for k, v := range header {
			h[k] = v
		}
		return &httpConn{
			peerID:       peerID,
			url:          url,
			header:       h,
			pollInterval: interval,
			outgoing:     make(chan Message, 16),
			done:         make(chan struct{}),
		}, nil
	}
}
