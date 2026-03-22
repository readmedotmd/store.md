# How to Implement Encrypted Sync Store

A practical, step-by-step guide to adding end-to-end encryption to store.md.

## Architecture Overview

```
┌────────────────────────────────────────────────────────────────┐
│                     CLIENT (Browser)                           │
│                                                                │
│  ┌──────────────┐      ┌──────────────┐      ┌─────────────┐  │
│  │   UI/App     │─────▶│ Encrypted    │─────▶│  IndexedDB  │  │
│  │              │◀─────│ SyncStore    │◀─────│  (persist)  │  │
│  └──────────────┘      └──────┬───────┘      └─────────────┘  │
│                               │                                │
│                               │ Encrypt/decrypt transparent    │
│                               │                                │
│  ┌────────────────────────────┼────────────────────────────┐  │
│  │      ECDH Key Derivation   │   (happens once)           │  │
│  │  - Generate box keypair    │                            │  │
│  │  - Exchange pub keys       │                            │  │
│  │  - Derive shared secret    │                            │  │
│  │  - Derive group key        │                            │  │
│  └────────────────────────────┼────────────────────────────┘  │
│                               │                                │
│                               ▼                                │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  WebSocket Client (syncs encrypted payloads only)      │  │
│  └────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────┘
                              │
                              │ Encrypted sync payload
                              │ (server can't read)
                              ▼
┌────────────────────────────────────────────────────────────────┐
│                     SERVER                                     │
│                                                                │
│  ┌──────────────┐      ┌──────────────┐      ┌─────────────┐  │
│  │   WebSocket  │─────▶│   Sync       │─────▶│   BBolt     │  │
│  │   Server     │◀─────│   Server     │◀─────│   (persist) │  │
│  └──────────────┘      └──────────────┘      └─────────────┘  │
│                                                                │
│  Server sees:                                                  │
│  - Encrypted payloads (opaque blobs)                          │
│  - Peer IDs (for routing)                                      │
│  - Connection metadata (timing, size)                         │
│                                                                │
│  Server CANNOT see:                                            │
│  - Message content                                             │
│  - Key values                                                  │
│  - Who is sending what to whom (content-wise)                 │
└────────────────────────────────────────────────────────────────┘
```

## Implementation Steps

### Step 1: Core Encryption Layer

File: `sync/encrypted/store.go` (already created)

```go
// EncryptedStore wraps SyncStore with transparent encryption
type EncryptedStore struct {
    inner  core.SyncStore
    key    [32]byte  // xsalsa20 key
    keyID  string     // for rotation
    signKey ed25519.PrivateKey
}

// Implements core.SyncStore interface
func (es *EncryptedStore) SetItem(ctx, ns, key, value string) error {
    encrypted := secretbox.Seal(nil, []byte(value), &nonce, &es.key)
    return es.inner.SetItem(ctx, ns, key, string(encrypted))
}
```

### Step 2: Key Exchange Protocol

File: `sync/encrypted/exchange.go`

```go
// KeyExchange handles ECDH key agreement between peers

type KeyExchange struct {
    myBoxKeys   *BoxKeyPair
    mySignKeys  ed25519.PrivateKey
    peerPubKeys map[string][32]byte // peerID -> box pubkey
}

// Join creates or joins an encrypted room
func (ke *KeyExchange) Join(roomID string, peerPubKeys [][32]byte) ([32]byte, error) {
    if len(peerPubKeys) == 1 {
        // 1-to-1: Direct ECDH
        return ke.pairwiseKey(peerPubKeys[0])
    }
    
    // N-to-N: Chain derivation
    return ke.groupKey(roomID, peerPubKeys)
}

// pairwiseKey derives shared key with one peer via ECDH
func (ke *KeyExchange) pairwiseKey(peerPub [32]byte) ([32]byte, error) {
    shared := GenerateSharedSecret(&ke.myBoxKeys.PrivateKey, &peerPub)
    return DeriveGroupKey(shared, "pairwise-v1"), nil
}

// groupKey derives shared key for group via incremental hashing
func (ke *KeyExchange) groupKey(roomID string, peerPubs [][32]byte) ([32]byte, error) {
    // Start with first peer
    key, _ := ke.pairwiseKey(peerPubs[0])
    
    // Mix in each additional peer
    for _, pub := range peerPubs[1:] {
        shared := GenerateSharedSecret(&ke.myBoxKeys.PrivateKey, &pub)
        key = DeriveGroupKey(shared, string(key[:]))
    }
    
    // Final derivation with room context
    return DeriveGroupKey(key, roomID), nil
}
```

### Step 3: Authentication Server

