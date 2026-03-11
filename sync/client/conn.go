//go:build !js || !wasm

package client

import (
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
)

// wsConn wraps a gorilla/websocket.Conn as a Connection.
type wsConn struct {
	ws     *websocket.Conn
	peerID string
}

func (c *wsConn) PeerID() string { return c.peerID }

func (c *wsConn) ReadMessage() (Message, error) {
	var msg Message
	err := c.ws.ReadJSON(&msg)
	return msg, err
}

func (c *wsConn) WriteMessage(msg Message) error {
	return c.ws.WriteJSON(msg)
}

func (c *wsConn) Close() error {
	return c.ws.Close()
}

// Dial creates a client-side Connection by dialing a WebSocket server.
func Dial(peerID, url string, header http.Header) (Connection, error) {
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	return &wsConn{ws: ws, peerID: peerID}, nil
}

// NewConn wraps an already-upgraded WebSocket as a server-side Connection.
func NewConn(ws *websocket.Conn, peerID string) Connection {
	ws.SetReadLimit(1 << 20)
	return &wsConn{ws: ws, peerID: peerID}
}
