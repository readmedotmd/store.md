package server

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	gosync "sync"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/client"
	storesync "github.com/readmedotmd/store.md/sync"
)

// Authorizer extracts a peer ID from a request, returning an error if unauthorized.
type Authorizer func(r *http.Request) (peerID string, err error)

// StoreResolver returns a SyncStore for the given store ID.
// It is called once per WebSocket connection. Return an error to reject the connection.
type StoreResolver func(storeID string) (storesync.SyncStore, error)

// Server is a WebSocket sync server that accepts incoming connections and
// delegates sync protocol handling to the client adapter.
type Server struct {
	store    storesync.SyncStore // single-store mode (nil when using resolver)
	resolver StoreResolver       // multi-store mode (nil when using single store)
	auth     Authorizer
	upgrader websocket.Upgrader

	adaptersMu gosync.RWMutex
	adapters   map[storesync.SyncStore]*client.Client
}

// New creates a single-store server. All connections share the same StoreSync.
func New(store storesync.SyncStore, auth Authorizer) *Server {
	s := &Server{
		store:    store,
		auth:     auth,
		upgrader: websocket.Upgrader{},
		adapters: make(map[storesync.SyncStore]*client.Client),
	}
	s.adapters[store] = client.New(store)
	return s
}

// NewMulti creates a multi-store server. The store ID is extracted from the URL
// path — connect to /sync/{storeID} and the resolver maps storeID to a StoreSync.
func NewMulti(resolver StoreResolver, auth Authorizer) *Server {
	return &Server{
		resolver: resolver,
		auth:     auth,
		upgrader: websocket.Upgrader{},
		adapters: make(map[storesync.SyncStore]*client.Client),
	}
}

// SetAllowedOrigins configures which origins are allowed for WebSocket connections.
// Pass "*" to allow all origins (not recommended for production).
func (s *Server) SetAllowedOrigins(origins []string) {
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		for _, o := range origins {
			if o == "*" || o == origin {
				return true
			}
		}
		return false
	}
}

// TokenAuth returns an Authorizer that validates Bearer tokens.
func TokenAuth(tokens map[string]string) Authorizer {
	return func(r *http.Request) (string, error) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return "", fmt.Errorf("missing or invalid Authorization header")
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		peerID, ok := tokens[token]
		if !ok {
			return "", fmt.Errorf("invalid token")
		}
		return peerID, nil
	}
}

func (s *Server) resolveStore(r *http.Request) (storesync.SyncStore, error) {
	if s.store != nil {
		return s.store, nil
	}
	path := strings.TrimRight(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 || idx == len(path)-1 {
		return nil, fmt.Errorf("missing store ID in path")
	}
	storeID := path[idx+1:]
	if storeID == "" {
		return nil, fmt.Errorf("missing store ID in path")
	}
	return s.resolver(storeID)
}

func (s *Server) getAdapter(store storesync.SyncStore) *client.Client {
	s.adaptersMu.RLock()
	a, ok := s.adapters[store]
	s.adaptersMu.RUnlock()
	if ok {
		return a
	}

	s.adaptersMu.Lock()
	defer s.adaptersMu.Unlock()
	if a, ok := s.adapters[store]; ok {
		return a
	}
	a = client.New(store)
	s.adapters[store] = a
	return a
}

// notifyConn wraps a Connection and signals a channel when ReadMessage returns an error.
type notifyConn struct {
	client.Connection
	done chan struct{}
	once gosync.Once
}

func (n *notifyConn) ReadMessage() (client.Message, error) {
	msg, err := n.Connection.ReadMessage()
	if err != nil {
		n.once.Do(func() { close(n.done) })
	}
	return msg, err
}

func (n *notifyConn) Close() error {
	n.once.Do(func() { close(n.done) })
	return n.Connection.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peerID, err := s.auth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	store, err := s.resolveStore(r)
	if err != nil {
		log.Printf("resolveStore error: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	conn := &notifyConn{
		Connection: client.NewConn(ws, peerID),
		done:       make(chan struct{}),
	}

	adapter := s.getAdapter(store)
	if err := adapter.AddConnection(conn); err != nil {
		log.Printf("AddConnection error: %v", err)
		ws.Close()
		return
	}

	// Block until the connection is closed. The adapter's readLoop handles
	// all protocol messages. The notifyConn signals done when ReadMessage
	// returns an error (i.e. the connection dropped).
	<-conn.done
}

// ListenAndServe starts an HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	return http.ListenAndServe(addr, mux)
}

var _ http.Handler = (*Server)(nil)
