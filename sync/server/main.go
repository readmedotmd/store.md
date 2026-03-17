package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/readmedotmd/store.md/sync/client"
	storesync "github.com/readmedotmd/store.md/sync/core"
)

var _ http.Handler = (*Server)(nil)

// Authorizer extracts a peer ID from a request, returning an error if unauthorized.
type Authorizer func(r *http.Request) (peerID string, err error)

// StoreResolver returns a SyncStore for the given store ID.
// It is called once per WebSocket connection. Return an error to reject the connection.
type StoreResolver func(storeID string) (storesync.SyncStore, error)

// TokenInfo describes a token with optional expiration for use with TokenAuthWithExpiry.
type TokenInfo struct {
	PeerID    string
	ExpiresAt time.Time // zero means never expires
}

// Server is a sync server that accepts incoming connections and delegates
// sync protocol handling to pluggable transports. By default, it uses
// WebSocket. Call EnableHTTP to also accept HTTP POST requests, or use
// AddTransport to register custom transports.
type Server struct {
	store    storesync.SyncStore // single-store mode (nil when using resolver)
	resolver StoreResolver       // multi-store mode (nil when using single store)
	auth     Authorizer
	logger   *slog.Logger

	transports  []Transport
	wsTransport *WebSocketTransport // kept for SetAllowedOrigins

	maxConnsPerPeer int
	maxTotalConns   int
	trustedProxies  map[string]struct{} // IPs trusted to set X-Forwarded-For

	// Connection tracking
	connsMu    gosync.Mutex
	totalConns int
	peerConns  map[string]int

	// Rate limiting for auth failures (per IP)
	authFailMu    gosync.Mutex
	authFailCount map[string]*authFailEntry

	adaptersMu gosync.Mutex
	adapters   map[storesync.SyncStore]*client.Client

	// Hooks
	onPeerConnect    []func(peerID string, r *http.Request) error
	onPeerDisconnect []func(peerID string)

	httpServer *http.Server // set by ListenAndServe/ListenAndServeTLS
}

type authFailEntry struct {
	count     int
	resetAt   time.Time
}

// New creates a single-store server. All connections share the same StoreSync.
// WebSocket transport is enabled by default.
func New(store storesync.SyncStore, auth Authorizer) *Server {
	wst := &WebSocketTransport{Upgrader: websocket.Upgrader{}}
	s := &Server{
		store:           store,
		auth:            auth,
		logger:          slog.Default(),
		transports:      []Transport{wst},
		wsTransport:     wst,
		maxConnsPerPeer: 10,
		maxTotalConns:   1000,
		peerConns:       make(map[string]int),
		authFailCount:   make(map[string]*authFailEntry),
		adapters:        make(map[storesync.SyncStore]*client.Client),
	}
	s.adapters[store] = client.New(store)
	return s
}

// NewMulti creates a multi-store server. The store ID is extracted from the URL
// path — connect to /sync/{storeID} and the resolver maps storeID to a StoreSync.
// WebSocket transport is enabled by default.
func NewMulti(resolver StoreResolver, auth Authorizer) *Server {
	wst := &WebSocketTransport{Upgrader: websocket.Upgrader{}}
	return &Server{
		resolver:        resolver,
		auth:            auth,
		logger:          slog.Default(),
		transports:      []Transport{wst},
		wsTransport:     wst,
		maxConnsPerPeer: 10,
		maxTotalConns:   1000,
		peerConns:       make(map[string]int),
		authFailCount:   make(map[string]*authFailEntry),
		adapters:        make(map[storesync.SyncStore]*client.Client),
	}
}

// SetLogger sets the structured logger for the server.
func (s *Server) SetLogger(l *slog.Logger) {
	s.logger = l
}

// SetMaxConnsPerPeer sets the maximum connections per peer. Default is 10.
func (s *Server) SetMaxConnsPerPeer(n int) {
	s.maxConnsPerPeer = n
}

// SetMaxTotalConns sets the maximum total connections. Default is 1000.
func (s *Server) SetMaxTotalConns(n int) {
	s.maxTotalConns = n
}

// SetTrustedProxies configures IP addresses trusted to set X-Forwarded-For.
// When a request arrives from a trusted proxy, the client IP is extracted
// from the X-Forwarded-For header instead of RemoteAddr. This is required
// for accurate rate limiting behind reverse proxies and CDNs.
func (s *Server) SetTrustedProxies(ips []string) {
	s.trustedProxies = make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		s.trustedProxies[ip] = struct{}{}
	}
}

