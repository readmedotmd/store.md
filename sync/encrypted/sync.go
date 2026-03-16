// Package encrypted provides optional sync-layer wire encryption and signatures.
//
// This is a runtime-optional plugin - if no keys are configured,
// it passes through without encryption (no-op).
//
// Usage:
//
//	// Create encrypted sync with optional keys
//	encSync := encrypted.New(syncenc.Config{
//	    Inner: syncStore,
//	    // If MyKeys is nil, encryption is disabled (passthrough)
//	    MyKeys: boxKeys,  // optional
//	})
//
//	// Or use as plugin via filters
//	filters := encrypted.Filters(syncenc.Config{MyKeys: boxKeys})
//	ss := core.NewWithOptions(store, filters...)
//
package encrypted

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/helpers/encryption"
	"github.com/readmedotmd/store.md/sync/core"
)

// Config for encrypted sync. All fields are optional.
// If MyKeys is nil, encryption is disabled (passthrough mode).
type Config struct {
	Inner core.SyncStore
	// MyKeys for encryption. If nil, encryption is disabled.
	MyKeys *encryption.BoxKeyPair
	// SignPriv/SignPub for signing. If nil, signatures are disabled.
	SignPriv ed25519.PrivateKey
	SignPub  ed25519.PublicKey
	// PeerBoxKeys: peerID -> box public key (for encryption to peer)
	// If empty, encryption only works if peer sends us their key first.
	PeerBoxKeys map[string][32]byte
	// PeerSignKeys: peerID -> sign public key (for verifying from peer)
	PeerSignKeys map[string]ed25519.PublicKey
}

// EncryptedSync wraps a SyncStore with optional wire encryption.
// If no keys are configured, it acts as a passthrough.
type EncryptedSync struct {
	inner      core.SyncStore
	myKeys     *encryption.BoxKeyPair // nil = encryption disabled
	mySignPriv ed25519.PrivateKey     // nil = signing disabled
	mySignPub  ed25519.PublicKey
	peerKeys   map[string][32]byte        // peerID -> box pubkey
	peerSigs   map[string]ed25519.PublicKey // peerID -> sign pubkey
}

// New creates an encrypted sync wrapper. If cfg.MyKeys is nil,
// encryption is disabled and the wrapper acts as a passthrough.
func New(cfg Config) *EncryptedSync {
	es := &EncryptedSync{
		inner:      cfg.Inner,
		myKeys:     cfg.MyKeys,
		mySignPriv: cfg.SignPriv,
		mySignPub:  cfg.SignPub,
		peerKeys:   cfg.PeerBoxKeys,
		peerSigs:   cfg.PeerSignKeys,
	}
	if es.peerKeys == nil {
		es.peerKeys = make(map[string][32]byte)
	}
	if es.peerSigs == nil {
		es.peerSigs = make(map[string]ed25519.PublicKey)
	}
	return es
}

// enabled returns true if encryption/signing is enabled.
func (es *EncryptedSync) enabled() bool {
	return es.myKeys != nil
}

// Sync encrypts outgoing and decrypts incoming payloads.
// If encryption is disabled (no keys), acts as passthrough.
func (es *EncryptedSync) Sync(ctx context.Context, peerID string, payload *core.SyncPayload) (*core.SyncPayload, error) {
	// Decrypt incoming if present and encryption is enabled
	if es.enabled() && payload != nil && len(payload.Items) > 0 {
		decrypted, err := es.decrypt(peerID, payload)
		if err != nil {
			return nil, fmt.Errorf("decrypt: %w", err)
		}
		payload = decrypted
	}

	// Call inner sync
	response, err := es.inner.Sync(ctx, peerID, payload)
	if err != nil {
		return nil, err
	}

	// Encrypt outgoing response if enabled
	if es.enabled() && response != nil && len(response.Items) > 0 {
		encrypted, err := es.encrypt(peerID, response)
		if err != nil {
			return nil, fmt.Errorf("encrypt: %w", err)
		}
		response = encrypted
	}

	return response, nil
}

// Pass-through methods to inner store (local operations, unencrypted).
func (es *EncryptedSync) SetItem(ctx context.Context, ns, key, value string) error {
	return es.inner.SetItem(ctx, ns, key, value)
}

func (es *EncryptedSync) GetItem(ctx context.Context, key string) (*core.SyncStoreItem, error) {
	return es.inner.GetItem(ctx, key)
}

func (es *EncryptedSync) ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]core.SyncStoreItem, error) {
	return es.inner.ListItems(ctx, prefix, startAfter, limit)
}

func (es *EncryptedSync) OnUpdate(fn core.UpdateListener) func() {
	return es.inner.OnUpdate(fn)
}

func (es *EncryptedSync) Close() error {
	return es.inner.Close()
}

// Get implements storemd.Store.
func (es *EncryptedSync) Get(ctx context.Context, key string) (string, error) {
	return es.inner.Get(ctx, key)
}

// Set implements storemd.Store.
func (es *EncryptedSync) Set(ctx context.Context, key, value string) error {
	return es.inner.Set(ctx, key, value)
}

// Delete implements storemd.Store.
func (es *EncryptedSync) Delete(ctx context.Context, key string) error {
	return es.inner.Delete(ctx, key)
}

// List implements storemd.Store.
func (es *EncryptedSync) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	return es.inner.List(ctx, args)
}

// AddPeer adds a peer's public keys for encryption/verification.
func (es *EncryptedSync) AddPeer(peerID string, boxPub [32]byte, signPub ed25519.PublicKey) {
	es.peerKeys[peerID] = boxPub
	es.peerSigs[peerID] = signPub
}

// RemovePeer removes a peer.
func (es *EncryptedSync) RemovePeer(peerID string) {
	delete(es.peerKeys, peerID)
	delete(es.peerSigs, peerID)
}

