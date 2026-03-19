package client

import (
	"net/http"
	"net/url"
)

// Dialer creates a [Connection] to a remote sync server.
//
// Built-in dialers:
//   - [Dial]: WebSocket (default, persistent bidirectional)
//   - [HTTPDialer]: HTTP POST polling (stateless, works everywhere)
//   - [SSEDialer]: Server-Sent Events (server push + POST writes)
//
// Custom dialers can be registered with [WithDialer] (global default)
// or [WithDialerForScheme] (per URL scheme).
type Dialer func(peerID, url string, header http.Header) (Connection, error)

// WithDialer sets a custom dialer for creating outbound connections via Connect.
// By default, the client uses the WebSocket Dial function.
func WithDialer(d Dialer) Option {
	return func(c *Client) { c.dialer = d }
}

// WithDialerForScheme registers a dialer for a specific URL scheme.
// When Connect is called, the URL scheme determines which dialer to use.
// If no scheme-specific dialer matches, the default dialer is used.
//
// Common schemes:
//   - "ws", "wss": WebSocket (use Dial)
//   - "http", "https": HTTP polling (use HTTPDialer)
//   - "sse", "sses": Server-Sent Events (use SSEDialer)
//
// This enables a single client to connect to multiple servers using
// different transports:
//
//	c := client.New(store,
//	    client.WithDialerForScheme("ws", client.Dial),
//	    client.WithDialerForScheme("sse", client.SSEDialer()),
//	)
//	c.Connect("peer1", "ws://host1/ws", headers)
//	c.Connect("peer2", "sse://host2/sse", headers)
func WithDialerForScheme(scheme string, d Dialer) Option {
	return func(c *Client) {
		if c.dialers == nil {
			c.dialers = make(map[string]Dialer)
		}
		c.dialers[scheme] = d
	}
}

// schemeMapping maps custom URL schemes to real HTTP schemes for dialing.
// When [Client.resolveDialer] encounters a registered custom scheme, it
// rewrites the URL to use the real scheme before passing it to the dialer.
// For example, "sse://host/path" becomes "http://host/path".
//
// This allows users to express transport intent in the URL scheme:
//
//	c.Connect("peer", "sse://host/sse", headers)   // → http://host/sse via SSEDialer
//	c.Connect("peer", "sses://host/sse", headers)  // → https://host/sse via SSEDialer
var schemeMapping = map[string]string{
	"sse":  "http",
	"sses": "https",
}

// resolveDialer selects the appropriate dialer for the given URL and
// returns the (possibly rewritten) URL for dialing.
//
// Resolution order:
//  1. If no scheme-specific dialers are registered, return the default dialer.
//  2. Parse the URL scheme and look up a registered dialer.
//  3. If found and the scheme has a mapping in [schemeMapping], rewrite the
//     URL scheme (e.g., sse:// → http://) before returning.
//  4. If no match, fall back to the default dialer.
func (c *Client) resolveDialer(rawURL string) (Dialer, string) {
	if len(c.dialers) == 0 {
		return c.dialer, rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return c.dialer, rawURL
	}

	scheme := u.Scheme
	if d, ok := c.dialers[scheme]; ok {
		// Rewrite custom schemes to real HTTP/WS schemes for dialing.
		if realScheme, mapped := schemeMapping[scheme]; mapped {
			u.Scheme = realScheme
			return d, u.String()
		}
		return d, rawURL
	}

	return c.dialer, rawURL
}
