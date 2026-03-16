# Encryption & Access Control for Sync Store

This document proposes adding end-to-end encryption and access control to store.md's sync layer.

## Overview

The sync store currently transmits data in plaintext over WebSocket. This proposal adds:

1. **End-to-End Encryption** - Server cannot read synced data
2. **Authenticated Sync** - Only authorized peers can join
3. **Key Rotation** - Forward secrecy through periodic key rotation

## Design Goals

- **Transparent**: Works with any backend (BBolt, IndexedDB, etc.)
- **Optional**: Encryption is opt-in per store
- **Performant**: Minimal overhead for sync operations
- **Compatible**: Works with existing sync protocol

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         SYNC ENCRYPTION                          │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌──────────────┐     Encrypted      ┌──────────────┐          │
│  │   Peer A     │ ◄────────────────► │   Peer B     │          │
│  │              │    WebSocket       │              │          │
│  │ ┌──────────┐ │                    │ ┌──────────┐ │          │
│  │ │SyncStore │ │                    │ │SyncStore │ │          │
│  │ │  + Enc   │ │                    │ │  + Enc   │ │          │
│  │ └────┬─────┘ │                    │ └────┬─────┘ │          │
│  │      │       │                    │      │       │          │
│  │ ┌────┴─────┐ │                    │ ┌────┴─────┐ │          │
│  │ │Backend   │ │                    │ │Backend   │ │          │
│  │ │(BBolt/etc)│ │                    │ │(IndexedDB)│ │          │
│  │ └──────────┘ │                    │ └──────────┘ │          │
│  └──────────────┘                    └──────────────┘          │
│         │                                   │                   │
│         └─────────── X (Server cannot read) ─┘                  │
│                                                                  │
│  Server only sees:                                               │
│  - Encrypted sync payloads                                       │
│  - Peer IDs and connection metadata                              │
│  - Message timing/size (metadata leakage)                        │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

## Proposed API

### 1. Encrypted Store Wrapper

```go
package encrypted

import (
    "github.com/readmedotmd/store.md"
    "github.com/readmedotmd/store.md/sync/core"
)

// EncryptedStore wraps a SyncStore with encryption
type EncryptedStore struct {
    inner    core.SyncStore
    groupKey [32]byte  // xsalsa20 key
    nonceGen NonceGenerator
}

// New creates an encrypted sync store
func New(inner core.SyncStore, key [32]byte) (*EncryptedStore, error)

// Implements core.SyncStore interface
func (s *EncryptedStore) Sync(ctx context.Context, peerID string, payload *core.SyncPayload) (*core.SyncPayload, error)
func (s *EncryptedStore) SetItem(ctx context.Context, ns, key, value string) error
func (s *EncryptedStore) GetItem(ctx context.Context, ns, key string) (string, error)
func (s *EncryptedStore) OnUpdate(fn func(core.SyncStoreItem)) func()
func (s *EncryptedStore) Close() error
```

### 2. Key Management

```go
package encrypted

// KeyManager handles secure key distribution
type KeyManager interface {
    // GetKey returns the current group key
    GetKey() ([32]byte, error)
    
    // RotateKey generates and distributes a new group key
    RotateKey() error
    
    // AuthorizePeer grants access to a new peer
    AuthorizePeer(peerID string, pubKey *[32]byte) error
    
    // RevokePeer removes peer access
    RevokePeer(peerID string) error
}

// NaClKeyManager implements KeyManager using NaCl/box
// for secure key distribution
type NaClKeyManager struct {
    serverPrivKey [32]byte
    peerKeys      map[string][32]byte // peerID -> public key
    currentKey    [32]byte
}
```

### 3. Authenticated Server

```go
package encrypted

// AuthServer wraps sync.Server with peer authentication
type AuthServer struct {
    inner      *server.Server
    keyManager KeyManager
    verifier   TokenVerifier
}

// TokenVerifier validates peer authentication tokens
type TokenVerifier interface {
    Verify(token string) (peerID string, pubKey *[32]byte, err error)
}

// NewAuthServer creates an authenticated sync server
func NewAuthServer(
    store core.SyncStore,
    keyManager KeyManager,
    verifier TokenVerifier,
) (*AuthServer, error)
```

