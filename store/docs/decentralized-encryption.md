# Decentralized Encryption - Dumb Server Design

Server is **completely dumb** - just relays opaque encrypted blobs. All access control happens client-side via public keys.

## Core Idea

```
┌────────────────────────────────────────────────────────────────┐
│  SERVER (dumb relay - zero knowledge)                          │
│                                                                │
│  ┌────────────────────────────────────────────────────────┐   │
│  │  Sync Store                                            │   │
│  │  ┌──────────────────────────────────────────────────┐  │   │
│  │  │  __keys/alice        (encrypted for alice)       │  │   │
│  │  │  __keys/bob          (encrypted for bob)         │  │   │
│  │  │  __keys/charlie      (encrypted for charlie)     │  │   │
│  │  │  chat/msg/1          (encrypted with group key)  │  │   │
│  │  │  chat/msg/2          (encrypted with group key)  │  │   │
│  │  │  chat/msg/3          (encrypted with group key)  │  │   │
│  │  └──────────────────────────────────────────────────┘  │   │
│  │                                                         │   │
│  │  Server: "I see only base64 blobs, can't decrypt"      │   │
│  └────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────┘
                              │
                              │ All data opaque to server
                              │
           ┌──────────────────┼──────────────────┐
           │                  │                  │
           ▼                  ▼                  ▼
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│     Alice       │  │      Bob        │  │    Charlie      │
│  ┌───────────┐  │  │  ┌───────────┐  │  │  ┌───────────┐  │
│  │ Private   │  │  │  │ Private   │  │  │  │ Private   │  │
│  │ Key       │  │  │  │ Key       │  │  │  │ Key       │  │
│  └─────┬─────┘  │  │  └─────┬─────┘  │  │  └─────┬─────┘  │
│        │        │  │        │        │  │        │        │
│  Decrypt        │  │  Decrypt        │  │  Decrypt        │
│  __keys/alice   │  │  __keys/bob     │  │  __keys/charlie │
│        │        │  │        │        │  │        │        │
│        ▼        │  │        ▼        │  │        ▼        │
│  Group Key      │  │  Group Key      │  │  Group Key      │
│        │        │  │        │        │  │        │        │
│  Decrypt        │  │  Decrypt        │  │  Decrypt        │
│  chat/*         │  │  chat/*         │  │  chat/*         │
└─────────────────┘  └──────────────────────────────────────┘

Access Control = "Can you decrypt your personal key entry?"
```

## How It Works

### 1. Room Creation

Alice creates an encrypted room:

```go
// Alice generates room
roomID := generateRoomID()  // "room-abc123"
groupKey, _ := encrypted.GenerateKey()  // Random 32-byte key

// Alice defines who can join by listing their pubkeys
members := [][32]byte{bobPub, charliePub}

// For each member, encrypt the group key with their pubkey
for _, memberPub := range members {
    // Encrypt: box.Seal(groupKey, nonce, memberPub, alicePriv)
    encryptedKey, _ := box.Seal(nil, groupKey[:], &nonce, &memberPub, &alicePrivKey)
    
    // Store in sync: __keys/{memberID} = encrypted(groupKey)
    store.SetItem(ctx, "__keys", memberID, base64(encryptedKey))
}

// Store encrypted for herself too
encryptedForAlice, _ := box.Seal(nil, groupKey[:], &nonce, &alicePub, &alicePrivKey)
store.SetItem(ctx, "__keys", "alice", base64(encryptedForAlice))

// Now store actual room data encrypted with groupKey
encStore, _ := encrypted.New(store, encrypted.Config{GroupKey: groupKey})
encStore.SetItem(ctx, "chat", "msg/1", "Hello room!")
```

### 2. Joining a Room

Bob discovers the room (via room ID shared out-of-band):

```go
// Bob connects to room
// 1. Syncs all data (sees opaque blobs)
// 2. Looks for __keys/bob entry
// 3. Decrypts with his private key + alice's pubkey

encryptedKey, err := store.GetItem(ctx, "__keys/bob")
if err != nil {
    // Not invited! Can't join.
    return fmt.Errorf("not authorized for this room")
}

// Decrypt: box.Open(encrypted, nonce, alicePub, bobPriv)
groupKeyBytes, ok := box.Open(nil, encryptedKey, &nonce, &alicePub, &bobPrivKey)
if !ok {
    return fmt.Errorf("decryption failed")
}

var groupKey [32]byte
copy(groupKey[:], groupKeyBytes)

// Now can read all room data!
encStore, _ := encrypted.New(store, encrypted.Config{GroupKey: groupKey})
msg, _ := encStore.GetItem(ctx, "chat/msg/1")
// msg.Value == "Hello room!"
```

