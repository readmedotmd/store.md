# Encrypted Sync - Optional Plugins

End-to-end encryption and access control for store.md sync as **runtime-optional plugins**.

## Features

- **Batch encryption** - Encrypt entire payloads as single blobs (not per-item)
- **Sign on save** - Items signed when you save them, not during sync
- **Plugin architecture** - Enable only what you need via sync/core hooks
- **Access control** - Filter which peers can send/receive which items
- **Backward compatible** - Works with unencrypted peers (graceful fallback)

## Quick Start

### Option 1: Signatures Only (Sign on Save)

```go
import (
    "github.com/readmedotmd/store.md/helpers/encryption"
    syncenc "github.com/readmedotmd/store.md/sync/encrypted"
)

// Generate signing keys
signPriv, signPub, _ := encryption.GenerateSigningKeys()

// Get peer's public key (out of band)
peerSignPub := getPeerPublicKey("bob")

// Create store that signs items when I save them
ss := core.NewWithOptions(store,
    // Sign items I create
    syncenc.SignOnSave(signPriv, signPub),
    // Verify signatures from peers
    syncenc.VerifySignatures(map[string]ed25519.PublicKey{
        "bob": peerSignPub,
    }),
)

// When I save an item, it's automatically signed
ss.SetItem(ctx, "chat", "msg/1", "Hello!")
// Stored with PublicKey and Signature fields

// During sync, peers receive my signed items and can verify
```

### Option 2: Full Encryption + Signatures

```go
// Generate keys
boxKeys, _ := encryption.GenerateBoxKeys()
signPriv, signPub, _ := encryption.GenerateSigningKeys()

// Create base store with signing
ss := core.NewWithOptions(store,
    syncenc.SignOnSave(signPriv, signPub),
    syncenc.VerifySignatures(peerSignKeys),
)

// Wrap with encryption layer
encSync := syncenc.New(syncenc.Config{
    Inner:        ss,
    MyKeys:       boxKeys,
    SignPriv:     signPriv,
    SignPub:      signPub,
    PeerBoxKeys:  map[string][32]byte{"bob": bobBoxPub},
    PeerSignKeys: map[string]ed25519.PublicKey{"bob": bobSignPub},
})

// Everything is signed AND encrypted
encSync.SetItem(ctx, "chat", "msg", "Secret!")
```

### Option 3: Access Control Only

```go
// No crypto, just filter what peers can see
ac := syncenc.NewAccessControl()
ac.AllowApp("peer1", "chat")  // peer1 only sees "chat" app
ac.BlockPeer("peer2")          // peer2 blocked completely

ss := core.NewWithOptions(store, ac.Options()...)
```

### Option 4: Passthrough (No Security)

```go
// No encryption, no signatures
ss := core.New(store)
```

## How It Works

### Sign on Save

```
When I call SetItem:
  1. Core creates item with Timestamp, ID, WriteTimestamp
  2. PreSetItem hook (SignOnSave) adds PublicKey and Signature
  3. Item is stored with signature

When item syncs to peer:
  1. Peer receives item with PublicKey and Signature
  2. SyncInFilter (VerifySignatures) checks signature
  3. If valid, item is stored; if invalid, rejected
```

### Batch Encryption

The encryption layer wraps the entire SyncPayload:

```go
// BatchWireFormat is the encrypted container
type BatchWireFormat struct {
    Version       int       `json:"v"`          // format version
    SenderSignPub []byte    `json:"sender"`     // who sent it  
    EphemeralPub  [32]byte  `json:"epk"`        // for ECDH
    Nonce         [24]byte  `json:"n"`          // for XSalsa20
    Ciphertext    []byte    `json:"ct"`         // encrypted SyncPayload
    Signature     []byte    `json:"sig"`        // detached Ed25519 sig
}
```

Benefits over per-item encryption:
- One signature per batch (not per item)
- Better compression
- Less overhead for small items

## API Reference

### SignOnSave

```go
// SignOnSave returns a PreSetItem hook that signs items when I save them.
func SignOnSave(signPriv ed25519.PrivateKey, signPub ed25519.PublicKey) core.Option
```

- Only signs items **you create** via `SetItem`
- Does NOT sign items received from other peers
- Signature covers: App, Key, Value, Timestamp, ID, Deleted