## Implementation Details

### Encrypted Sync Protocol

When encryption is enabled, the sync payload is wrapped:

```go
// EncryptedPayload wraps a sync payload with encryption
type EncryptedPayload struct {
    Version    int              `json:"v"`      // 1
    KeyID      string           `json:"kid"`    // Key identifier for rotation
    Nonce      [24]byte         `json:"n"`      // xsalsa20 nonce
    Ciphertext []byte           `json:"ct"`     // encrypted payload
    Signature  []byte           `json:"sig"`    // detached signature
}

// Encryption flow:
// 1. Serialize original payload
// 2. Encrypt with secretbox (group key)
// 3. Sign with sender's private key
// 4. Send EncryptedPayload over wire
//
// Decryption flow:
// 1. Verify signature with sender's public key
// 2. Decrypt with secretbox (group key)
// 3. Deserialize original payload
```

### Key Distribution Flow

```
New Peer Joining:
                  ┌─────────────┐
                  │   Server    │
                  │  (KeyMgr)   │
                  └──────┬──────┘
                         │
  ┌─────────┐            │            ┌─────────┐
  │ Peer A  │            │            │ Peer B  │
  │(existing)│           │            │  (new)  │
  └────┬────┘            │            └────┬────┘
       │                 │                 │
       │                 │ 1. Request join │
       │                 │◄────────────────│
       │                 │                 │
       │                 │ 2. Verify token │
       │                 │                 │
       │                 │ 3. Encrypt key  │
       │                 │    for Peer B   │
       │                 │                 │
       │ 4. Broadcast    │                 │
       │    new peer     │                 │
       │◄────────────────│                 │
       │                 │                 │
       │                 │ 5. Send key     │
       │                 │────────────────►│
       │                 │                 │
       │                 │ 6. Ack          │
       │                 │◄────────────────│
       │                 │                 │
       │ 7. Add to       │                 │
       │    sync group   │                 │
       │◄────────────────│                 │
       │                 │                 │
```

### Token-Based Authentication

Peers authenticate using signed tokens:

```go
// AccessToken grants sync access
type AccessToken struct {
    StoreID   string    // Which store/realm
    PeerID    string    // Who is joining
    PubKey    [32]byte  // Their public key
    ExpiresAt time.Time // Token expiration
    Nonce     [24]byte  // Unique token ID
}

// Signed by store owner/admin
func (t *AccessToken) Sign(privKey ed25519.PrivateKey) []byte

// Verified by server
func (t *AccessToken) Verify(pubKey ed25519.PublicKey) error
```

## Usage Examples

### Encrypted Store (Client)

```go
// Generate or load peer keys
peerKeys, _ := encrypted.GeneratePeerKeys()

// Join store with invite token
invite := "store-abc-xyz-123"
token, _ := encrypted.RequestAccess(invite, &peerKeys.PublicKey)

// Get group key from server (encrypted with our public key)
encryptedKey, _ := server.GetKey(token, peerID)
groupKey, _ := encrypted.DecryptKey(encryptedKey, &peerKeys.PrivateKey)

// Create encrypted store
baseStore := bbolt.New("data.db")
syncStore := core.New(baseStore)
encStore, _ := encrypted.New(syncStore, groupKey)

// Use normally - encryption is transparent
encStore.SetItem(ctx, "chat", "msg/1", "Hello, encrypted world!")
```

### Authenticated Server

```go
// Setup
baseStore := bbolt.New("server.db")
syncStore := core.New(baseStore)

keyManager := encrypted.NewNaClKeyManager(adminKeys)
verifier := encrypted.NewTokenVerifier(adminPubKey)

authServer, _ := encrypted.NewAuthServer(
    syncStore,
    keyManager,
    verifier,
)

// Run
authServer.ListenAndServe(":8080")
```

### Multi-Tenant Server