// MyBoxPublicKey returns my box public key (empty if encryption disabled).
func (es *EncryptedSync) MyBoxPublicKey() [32]byte {
	if es.myKeys == nil {
		return [32]byte{}
	}
	return es.myKeys.PublicKey
}

// MySigningPublicKey returns my Ed25519 public key (nil if signing disabled).
func (es *EncryptedSync) MySigningPublicKey() ed25519.PublicKey {
	return es.mySignPub
}

// BatchWireFormat is the encrypted wire format for the whole payload.
// We encrypt the entire SyncPayload as a single blob (batch encryption).
type BatchWireFormat struct {
	// Version of the wire format
	Version int `json:"v"`
	// Sender's signing public key (for signature verification)
	SenderSignPub []byte `json:"sender,omitempty"`
	// Ephemeral public key for box encryption
	EphemeralPub [32]byte `json:"epk,omitempty"`
	// Nonce for encryption
	Nonce [24]byte `json:"n,omitempty"`
	// Encrypted payload (JSON of SyncPayload)
	Ciphertext []byte `json:"ct,omitempty"`
	// Signature of the encrypted data (detached)
	Signature []byte `json:"sig,omitempty"`
}

// encrypt encrypts the entire payload as a single batch.
func (es *EncryptedSync) encrypt(peerID string, payload *core.SyncPayload) (*core.SyncPayload, error) {
	peerPub, ok := es.peerKeys[peerID]
	if !ok {
		return nil, fmt.Errorf("unknown peer: %s", peerID)
	}

	// Serialize the entire payload
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	// Encrypt the whole payload at once
	ciphertext, err := encryption.BoxEncrypt(payloadJSON, &peerPub, &es.myKeys.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("box encrypt: %w", err)
	}

	// Parse ephemeral pub and nonce from ciphertext
	var ephemeralPub [32]byte
	copy(ephemeralPub[:], ciphertext[:32])

	var nonce [24]byte
	copy(nonce[:], ciphertext[32:56])

	// Just the box ciphertext (without header)
	boxCiphertext := ciphertext[56:]

	// Build wire format
	wire := BatchWireFormat{
		Version:       1,
		SenderSignPub: es.mySignPub,
		EphemeralPub:  ephemeralPub,
		Nonce:         nonce,
		Ciphertext:    boxCiphertext,
	}

	// Sign the ciphertext if we have signing keys
	if es.mySignPriv != nil {
		sig := encryption.Sign(ciphertext, es.mySignPriv)
		wire.Signature = sig
	}

	// Serialize wire format
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	// Return a single "batch" item containing the encrypted payload
	return &core.SyncPayload{
		Items: []core.SyncStoreItem{
			{
				App:       "__enc",
				Key:       "batch",
				Value:     base64.StdEncoding.EncodeToString(wireJSON),
				// Include pubkey/sig in the item for visibility
				PublicKey: base64.StdEncoding.EncodeToString(es.mySignPub),
				Signature: base64.StdEncoding.EncodeToString(wire.Signature),
			},
		},
	}, nil
}

// decrypt decrypts a batch payload from a specific peer.
func (es *EncryptedSync) decrypt(peerID string, payload *core.SyncPayload) (*core.SyncPayload, error) {
	if len(payload.Items) == 0 {
		return payload, nil
	}

	// Find the batch item (should be the only item)
	var batchItem *core.SyncStoreItem
	for i := range payload.Items {
		if payload.Items[i].App == "__enc" && payload.Items[i].Key == "batch" {
			batchItem = &payload.Items[i]
			break
		}
	}

	// If no batch item found, assume plaintext (backward compatible)
	if batchItem == nil {
		return payload, nil
	}

	// Decode wire format
	wireJSON, err := base64.StdEncoding.DecodeString(batchItem.Value)
	if err != nil {
		return nil, fmt.Errorf("decode batch: %w", err)
	}

	var wire BatchWireFormat
	if err := json.Unmarshal(wireJSON, &wire); err != nil {
		return nil, fmt.Errorf("unmarshal wire: %w", err)
	}

	// Reconstruct full ciphertext
	fullCiphertext := make([]byte, 32+24+len(wire.Ciphertext))
	copy(fullCiphertext[:32], wire.EphemeralPub[:])
	copy(fullCiphertext[32:56], wire.Nonce[:])
	copy(fullCiphertext[56:], wire.Ciphertext)

	// Verify signature if present
	if len(wire.Signature) > 0 && len(wire.SenderSignPub) > 0 {
		if !encryption.Verify(fullCiphertext, wire.Signature, wire.SenderSignPub) {
			return nil, fmt.Errorf("signature verification failed")
		}
	}

	// Decrypt - try to find which peer sent this by attempting decryption with each known peer key
	// The sender's public key is needed for box.Open
	var plaintext []byte
	var decryptErr error
	
	// First try the peerID we think sent this
	if senderPub, ok := es.peerKeys[peerID]; ok {
		plaintext, decryptErr = encryption.BoxDecrypt(fullCiphertext, &senderPub, &es.myKeys.PrivateKey)
	}
	
	// If that fails, try all known peers (in case sender hint was wrong)
	if plaintext == nil {
		for _, senderPub := range es.peerKeys {
			plaintext, decryptErr = encryption.BoxDecrypt(fullCiphertext, &senderPub, &es.myKeys.PrivateKey)
			if plaintext != nil {
				break
			}
		}
	}
	
	if plaintext == nil {
		return nil, fmt.Errorf("decrypt: %w", decryptErr)
	}

	// Parse decrypted payload
	var decryptedPayload core.SyncPayload
	if err := json.Unmarshal(plaintext, &decryptedPayload); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	return &decryptedPayload, nil
}