### VerifySignatures

```go
// VerifySignatures returns a SyncInFilter that verifies item signatures.
func VerifySignatures(peerSignKeys map[string]ed25519.PublicKey) core.Option
```

- Rejects items with invalid or missing signatures
- Checks that PublicKey matches trusted key for that peer
- Items must be signed by trusted peer to be accepted

### AccessControl

```go
ac := syncenc.NewAccessControl()
ac.AllowPeer(peerID string)              // Allow peer to sync
ac.BlockPeer(peerID string)              // Block peer completely
ac.AllowApp(peerID, app string)          // Allow peer to receive specific app
opts := ac.Options()                      // Get filter options
```

### EncryptedSync

```go
// New creates encrypted sync wrapper. If cfg.MyKeys is nil, acts as passthrough.
func New(cfg Config) *EncryptedSync

type Config struct {
    Inner        core.SyncStore
    MyKeys       *encryption.BoxKeyPair      // nil = no encryption
    SignPriv     ed25519.PrivateKey          // nil = no signing
    SignPub      ed25519.PublicKey
    PeerBoxKeys  map[string][32]byte         // peerID -> box pubkey
    PeerSignKeys map[string]ed25519.PublicKey // peerID -> sign pubkey
}
```

## SyncStoreItem Fields

```go
type SyncStoreItem struct {
    App            string `json:"app"`
    Key            string `json:"key"`
    Value          string `json:"value"`
    Timestamp      int64  `json:"timestamp"`
    ID             string `json:"id"`
    WriteTimestamp int64  `json:"writeTimestamp"`
    Deleted        bool   `json:"deleted,omitempty"`
    
    // NEW: Set by SignOnSave, verified by VerifySignatures
    PublicKey string `json:"publicKey,omitempty"`  // sender's public key (base64)
    Signature string `json:"signature,omitempty"`  // signature of item (base64)
}
```

## Security Properties

| Property | How | Optional? |
|----------|-----|-----------|
| **Authentication** | Ed25519 signatures | Yes - SignOnSave |
| **Verification** | Signature check on receive | Yes - VerifySignatures |
| **Confidentiality** | NaCl box (XSalsa20 + Poly1305) | Yes - EncryptedSync |
| **Integrity** | Poly1305 MAC + signatures | Yes - depends on above |
| **Access Control** | Sync filters | Yes - AccessControl |

## Examples

### Basic Signing

```go
package main

import (
    "context"
    "fmt"

    "github.com/readmedotmd/store.md/backend/memory"
    "github.com/readmedotmd/store.md/helpers/encryption"
    "github.com/readmedotmd/store.md/sync/core"
    syncenc "github.com/readmedotmd/store.md/sync/encrypted"
)

func main() {
    // Generate keys
    myPriv, myPub, _ := encryption.GenerateSigningKeys()
    peerPub := getPeerPublicKey() // out-of-band exchange

    // Create store
    store := memory.New()
    ss := core.NewWithOptions(store,
        syncenc.SignOnSave(myPriv, myPub),
        syncenc.VerifySignatures(map[string]ed25519.PublicKey{
            "peer1": peerPub,
        }),
    )

    // Save signed item
    ctx := context.Background()
    ss.SetItem(ctx, "chat", "msg/1", "Hello!")

    // Get item - includes signature
    item, _ := ss.GetItem(ctx, "msg/1")
    fmt.Printf("PublicKey: %s...\n", item.PublicKey[:20])
    fmt.Printf("Signature: %s...\n", item.Signature[:20])
}
```

### Mixed Environment (Some Peers Signed, Some Not)

```go
// Allow both signed and unsigned items
ss := core.NewWithOptions(store,
    syncenc.SignOnSave(myPriv, myPub),  // sign my items
    syncenc.AllowUnsigned(),             // accept unsigned from legacy peers
)

// Or: Require signatures from some peers, allow others
strictFilter := syncenc.VerifySignatures(trustedKeys)
permissiveFilter := syncenc.AllowUnsigned()

// Apply strict to specific peers in SyncInFilter based on peerID
```

## Dependencies

- `helpers/encryption` - NaCl crypto primitives
- `golang.org/x/crypto/nacl` - Actual crypto implementations
- `sync/core` - Hook system (PreSetItem, SyncInFilter, etc.)
