//go:build !js || !wasm

package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	gosync "sync"
	"time"
)

// sseConn implements [Connection] over an SSE event stream (read) paired
// with HTTP POST (write). The server pushes sync messages as SSE events
// with event type "sync" and JSON-encoded [Message] data. The client sends
// sync messages via POST to the same URL with X-Transport: sse header.
//
// Two separate HTTP clients are used: one with no timeout for the
// long-lived SSE stream, and one with a 30-second timeout for POST writes.
//
// The connection is created by [SSEDialer] and closed by calling Close(),
// which closes the SSE response body to unblock the reader goroutine.
type sseConn struct {
	peerID string
	url    string
	header http.Header
	client *http.Client

	streamResp *http.Response // the SSE GET response, closed on Close()
	msgCh      chan Message
	errCh      chan error
	done       chan struct{}
	once       gosync.Once
}

func (c *sseConn) PeerID() string { return c.peerID }

// ReadMessage returns the next message from the SSE event stream.
func (c *sseConn) ReadMessage() (Message, error) {
	select {
	case msg := <-c.msgCh:
		return msg, nil
	case err := <-c.errCh:
		return Message{}, err
	case <-c.done:
		return Message{}, fmt.Errorf("connection closed")
	}
}

// WriteMessage sends a sync message to the server via HTTP POST.
func (c *sseConn) WriteMessage(msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Transport", "sse")
	for k, v := range c.header {
		req.Header[k] = v
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

func (c *sseConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		// Close the SSE stream response body to unblock the reader goroutine.
		if c.streamResp != nil {
			c.streamResp.Body.Close()
		}
	})
	return nil
}

// readSSEStream reads from the SSE event stream and sends parsed messages
// to msgCh. It runs in a goroutine started by [SSEDialer] and exits when
// the stream ends, an I/O error occurs, or the connection is closed.
//
// Malformed events (invalid JSON) are silently skipped. When the stream
// ends, an error is sent to errCh so ReadMessage can return it.
//
// The response body is not closed here — it is closed by sseConn.Close().
func (c *sseConn) readSSEStream(resp *http.Response) {

	scanner := bufio.NewScanner(resp.Body)
	var dataLines []string

	for scanner.Scan() {
		select {
		case <-c.done:
			return
		default:
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			continue
		}

		// Empty line = end of event.
		if line == "" && len(dataLines) > 0 {
			data := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]

			var msg Message
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				continue // skip malformed events
			}

			select {
			case c.msgCh <- msg:
			case <-c.done:
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case c.errCh <- fmt.Errorf("SSE stream: %w", err):
		case <-c.done:
		}
	} else {
		select {
		case c.errCh <- fmt.Errorf("SSE stream ended"):
		case <-c.done:
		}
	}
}

// SSEDialer returns a [Dialer] that creates SSE-based connections.
//
// The URL should point to the server's SSE endpoint using http:// or
// https://. When used with [WithDialerForScheme], the custom sse:// and
// sses:// schemes are automatically mapped to http:// and https://.
//
// The dialer opens a GET request with Accept: text/event-stream for the
// event stream and sends sync messages via POST to the same URL. It
// validates the response status and Content-Type before returning.
//
// Example:
//
//	// Direct usage:
//	c := client.New(store, client.WithDialer(client.SSEDialer()))
//	c.Connect("peer", "http://host/sse", headers)
//
//	// With scheme-based routing:
//	c := client.New(store,
//	    client.WithDialerForScheme("sse", client.SSEDialer()),
//	)
//	c.Connect("peer", "sse://host/sse", headers)
func SSEDialer() Dialer {
	return func(peerID, url string, header http.Header) (Connection, error) {
		h := make(http.Header)
		for k, v := range header {
			h[k] = v
		}

		httpClient := &http.Client{Timeout: 0} // no timeout for SSE stream

		// Open the SSE event stream.
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("new SSE request: %w", err)
		}
		req.Header.Set("Accept", "text/event-stream")
		for k, v := range h {
			req.Header[k] = v
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("SSE dial: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("SSE server returned %d", resp.StatusCode)
		}

		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/event-stream") {
			resp.Body.Close()
			return nil, fmt.Errorf("unexpected content type: %s", ct)
		}

		conn := &sseConn{
			peerID:     peerID,
			url:        url,
			header:     h,
			client:     &http.Client{Timeout: 30 * time.Second},
			streamResp: resp,
			msgCh:      make(chan Message, 64),
			errCh:      make(chan error, 1),
			done:       make(chan struct{}),
		}

		go conn.readSSEStream(resp)
		return conn, nil
	}
}