// SetAllowedOrigins configures which origins are allowed for WebSocket connections.
// In production, always specify exact origins. Never use "*" in production as it
// allows any website to connect to your sync server.
// Example: SetAllowedOrigins([]string{"https://myapp.example.com"})
func (s *Server) SetAllowedOrigins(origins []string) {
	if s.wsTransport == nil {
		return
	}
	s.wsTransport.Upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		for _, o := range origins {
			if o == "*" || o == origin {
				return true
			}
		}
		return false
	}
}

// OnPeerConnect registers an interceptor invoked after a peer is authenticated.
// Return a non-nil error to reject the connection — the server will respond
// with 403 Forbidden.
func (s *Server) OnPeerConnect(fn func(peerID string, r *http.Request) error) {
	s.onPeerConnect = append(s.onPeerConnect, fn)
}

// OnPeerDisconnect registers a callback invoked after a peer's transport
// connection ends (e.g. WebSocket closed, HTTP request completed).
func (s *Server) OnPeerDisconnect(fn func(peerID string)) {
	s.onPeerDisconnect = append(s.onPeerDisconnect, fn)
}

// AddTransport registers a transport with the server. Transports are tried
// in registration order; the first whose CanHandle returns true is used.
func (s *Server) AddTransport(t Transport) {
	s.transports = append(s.transports, t)
}