### 3. Revocation

Alice removes Charlie:

```go
// 1. Generate NEW group key
newGroupKey, _ := encrypted.GenerateKey()

// 2. Re-encrypt for remaining members (alice, bob)
//    Skip charlie!
for _, memberID := range []string{"alice", "bob"} {  // Charlie removed
    memberPub := getPubKey(memberID)
    encryptedKey, _ := box.Seal(nil, newGroupKey[:], &nonce, &memberPub, &alicePrivKey)
    store.SetItem(ctx, "__keys", memberID, base64(encryptedKey))
}

// 3. Delete old __keys/charlie entry
store.Delete(ctx, "__keys/charlie")

// 4. Start using newGroupKey for new messages
// Old messages still encrypted with old key (forward secrecy for new only)
// Or: re-encrypt all history (complete forward secrecy)
```

## Key Namespaces

| Namespace | Key Format | Content | Encrypted With |
|-----------|-----------|---------|----------------|
| `__keys` | `{peerID}` | Group key (32 bytes) | box (peer's pubkey) |
| `__meta` | `creator` | Creator's pubkey | plaintext |
| `__meta` | `version` | Room key version | plaintext |
| `chat` | `msg/{id}` | Actual messages | secretbox (group key) |
| `chat` | `file/{id}` | File attachments | secretbox (group key) |

## Access Control Logic

```go
func (c *Client) JoinRoom(roomID string) error {
    // 1. Sync metadata
    meta, _ := c.syncRoomMetadata(roomID)
    
    // 2. Check if invited
    myKeyEntry := fmt.Sprintf("__keys/%s", c.myPeerID)
    encryptedKey, err := c.store.GetItem(ctx, myKeyEntry)
    if err != nil {
        return fmt.Errorf("not invited to this room")
    }
    
    // 3. Decrypt group key
    creatorPub := meta.CreatorPubKey
    groupKey, err := c.decryptGroupKey(encryptedKey, creatorPub)
    if err != nil {
        return fmt.Errorf("failed to decrypt room key")
    }
    
    // 4. Verify creator signature (optional but recommended)
    if !c.verifyRoomManifest(meta, creatorPub) {
        return fmt.Errorf("invalid room manifest")
    }
    
    // 5. Join successful!
    c.currentRoom = &Room{
        ID: roomID,
        GroupKey: groupKey,
        Creator: creatorPub,
    }
    
    return nil
}
```

## Server Responsibilities

**Almost nothing!**

```go
// Server just needs standard sync endpoints
mux.Handle("/sync", server.New(store, auth))

// Auth only validates peer identity (not room membership!)
auth := func(r *http.Request) (string, error) {
    // Verify peer's signature on request
    // Or just accept any valid WebSocket (even simpler!)
    return peerID, nil
}
```

Server sees:
- `__keys/alice`: `base64(encrypted_blob)` - can't decrypt
- `chat/msg/1`: `base64(encrypted_blob)` - can't decrypt
- Peer IDs: `alice`, `bob`, `charlie` - knows who's connected
- Timestamps: when data was written

Server **never** sees:
- Who is allowed in the room
- The group key
- Message content
- Who sent what to whom

## Room Discovery

Since server is dumb, room discovery is out-of-band:

```go
// Alice creates room, gets roomID
roomID := createRoom()
fmt.Println("Room created:", roomID)  // "room-abc123"

// Alice sends roomID to Bob via any channel
// - QR code
// - Email
// - Text message
// - URL: https://chat.example.com/?room=abc123

// Bob enters roomID in UI
// Client fetches all data, tries to decrypt __keys/bob
```

## Revocation Strategies

### Option 1: Lazy Rotation
- Old key continues to work for old messages
- New key for new messages
- Revoked peer sees old history but not new

### Option 2: Active Rotation
- Generate new key
- Re-encrypt all history with new key
- Distribute new key to remaining members
- Revoked peer sees nothing

### Option 3: Per-Message Encryption
- Each message encrypted separately for each recipient
- No shared group key
- Revocation = stop encrypting for that peer
- Cost: O(n) encryption per message

## Implementation

```go
// sync/encrypted/decentralized.go

package encrypted

// DecentralizedStore adds room management to EncryptedStore
type DecentralizedStore struct {
    *EncryptedStore  // embed base encrypted store
    myPeerID         string
    myBoxPriv        [32]byte
    myBoxPub         [32]byte
    currentRoom      *Room
}

type Room struct {
    ID         string
    GroupKey   [32]byte
    CreatorPub [32]byte  // Needed to verify signatures
    Version    int       // For key rotation
}

// CreateRoom creates a new encrypted room
func (ds *DecentralizedStore) CreateRoom(roomID string, memberPubKeys [][32]byte) error {
    groupKey, _ := GenerateKey()
    
    // Store metadata
    ds.inner.SetItem(ctx, "__meta", "creator", base64(ds.myBoxPub[:]))
    ds.inner.SetItem(ctx, "__meta", "version", "1")
    
    // Encrypt group key for each member (including self)
    for _, pubKey := range append(memberPubKeys, ds.myBoxPub) {
        encrypted, _ := encryptWithBox(groupKey, pubKey, ds.myBoxPriv)
        memberID := derivePeerID(pubKey)  // pubkey -> peerID
        ds.inner.SetItem(ctx, "__keys", memberID, base64(encrypted))
    }
    
    ds.currentRoom = &Room{ID: roomID, GroupKey: groupKey}
    return nil
}

// JoinRoom attempts to join by decrypting personal key entry
func (ds *DecentralizedStore) JoinRoom(roomID string) error {
    // Look for my key entry
    myKeyEntry := "__keys/" + ds.myPeerID
    encryptedKey, err := ds.inner.GetItem(ctx, myKeyEntry)
    if err != nil {
        return fmt.Errorf("not invited: %w", err)
    }
    
    // Get creator pubkey
    creatorPubStr, _ := ds.inner.GetItem(ctx, "__meta/creator")
    creatorPub := decodePubKey(creatorPubStr)
    
    // Decrypt group key
    groupKey, err := decryptWithBox(encryptedKey, creatorPub, ds.myBoxPriv)
    if err != nil {
        return fmt.Errorf("decryption failed: %w", err)
    }
    
    // Initialize encrypted store with group key
    ds.EncryptedStore, _ = New(ds.inner, Config{GroupKey: groupKey})
    ds.currentRoom = &Room{
        ID: roomID,
        GroupKey: groupKey,
        CreatorPub: creatorPub,
    }
    
    return nil
}

// InviteMember adds a new member (only creator can do this)
func (ds *DecentralizedStore) InviteMember(peerPub [32]byte) error {
    // Verify I'm the creator
    if ds.myBoxPub != ds.currentRoom.CreatorPub {
        return fmt.Errorf("only creator can invite")
    }
    
    // Encrypt current group key for new member
    encrypted, _ := encryptWithBox(ds.currentRoom.GroupKey, peerPub, ds.myBoxPriv)
    peerID := derivePeerID(peerPub)
    return ds.inner.SetItem(ctx, "__keys", peerID, base64(encrypted))
}

// RevokeMember removes a member and rotates key
func (ds *DecentralizedStore) RevokeMember(peerID string, remainingMembers [][32]byte) error {
    // Verify I'm the creator
    if ds.myBoxPub != ds.currentRoom.CreatorPub {
        return fmt.Errorf("only creator can revoke")
    }
    
    // Delete their key entry
    ds.inner.Delete(ctx, "__keys/"+peerID)
    
    // Generate new key
    newKey, _ := GenerateKey()
    ds.currentRoom.GroupKey = newKey
    ds.currentRoom.Version++
    
    // Re-encrypt for remaining members
    for _, pub := range append(remainingMembers, ds.myBoxPub) {
        encrypted, _ := encryptWithBox(newKey, pub, ds.myBoxPriv)
        pid := derivePeerID(pub)
        ds.inner.SetItem(ctx, "__keys", pid, base64(encrypted))
    }
    
    // Update version
    ds.inner.SetItem(ctx, "__meta", "version", fmt.Sprintf("%d", ds.currentRoom.Version))
    
    // Re-initialize with new key
    ds.EncryptedStore, _ = New(ds.inner, Config{GroupKey: newKey})
    
    return nil
}
```

## Advantages

1. **Server is truly dumb** - just a blob relay
2. **No server-side state** - who is allowed is implicit in who can decrypt
3. **Censorship resistant** - server can't block based on content
4. **Simple scaling** - add servers without coordination
5. **Offline-first** - sync works even if server doesn't know room structure

## Trade-offs

1. **Room discovery** - must share room ID out-of-band
2. **Revocation complexity** - need key rotation
3. **No server enforcement** - server can't prevent banned peers from connecting (they just can't decrypt)
4. **Creator is single point of failure** - if creator loses keys, room is locked

## Comparison

| Feature | Server-Mediated | Decentralized (this) |
|---------|-----------------|----------------------|
| Server knowledge | Knows who is allowed | Zero knowledge |
| Revocation | Easy (server blocks) | Hard (key rotation) |
| Room discovery | Server can list rooms | Out-of-band only |
| Censorship resistance | Low | High |
| Complexity | Medium | Low |
| Creator power | High | High (but revocable) |