File: `sync/encrypted/server.go`

```go
// AuthServer wraps sync.Server with peer authentication
// Controls who can join the encrypted room

type AuthServer struct {
    inner      *server.Server
    roomKeys   map[string][32]byte // roomID -> current group key
    authorized map[string]bool     // peerID -> authorized
    mu         sync.RWMutex
}

// AuthorizePeer grants access to a room
func (as *AuthServer) AuthorizePeer(roomID, peerID string, pubKey [32]byte) error {
    as.mu.Lock()
    defer as.mu.Unlock()
    
    // Check if peer already authorized
    if as.authorized[peerID] {
        return fmt.Errorf("peer already authorized")
    }
    
    // Get current room key
    roomKey := as.roomKeys[roomID]
    
    // Encrypt room key for this peer using server's box keys
    encryptedKey, err := as.encryptForPeer(roomKey, pubKey)
    if err != nil {
        return err
    }
    
    // Store authorization
    as.authorized[peerID] = true
    
    // Send encrypted key to peer (via special message or API)
    return as.sendKeyToPeer(peerID, encryptedKey)
}

// RevokePeer removes access and rotates key
func (as *AuthServer) RevokePeer(roomID, peerID string) error {
    as.mu.Lock()
    defer as.mu.Unlock()
    
    delete(as.authorized, peerID)
    
    // Generate new room key (forward secrecy)
    newKey, _ := encrypted.GenerateKey()
    as.roomKeys[roomID] = newKey
    
    // Re-distribute to remaining authorized peers
    return as.redistributeKeys(roomID)
}
```

### Step 4: Client-Side Key Management

File: `sync/encrypted/client.go`

```go
// EncryptedClient wraps sync.Client with key handling

type EncryptedClient struct {
    inner      *client.Client
    keyEx      *KeyExchange
    roomID     string
    groupKey   [32]byte
    encStore   *EncryptedStore
}

// Connect joins an encrypted room
func (ec *EncryptedClient) Connect(serverURL string, roomID string) error {
    ec.roomID = roomID
    
    // 1. Connect to server (unencrypted initially)
    err := ec.inner.Connect(ec.keyEx.myBoxKeys.PublicKey[:], serverURL, nil)
    if err != nil {
        return err
    }
    
    // 2. Request room access
    // Server responds with list of peer pubkeys + encrypted room key
    roomInfo, err := ec.requestRoomAccess(roomID)
    if err != nil {
        return err
    }
    
    // 3. Derive or decrypt group key
    if len(roomInfo.PeerPubKeys) == 0 {
        // We're first - generate new key
        ec.groupKey, _ = encrypted.GenerateKey()
    } else {
        // Decrypt key from server (encrypted with our box pubkey)
        ec.groupKey, err = ec.decryptRoomKey(roomInfo.EncryptedKey)
        if err != nil {
            return err
        }
    }
    
    // 4. Create encrypted store wrapper
    ec.encStore, _ = encrypted.New(
        ec.inner, // Actually this needs the syncStore, not client
        encrypted.Config{
            GroupKey: ec.groupKey,
            SignKey:  ec.keyEx.mySignKeys,
        },
    )
    
    return nil
}
```

### Step 5: Integration with Existing Code

Modify chat example to use encryption:

```go
// client/main.go

func initStore() {
    // ... existing code ...
    
    // Generate or load identity keys
    boxKeys, _ := encrypted.GenerateBoxKeys()
    signPriv, _, _ := encrypted.GenerateSigningKeys()
    
    // Create key exchange handler
    keyEx := &encrypted.KeyExchange{
        MyBoxKeys:  boxKeys,
        MySignKeys: signPriv,
    }
    
    // Try to connect with encryption
    go connectEncrypted(keyEx)
}

func connectEncrypted(keyEx *encrypted.KeyExchange) {
    // Show "Join Room" dialog with room ID input
    roomID := promptRoomID() // "abc123"
    
    // Connect and join room
    encClient := &encrypted.EncryptedClient{
        KeyEx: keyEx,
    }
    
    err := encClient.Connect(serverURL, roomID)
    if err != nil {
        setStatus("Failed to join: "+err.Error(), "disconnected")
        return
    }
    
    // Use encrypted store normally
    syncStore = encClient.EncStore
    
    setStatus("Connected (encrypted)", "connected")
}
```

### Step 6: Room Creation Flow