```go
// Each store has its own encryption key
stores := map[string]*encrypted.AuthServer{}

resolver := func(storeID string) (*encrypted.AuthServer, error) {
    if s, ok := stores[storeID]; ok {
        return s, nil
    }
    
    // Create new encrypted store
    store, _ := bbolt.New(fmt.Sprintf("data-%s.db", storeID))
    keyManager := encrypted.NewNaClKeyManager(generateAdminKeys())
    
    s, _ := encrypted.NewAuthServer(
        core.New(store),
        keyManager,
        verifier,
    )
    stores[storeID] = s
    return s, nil
}

server := encrypted.NewMultiAuthServer(resolver, verifier)
server.ListenAndServe(":8080")
```

## Security Properties

### What the Server CANNOT See

| Data | Visibility | Notes |
|------|------------|-------|
| Message content | ❌ Encrypted | Server sees only ciphertext |
| Key values | ❌ Encrypted | Values encrypted before sync |
| Sync payload | ❌ Encrypted | Entire payload encrypted |

### What the Server CAN See

| Data | Visibility | Mitigation |
|------|------------|------------|
| Peer IDs | ✅ Visible | Needed for routing |
| Connection times | ✅ Visible | Use Tor for anonymity |
| Message count | ✅ Visible | Batch updates |
| Message sizes | ✅ Visible | Padding to fixed sizes |
| Store membership | ✅ Visible | Server manages access |

## Threat Model & Countermeasures

| Threat | Impact | Countermeasure |
|--------|--------|----------------|
| Server compromise | High | E2E encryption - server only sees ciphertext |
| MITM attack | High | Authenticated encryption (box) + signatures |
| Replay attack | Medium | Nonce + timestamp verification |
| Key compromise | High | Key rotation, forward secrecy |
| Unauthorized peer | High | Token-based auth + key distribution |
| Metadata analysis | Low | Padding, batching, delays |

## Implementation Plan

### Phase 1: Core Encryption

```
sync/encrypted/
├── store.go        # EncryptedStore wrapper
├── keys.go         # Key management
├── payload.go      # Encrypted payload types
└── crypto.go       # NaCl primitives
```

**Tasks:**
- [ ] Implement EncryptedStore wrapper
- [ ] Add payload encryption/decryption
- [ ] Integrate with existing sync protocol
- [ ] Tests for crypto correctness

### Phase 2: Authentication

```
sync/encrypted/
├── auth.go         # Token generation/verification
├── server.go       # AuthServer wrapper
└── invite.go       # Invite code system
```

**Tasks:**
- [ ] Token format and signing
- [ ] AuthServer implementation
- [ ] Invite code generation
- [ ] Peer admission flow

### Phase 3: Key Rotation

**Tasks:**
- [ ] Key versioning (KeyID)
- [ ] Rotation protocol
- [ ] Backward compatibility
- [ ] Emergency key revocation

### Phase 4: Documentation & Examples

**Tasks:**
- [ ] API documentation
- [ ] Security guide
- [ ] Example: Encrypted chat
- [ ] Example: Multi-tenant server

## API Summary

```go
// Enable encryption on client
import "github.com/readmedotmd/store.md/sync/encrypted"

store := core.New(bbolt.New("data.db"))
encStore, _ := encrypted.New(store, groupKey)

// Enable auth on server
authServer, _ := encrypted.NewAuthServer(
    store,
    keyManager,
    verifier,
)
authServer.ListenAndServe(":8080")

// Generate invite
invite, _ := authServer.CreateInvite(duration, maxUses)

// Use invite
client := encrypted.NewClient(invite)
client.Connect("ws://server/sync")
```

## Dependencies

```go
import (
    "golang.org/x/crypto/nacl/box"
    "golang.org/x/crypto/nacl/secretbox"
    "crypto/ed25519"
)
```

## References

- [NaCl](https://nacl.cr.yp.to/) - Networking and Cryptography library
- [Signal Protocol](https://signal.org/docs/) - Inspiration for key management
- [Double Ratchet](https://signal.org/docs/specifications/doubleratchet/) - Forward secrecy
- [store.md sync protocol](../sync.md) - Existing sync documentation

## Summary

This proposal adds transparent end-to-end encryption to store.md's sync layer:

- **`sync/encrypted`** - Drop-in encrypted store wrapper
- **Token-based auth** - Control who can join
- **Key rotation** - Forward secrecy
- **Minimal API changes** - Works with existing code

The server acts as an untrusted relay - it can enforce access control but cannot read the data.
