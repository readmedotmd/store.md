package client

import "net/http"

// Dialer creates a Connection to a remote sync server.
// The default dialer uses WebSocket (Dial). Use HTTPDialer for environments
// where WebSocket is not available.
type Dialer func(peerID, url string, header http.Header) (Connection, error)

// WithDialer sets a custom dialer for creating outbound connections via Connect.
// By default, the client uses the WebSocket Dial function.
func WithDialer(d Dialer) Option {
	return func(c *Client) { c.dialer = d }
}