```
New Room Creation:

1. User clicks "Create Encrypted Room"
2. Client generates:
   - Room ID (random string)
   - Group key (random 32 bytes)
   - Creator's box/sign keys
3. Client sends to server:
   POST /rooms
   {
       "room_id": "abc123",
       "creator_pubkey": "base64...",
       "encrypted_key": null  // creator knows key directly
   }
4. Server creates room, stores creator as admin
5. Server returns room ID to creator
6. Creator can now share room ID with others

Join Existing Room:

1. User enters room ID "abc123"
2. Client sends:
   POST /rooms/abc123/join
   {
       "pubkey": "base64..."
   }
3. Server checks if authorized
4. If yes:
   - Server encrypts room key with user's pubkey
   - Returns encrypted key + list of peer pubkeys
5. Client decrypts key, derives final group key
6. Client can now sync encrypted data
```

## Data Flow

### Sending a Message

```go
// Application code
syncStore.SetItem(ctx, "chat", "msg/1", "Hello!")

// EncryptedStore intercepts:
// 1. Serialize: []byte(`{"content":"Hello!",...}`)
// 2. Encrypt: secretbox.Seal(plaintext, nonce, groupKey)
// 3. Base64 encode for JSON
// 4. Pass to inner store

// Inner store (core.SyncStore):
// 1. Queues for sync
// 2. Sync client sends to server

// On wire:
// { "items": [
//   {"key":"msg/1", "value":"base64(encrypted+nonce+mac)"}
// ]}
// Server sees only opaque blob!
```

### Receiving a Message

```go
// From server (encrypted):
// { "items": [
//   {"key":"msg/1", "value":"base64(encrypted+nonce+mac)"}
// ]}

// Sync client passes to core.SyncStore
// Core store calls OnUpdate listeners

// EncryptedStore.OnUpdate:
// 1. Base64 decode
// 2. Extract nonce + ciphertext
// 3. Decrypt: secretbox.Open(ciphertext, nonce, groupKey)
// 4. Deserialize JSON
// 5. Call app's OnUpdate with plaintext

// App displays: "Hello!"
```

## Security Checklist

- [ ] Keys never sent over wire unencrypted
- [ ] Group key encrypted with ECDH for each peer
- [ ] Per-message random nonces
- [ ] Message signatures for authenticity
- [ ] Key rotation on peer revocation
- [ ] Forward secrecy (old keys don't decrypt new messages)
- [ ] Constant-time comparison for MAC verification
- [ ] Secure random number generation
- [ ] Keys cleared from memory when possible

## Testing Strategy

```go
func TestEncryptedStore(t *testing.T) {
    // Test encryption/decryption roundtrip
    key, _ := encrypted.GenerateKey()
    store, _ := encrypted.New(memory.New(), encrypted.Config{GroupKey: key})
    
    store.SetItem(ctx, "test", "key1", "secret")
    item, _ := store.GetItem(ctx, "key1")
    
    if item.Value != "secret" {
        t.Errorf("decryption failed")
    }
    
    // Test ECDH key agreement
    alice, _ := encrypted.GenerateBoxKeys()
    bob, _ := encrypted.GenerateBoxKeys()
    
    shared1 := encrypted.GenerateSharedSecret(&alice.PrivateKey, &bob.PublicKey)
    shared2 := encrypted.GenerateSharedSecret(&bob.PrivateKey, &alice.PublicKey)
    
    if shared1 != shared2 {
        t.Errorf("ECDH failed - secrets don't match")
    }
}
```

## Production Considerations

1. **Key Storage**: Store identity keys encrypted with user password
2. **Key Backup**: Allow exporting encrypted keys for recovery
3. **Rate Limiting**: Prevent key request spam on server
4. **Metadata**: Consider padding messages to hide size patterns
5. **Key Rotation**: Auto-rotate keys periodically (e.g., daily)
6. **Audit Logs**: Server logs access (not content) for compliance

## Files to Create

```
sync/encrypted/
├── store.go          # EncryptedStore wrapper ✓
├── exchange.go       # ECDH key agreement
├── server.go         # AuthServer for key distribution
├── client.go         # EncryptedClient for easy use
├── crypto.go         # Helper functions ✓
└── README.md         # Documentation ✓

examples/chat/
├── client/
│   ├── main.go       # Modified to use encryption
│   └── join.go       # Room joining UI
└── server/
    └── rooms.go      # Room management API
```

## Timeline Estimate

| Component | Time |
|-----------|------|
| Core encryption layer | 1 day ✓ |
| Key exchange protocol | 2 days |
| Auth server | 2 days |
| Client integration | 2 days |
| Testing & polish | 2 days |
| **Total** | **~1 week** |

## Next Steps

1. Implement `exchange.go` for ECDH key agreement
2. Add room management API to server
3. Create "Join Room" UI in chat example
4. Write integration tests
5. Security audit (timing attacks, etc.)
