//go:build !js || !wasm

package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	gosync "sync"
	"time"

	storesync "github.com/readmedotmd/store.md/sync/core"
)

// httpConn implements Connection over HTTP POST requests. Outgoing sync
// messages are buffered and sent with the next ReadMessage call. When no
// outgoing messages are pending, ReadMessage polls the server at a regular
// interval to discover server-initiated changes.
type httpConn struct {
	peerID       string
	url          string
	header       http.Header
	client       *http.Client
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
// poll interval. It sends the message via HTTP POST and returns the response.
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

	body, err := json.Marshal(msg)
	if err != nil {
		return Message{}, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Message{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.header {
		req.Header[k] = v
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var response Message
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Message{}, fmt.Errorf("decode: %w", err)
	}

	return response, nil
}

func (c *httpConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// HTTPDialer returns a Dialer that creates HTTP POST-based connections.
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
			client:       &http.Client{Timeout: 30 * time.Second},
			pollInterval: interval,
			outgoing:     make(chan Message, 16),
			done:         make(chan struct{}),
		}, nil
	}
}
