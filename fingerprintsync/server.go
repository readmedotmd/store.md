package fingerprintsync

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	gosync "sync"

	"github.com/gorilla/websocket"
	storesync "github.com/readmedotmd/store.md/sync"
)

// Authorizer extracts a peer ID from a request, returning an error if unauthorized.
type Authorizer func(r *http.Request) (peerID string, err error)

// StoreResolver returns a StoreFingerprint for the given store ID.
type StoreResolver func(storeID string) (*StoreFingerprint, error)

// Server is a WebSocket sync server that uses fingerprint reconciliation.
type Server struct {
	store    *StoreFingerprint // single-store mode
	resolver StoreResolver     // multi-store mode
	auth     Authorizer
	upgrader websocket.Upgrader

	adaptersMu gosync.Mutex
	adapters   map[*StoreFingerprint]*FingerprintClient
}

// NewServer creates a single-store fingerprint server.
func NewServer(store *StoreFingerprint, auth Authorizer) *Server {
	s := &Server{
		store:    store,
		auth:     auth,
		upgrader: websocket.Upgrader{},
		adapters: make(map[*StoreFingerprint]*FingerprintClient),
	}
	s.adapters[store] = NewClient(store)
	return s
}

// NewMultiServer creates a multi-store fingerprint server.
func NewMultiServer(resolver StoreResolver, auth Authorizer) *Server {
	return &Server{
		resolver: resolver,
		auth:     auth,
		upgrader: websocket.Upgrader{},
		adapters: make(map[*StoreFingerprint]*FingerprintClient),
	}
}

// SetAllowedOrigins configures which origins are allowed for WebSocket connections.
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

func (s *Server) resolveStore(r *http.Request) (*StoreFingerprint, error) {
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

func (s *Server) getAdapter(store *StoreFingerprint) *FingerprintClient {
	s.adaptersMu.Lock()
	defer s.adaptersMu.Unlock()
	if a, ok := s.adapters[store]; ok && !a.Closed() {
		return a
	}
	a := NewClient(store)
	s.adapters[store] = a
	return a
}

// notifyConn wraps a Connection and signals when ReadMessage returns an error.
type notifyConn struct {
	Connection
	done chan struct{}
	once gosync.Once
}

func (n *notifyConn) ReadMessage() (Message, error) {
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
		Connection: NewConn(ws, peerID),
		done:       make(chan struct{}),
	}

	adapter := s.getAdapter(store)
	if err := adapter.AddConnection(conn); err != nil {
		log.Printf("AddConnection error: %v", err)
		ws.Close()
		return
	}

	<-conn.done
}

// ListenAndServe starts an HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	return http.ListenAndServe(addr, mux)
}

// SyncStore returns the underlying SyncStore, satisfying the same pattern
// as the original server for consumers that need store access.
func (s *Server) SyncStore() storesync.SyncStore {
	return s.store
}

var _ http.Handler = (*Server)(nil)