// EnableHTTP adds an HTTP POST transport so the server can accept sync
// requests over plain HTTP in addition to WebSocket. HTTP transport is
// stateless — each POST carries one sync message and receives one response.
func (s *Server) EnableHTTP() {
	s.AddTransport(&HTTPTransport{})
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

// TokenAuthWithExpiry returns an Authorizer that validates Bearer tokens with
// optional expiration support.
func TokenAuthWithExpiry(tokens map[string]TokenInfo) Authorizer {
	return func(r *http.Request) (string, error) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return "", fmt.Errorf("missing or invalid Authorization header")
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		info, ok := tokens[token]
		if !ok {
			return "", fmt.Errorf("invalid token")
		}
		if !info.ExpiresAt.IsZero() && time.Now().After(info.ExpiresAt) {
			return "", fmt.Errorf("token expired")
		}
		return info.PeerID, nil
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
	s.adaptersMu.Lock()
	defer s.adaptersMu.Unlock()
	if a, ok := s.adapters[store]; ok && !a.Closed() {
		return a
	}
	a := client.New(store)
	s.adapters[store] = a
	return a
}

// checkAuthRateLimit returns true if the IP has exceeded the auth failure limit.
func (s *Server) checkAuthRateLimit(ip string) bool {
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	entry, ok := s.authFailCount[ip]
	if !ok {
		return false
	}
	if time.Now().After(entry.resetAt) {
		delete(s.authFailCount, ip)
		return false
	}
	return entry.count >= 10
}

// recordAuthFailure records an auth failure for the given IP.
func (s *Server) recordAuthFailure(ip string) {
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	// Cap map size to prevent memory exhaustion from distributed attacks.
	if len(s.authFailCount) >= 10000 {
		return
	}
	entry, ok := s.authFailCount[ip]
	if !ok || time.Now().After(entry.resetAt) {
		s.authFailCount[ip] = &authFailEntry{
			count:   1,
			resetAt: time.Now().Add(1 * time.Minute),
		}
		return
	}
	entry.count++
}

// acquireConn attempts to acquire a connection slot for the given peer.
// Returns an error if limits are exceeded.
func (s *Server) acquireConn(peerID string) error {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.totalConns >= s.maxTotalConns {
		return fmt.Errorf("maximum total connections reached (%d)", s.maxTotalConns)
	}
	if s.peerConns[peerID] >= s.maxConnsPerPeer {
		return fmt.Errorf("maximum connections per peer reached (%d)", s.maxConnsPerPeer)
	}
	s.totalConns++
	s.peerConns[peerID]++
	return nil
}

// releaseConn releases a connection slot for the given peer.
func (s *Server) releaseConn(peerID string) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.totalConns > 0 {
		s.totalConns--
	}
	s.peerConns[peerID]--
	if s.peerConns[peerID] <= 0 {
		delete(s.peerConns, peerID)
	}
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

// CloseCodeAuthFailed is the WebSocket close status code sent when
// authentication fails. The server upgrades the connection before
// closing so that browser clients (which cannot read HTTP status
// codes from failed upgrades) can detect auth failures.
const CloseCodeAuthFailed = 4401

// clientIP extracts the client IP from the request. If the request comes
// from a trusted proxy and contains an X-Forwarded-For header, the leftmost
// (original client) IP is used. Otherwise, RemoteAddr is used.
func (s *Server) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx >= 0 {
		ip = ip[:idx]
	}
	if len(s.trustedProxies) > 0 {
		if _, trusted := s.trustedProxies[ip]; trusted {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				// X-Forwarded-For: client, proxy1, proxy2
				// The leftmost entry is the original client.
				if clientIP, _, ok := strings.Cut(xff, ","); ok {
					return strings.TrimSpace(clientIP)
				}
				return strings.TrimSpace(xff)
			}
		}
	}
	return ip
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Rate limit auth failures by IP.
	ip := s.clientIP(r)
	if s.checkAuthRateLimit(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	peerID, err := s.auth(r)
	if err != nil {
		s.recordAuthFailure(ip)
		// For WebSocket requests, upgrade first then close with 4401
		// so browsers can distinguish auth failures from network errors.
		// The browser WebSocket API does not expose HTTP status codes
		// from failed upgrades — both 401 and network errors surface
		// as close code 1006.
		if websocket.IsWebSocketUpgrade(r) && s.wsTransport != nil {
			ws, upgradeErr := s.wsTransport.Upgrader.Upgrade(w, r, nil)
			if upgradeErr != nil {
				return
			}
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseCodeAuthFailed, "Unauthorized"))
			ws.Close()
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	store, err := s.resolveStore(r)
	if err != nil {
		s.logger.Error("resolveStore error", "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Find a transport that can handle this request.
	var transport Transport
	for _, t := range s.transports {
		if t.CanHandle(r) {
			transport = t
			break
		}
	}
	if transport == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Check connection limits.
	if err := s.acquireConn(peerID); err != nil {
		s.logger.Warn("connection limit reached", "peer", peerID, "err", err)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	for _, fn := range s.onPeerConnect {
		if err := fn(peerID, r); err != nil {
			s.releaseConn(peerID)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	adapter := s.getAdapter(store)
	if err := transport.Serve(w, r, peerID, store, adapter, s.logger); err != nil {
		s.logger.Error("transport error", "err", err)
	}
	s.releaseConn(peerID)

	for _, fn := range s.onPeerDisconnect {
		fn(peerID)
	}
}

// ListenAndServe starts an HTTP server on the given address. It blocks until
// the server is stopped. Use Shutdown(ctx) for graceful shutdown.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	s.httpServer = &http.Server{Addr: addr, Handler: mux}
	return s.httpServer.ListenAndServe()
}

// ListenAndServeTLS starts an HTTPS server on the given address. It blocks
// until the server is stopped. Use Shutdown(ctx) for graceful shutdown.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	s.httpServer = &http.Server{Addr: addr, Handler: mux}
	return s.httpServer.ListenAndServeTLS(certFile, keyFile)
}

// Shutdown gracefully shuts down the HTTP server without interrupting
// active connections, then closes all sync adapters.
func (s *Server) Shutdown(ctx context.Context) error {
	var shutdownErr error
	if s.httpServer != nil {
		shutdownErr = s.httpServer.Shutdown(ctx)
	}
	if err := s.CloseWithContext(ctx); err != nil && shutdownErr == nil {
		shutdownErr = err
	}
	return shutdownErr
}

// Close closes all client adapters managed by this server, waiting for their
// goroutines to drain. Call this before closing the underlying stores.
func (s *Server) Close() error {
	return s.CloseWithContext(context.Background())
}

// CloseWithContext closes all client adapters with context-aware shutdown support.
func (s *Server) CloseWithContext(ctx context.Context) error {
	s.adaptersMu.Lock()
	adapters := make([]*client.Client, 0, len(s.adapters))
	for _, a := range s.adapters {
		adapters = append(adapters, a)
	}
	s.adaptersMu.Unlock()

	done := make(chan error, 1)
	go func() {
		var firstErr error
		for _, a := range adapters {
			if err := a.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		done <- firstErr
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
