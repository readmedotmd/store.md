# Encryption & Access Control Proposal

This document proposes adding end-to-end encryption and access control to the chat example using **NaCl** (Networking and Cryptography library) via `golang.org/x/crypto/nacl`.

## Goals

1. **End-to-End Encryption** - Messages encrypted before leaving browser
2. **Access Control** - Only authorized peers can join the chat
3. **Message Authenticity** - Verify message sender identity
4. **Forward Secrecy** - Keys rotated periodically

## Threat Model

| Threat | Protection |
|--------|------------|
| Server reads messages | ✅ E2E encryption |
| MITM intercepts | ✅ Authenticated encryption (box) |
| Impersonation | ✅ Digital signatures |
| Replay attacks | ✅ Nonce + timestamp verification |
| Unauthorized join | ✅ Access tokens with crypto verification |

## Architecture

### Option 1: Public Key Encryption (box)

Each peer generates a keypair. Messages are encrypted per-recipient.

```
┌─────────────┐                     ┌─────────────┐
│   Peer A    │                     │   Peer B    │
│  ┌───────┐  │                     │  ┌───────┐  │
│  │PrivKey│  │                     │  │PrivKey│  │
│  │PubKey │──┼──► box.Seal() ─────►│──┼►PubKey │  │
│  │(saved)│  │                     │  │(saved) │  │
│  └───────┘  │                     │  └───────┘  │
└─────────────┘                     └─────────────┘
```

**Pros:** True E2E, no shared secrets  
**Cons:** O(n) encryption for n recipients, server sees metadata

### Option 2: Group Shared Secret (secretbox)

All authorized peers share a symmetric key for the chat room.

```
┌─────────────┐                     ┌─────────────┐
│   Peer A    │    secretbox.Seal   │   Peer B    │
│  ┌───────┐  │ ──────────────────► │  ┌───────┐  │
│  │Group  │  │                     │  │Group  │  │
│  │Key    │  │ ◄────────────────── │  │Key    │  │
│  └───────┘  │    secretbox.Open   │  └───────┘  │
└─────────────┘                     └─────────────┘
```

**Pros:** Fast, O(1) encryption, simple  
**Cons:** Key distribution challenge, all-or-nothing access

### Option 3: Hybrid (Recommended)

Combine both approaches for best security and flexibility.

```
Setup Phase (Joining):
  1. Server provides room public key (box)
  2. New peer generates ephemeral keypair
  3. Peer encrypts join request with server's public key
  4. Server verifies access token, sends group key (box)

Messaging:
  ┌─────────────────────────────────────────────────────┐
  │  Message → secretbox.Seal(groupKey) → Sync Store   │
  └─────────────────────────────────────────────────────┘
```

## Implementation Design

### 1. Key Structure

```go
// PeerKeyPair - each peer's identity
 type PeerKeyPair struct {
     PublicKey  [32]byte  // curve25519
     PrivateKey [32]byte  // curve25519
 }

// RoomKey - shared symmetric key for group
 type RoomKey struct {
     Key [32]byte  // xsalsa20 key
     Nonce [24]byte // xsalsa20 nonce prefix
 }
```

### 2. Message Format

```go
// EncryptedMessage wraps encrypted content with metadata
type EncryptedMessage struct {
    Version   int      // 1 = current
    Sender    [32]byte // Sender's public key
    Timestamp int64    // Unix nanos (anti-replay)
    Nonce     [24]byte // unique per message
    Ciphertext []byte  // secretbox encrypted
    Signature []byte   // detached Ed25519 signature
}
```

### 3. Access Control

```go
// AccessToken contains room access credentials
type AccessToken struct {
    RoomID    string   // Room identifier
    ExpiresAt int64    // Token expiration
    Role      string   // "admin", "member", "readonly"
}

// Signed by room owner's private key
func (t *AccessToken) Sign(ownerPrivateKey ed25519.PrivateKey) ([]byte, error)
func (t *AccessToken) Verify(ownerPublicKey ed25519.PublicKey) error
```

### 4. Encryption Flow

```go
// Sending
func (c *ChatClient) SendMessage(content string) error {
    // 1. Create message
    msg := Message{
        Content:   content,
        Timestamp: time.Now().UnixNano(),
        From:      c.peerID,
    }
    
    // 2. Serialize
    plaintext, _ := json.Marshal(msg)
    
    // 3. Encrypt with room key
    nonce := generateNonce()
    ciphertext := secretbox.Seal(nil, plaintext, &nonce, &c.roomKey.Key)
    
    // 4. Sign
    signature := ed25519.Sign(c.privateKey, ciphertext)
    
    // 5. Store encrypted
    encrypted := EncryptedMessage{
        Version:    1,
        Sender:     c.publicKey,
        Timestamp:  msg.Timestamp,
        Nonce:      nonce,
        Ciphertext: ciphertext,
        Signature:  signature,
    }
    
    return c.syncStore.SetItem(ctx, "chat", key, encrypted)
}
```

