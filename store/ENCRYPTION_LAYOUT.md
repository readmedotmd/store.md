# Encryption Layout

## Folder Structure

```
store.md/
├── helpers/
│   └── encryption/           # Generic crypto utilities
│       ├── keys.go          # Key generation, ECDH, box/secretbox
│       └── README.md        # Docs for crypto helpers
│
├── sync/
│   └── encrypted/            # Sync-layer wire encryption
│       ├── wire.go          # EncryptedSync wrapper
│       └── README.md        # Docs for sync encryption
│
└── ENCRYPTION_LAYOUT.md     # This file
```

## Separation of Concerns

| Layer | Purpose | Depends On |
|-------|---------|------------|
| `helpers/encryption` | Generic NaCl primitives | Only stdlib + golang.org/x/crypto |
| `sync/encrypted` | Sync wire encryption | helpers/encryption, sync/core |

## How It Works

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                               APPLICATION                                    │
│                                                                              │
│   Your app code:                                                             │
│   ```go                                                                      │
│   syncStore.SetItem(ctx, "chat", "msg", "hi")  // Plaintext locally!        │
│   ```                                                                        │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                          sync/encrypted                                     │
│                                                                              │
│   ┌─────────────────────────────────────────────────────────────────────┐  │
│   │   EncryptedSync wrapper                                             │  │
│   │                                                                     │  │
│   │   Sync() called with payload:                                       │  │
│   │   ```                                                               │  │
│   │   items: [{Key: "chat/msg", Value: "hi", ...}]                      │  │
│   │   ```                                                               │  │
│   │                                                                     │  │
│   │   1. For each item:                                                 │  │
│   │      - Serialize to JSON                                            │  │
│   │      - Call helpers/encryption.BoxEncrypt()                         │  │
│   │        → Generates ephemeral keypair (random!)                      │  │
│   │        → ECDH: ephemeralPriv + peerPub → shared secret              │  │
│   │        → XSalsa20 + Poly1305 encryption                             │  │
│   │      - Call helpers/encryption.Sign()                               │  │
│   │        → Ed25519 signature                                          │  │
│   │                                                                     │  │
│   │   2. Format as WireItem:                                            │  │
│   │      {ephemeralPub, nonce, ciphertext, signature}                   │  │
│   │                                                                     │  │
│   │   3. Base64 encode → inner SyncStore.Sync()                         │  │
│   │                                                                     │  │
│   │   Result: Encrypted payload on wire!                                │  │
│   └─────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       │ Wire: Encrypted blob
                                       │ "eyJlcGsiOiAiQUFBLi4u"
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           helpers/encryption                                │
│                                                                              │
│   Generic crypto functions used by sync/encrypted:                          │
│                                                                              │
│   - GenerateBoxKeys()       → Curve25519 keypair                          │
│   - GenerateSharedSecret()  → ECDH key agreement                          │
│   - DeriveKey()             → HKDF-like key derivation                    │
│   - BoxEncrypt()            → NaCl box (ephemeral keys)                   │
│   - BoxDecrypt()            → Decrypt with private key                    │
│   - Sign()/Verify()         → Ed25519 signatures                          │
│   - GenerateSecretKey()     → Symmetric key for secretbox                 │
│                                                                              │
│   Used by: sync/encrypted (and any other package needing crypto)            │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       │ Calls into
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         golang.org/x/crypto/nacl                            │
│                                                                              │
│   - box        (Curve25519 + XSalsa20 + Poly1305)                          │
│   - secretbox  (XSalsa20 + Poly1305)                                       │
│                                                                              │
│   Industry-standard, well-audited implementations.                          │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Data Flow Example

```
Alice wants to send "Hello" to Bob:

1. Alice's app:
   syncStore.SetItem(ctx, "chat", "msg/1", "Hello")
   → Goes to inner store (plaintext locally)

2. Sync triggers:
   encSync.Sync("bob", payload)
   → Wraps with encryption

3. Encryption per item:
   - Generate ephemeral Curve25519 keypair
   - ECDH: ephemeralPriv + bobPub → shared secret
   - Encrypt: XSalsa20("Hello") + Poly1305 MAC
   - Sign: Ed25519(ciphertext)
   
4. Wire format:
   {
     "epk": "base64(ephemeralPub)",
     "n":   "base64(nonce)",
     "ct":  "base64(ciphertext)",
     "sig": "base64(signature)"
   }

5. Server sees:
   Opaque base64 blob, cannot decrypt

6. Bob receives:
   encSync.Sync("alice", encryptedPayload)
   → Decrypts each item

7. Bob's decryption:
   - Verify Ed25519 signature (proves Alice sent it)
   - Extract ephemeralPub from WireItem
   - ECDH: ephemeralPub + bobPriv → same shared secret!
   - Decrypt: XSalsa20 + Poly1305 verification
   → Returns plaintext "Hello"

8. Bob's inner store:
   Stores "Hello" (plaintext locally)
```

## Key Properties

| Property | How It's Achieved |
|----------|-------------------|
| **Different encryption per message** | Random ephemeral keypair per item |
| **Authentication** | Ed25519 signatures |
| **Forward secrecy** | Ephemeral keys discarded after use |
| **Local performance** | Plaintext storage (no crypto overhead) |
| **Wire security** | ECDH + XSalsa20 + Poly1305 |

## Usage

### 1. Generate Keys (once)

```go
import "github.com/readmedotmd/store.md/helpers/encryption"

boxKeys, _ := encryption.GenerateBoxKeys()
signPriv, signPub, _ := encryption.GenerateSigningKeys()

// Save securely!
```

### 2. Setup Encrypted Sync

```go
import (
    "github.com/readmedotmd/store.md/helpers/encryption"
    syncenc "github.com/readmedotmd/store.md/sync/encrypted"
)

// Local store (plaintext)
local := bbolt.New("data.db")
syncStore := core.New(local)

// Wrap with wire encryption
encSync := syncenc.New(syncenc.Config{
    Inner:    syncStore,
    MyKeys:   boxKeys,
    SignPriv: signPriv,
    SignPub:  signPub,
    Peers: map[string][32]byte{
        "bob": bobPubKey,
    },
})
```

### 3. Use Normally

```go
// This is fast - local plaintext!
syncStore.SetItem(ctx, "chat", "msg", "secret")

// Automatically encrypted when syncing to Bob
// Bob needs your pubkey to decrypt
```

## Why This Layout?

| Benefit | Explanation |
|---------|-------------|
| **Reusable crypto** | `helpers/encryption` can be used by any package |
| **Sync-specific logic** | `sync/encrypted` only handles sync wire format |
| **No bloat** | Don't import sync stuff if you just need crypto |
| **Testable** | Crypto helpers tested independently |
| **Clear deps** | sync/encrypted → helpers/encryption (not reverse) |

## Direct Dependencies

```
sync/encrypted/wire.go
    ├── helpers/encryption (crypto primitives)
    ├── sync/core (SyncStore interface)
    └── golang.org/x/crypto/nacl/box (actual crypto)

helpers/encryption/keys.go
    └── golang.org/x/crypto/nacl/* (actual crypto)
```