```go
// Receiving
func (c *ChatClient) onEncryptedMessage(item SyncStoreItem) {
    var em EncryptedMessage
    json.Unmarshal([]byte(item.Value), &em)
    
    // 1. Verify signature
    if !ed25519.Verify(em.Sender, em.Ciphertext, em.Signature) {
        log("Invalid signature, dropping message")
        return
    }
    
    // 2. Check timestamp (anti-replay)
    if em.Timestamp < time.Now().Add(-5*time.Minute).UnixNano() {
        log("Stale message, dropping")
        return
    }
    
    // 3. Decrypt
    plaintext, ok := secretbox.Open(nil, em.Ciphertext, &em.Nonce, &c.roomKey.Key)
    if !ok {
        log("Decryption failed")
        return
    }
    
    // 4. Display
    var msg Message
    json.Unmarshal(plaintext, &msg)
    displayMessage(msg)
}
```

### 5. Key Distribution

**Problem:** How do new peers get the room key securely?

**Solution:** Server-mediated key exchange with authentication

```go
// Server-side: Room owner creates invite
func (s *Server) CreateInvite(roomID string, targetPeerPubKey [32]byte) ([]byte, error) {
    // Encrypt room key for specific peer
    roomKey := s.getRoomKey(roomID)
    encryptedKey := box.Seal(nil, roomKey[:], &nonce, &targetPeerPubKey, &s.serverPrivateKey)
    return encryptedKey, nil
}

// Client-side: Join room with invite
func (c *ChatClient) JoinRoom(encryptedKey []byte) error {
    // Decrypt room key
    roomKeyBytes, ok := box.Open(nil, encryptedKey, &nonce, &serverPublicKey, &c.privateKey)
    if !ok {
        return fmt.Errorf("failed to decrypt room key")
    }
    copy(c.roomKey.Key[:], roomKeyBytes)
    return nil
}
```

## Files to Modify

### New Files

```
examples/chat/
├── crypto/
│   ├── keys.go        # Key generation, storage
│   ├── encrypt.go     # Encryption/decryption
│   ├── access.go      # Token generation/verification
│   └── invite.go      # Room key distribution
```

### Modified Files

```
examples/chat/client/main.go
  - Add key generation on first load
  - Encrypt messages before sending
  - Decrypt messages on receive
  - Store keys in IndexedDB (encrypted at rest)

examples/chat/server/main.go
  - Add room management endpoints
  - Validate access tokens
  - Distribute room keys to authorized peers

examples/chat/server/index.html
  - Add invite code input
  - Add key backup/restore UI
```

## Security Considerations

### 1. Key Storage (Browser)

```go
// Keys stored in IndexedDB, encrypted with password-derived key
func storeKeys(password string, keys *PeerKeyPair) error {
    // Argon2id to derive key from password
    argonKey := argon2id.Key(password, salt, time, memory, threads, 32)
    
    // Encrypt private key
    var nonce [24]byte
    rand.Read(nonce[:])
    encryptedPriv := secretbox.Seal(nil, keys.PrivateKey[:], &nonce, &argonKey)
    
    // Store: publicKey, encryptedPriv, nonce, salt
    return idbStore.Set(ctx, "keys", keyData)
}
```

### 2. Forward Secrecy

Rotate room key periodically:
- New key encrypted with old key
- Old messages remain encrypted with old key
- Active peers get new key via encrypted broadcast

### 3. Metadata Leakage

Server still sees:
- Who is connected (peer IDs)
- Message timing and size
- Room membership

**Mitigations:**
- Pad messages to fixed sizes
- Mix traffic through server delays
- Use Tor for transport (optional)

## Implementation Phases

### Phase 1: Basic E2E Encryption
- [ ] Add NaCl dependency
- [ ] Generate peer keypairs
- [ ] Implement secretbox encryption
- [ ] Store keys in IndexedDB

### Phase 2: Access Control
- [ ] Room key distribution
- [ ] Invite codes
- [ ] Access token verification

### Phase 3: Hardening
- [ ] Signatures for authenticity
- [ ] Anti-replay timestamps
- [ ] Key rotation
- [ ] Password-protected key storage

### Phase 4: UX
- [ ] Key backup/restore UI
- [ ] Invite generation UI
- [ ] Security settings panel

## Dependencies

```go
import (
    "golang.org/x/crypto/nacl/box"
    "golang.org/x/crypto/nacl/secretbox"
    "golang.org/x/crypto/argon2"
    "crypto/ed25519"
    "crypto/rand"
)
```

## Example Usage

```bash
# Start encrypted chat room
cd examples/chat
ENABLE_ENCRYPTION=true go run server/main.go

# Browser 1: Create room
# - Click "Create Encrypted Room"
# - Save invite code: "room-abc123-def456"

# Browser 2: Join room
# - Enter invite code
# - Keys exchanged automatically
# - Start chatting securely
```

## References

- [NaCl: Networking and Cryptography library](https://nacl.cr.yp.to/)
- [golang.org/x/crypto/nacl](https://pkg.go.dev/golang.org/x/crypto/nacl)
- [Curve25519](https://cr.yp.to/ecdh.html)
- [XSalsa20](https://cr.yp.to/snuffle.html)
- [Ed25519](https://ed25519.cr.yp.to/)

## Summary

This proposal adds strong encryption and access control using battle-tested NaCl primitives:

- **`box`** - Secure key exchange (Curve25519 + XSalsa20)
- **`secretbox`** - Fast group messaging (XSalsa20 + Poly1305)
- **`ed25519`** - Message signatures for authenticity
- **`argon2id`** - Password-based key derivation

The hybrid approach gives us:
- ✅ True end-to-end encryption
- ✅ Server cannot read messages
- ✅ Verified sender identity
- ✅ Efficient group messaging
- ✅ Controlled access via invites
