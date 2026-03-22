// Package encrypted provides optional encryption and access control plugins.
//
// This file contains runtime-optional features that can be composed
// via the sync/core hook system.
//
// Example: Sign items on save (not on sync)
//
//	// Sign items when I save them
//	ss := core.NewWithOptions(store,
//	    encrypted.SignOnSave(mySignPriv, mySignPub),
//	)
//	
//	// Now when I SetItem, it gets signed immediately
//	ss.SetItem(ctx, "chat", "msg/1", "Hello!")
//	// The item in the store has PublicKey and Signature fields set
//
//	// During sync, peers can verify my signature
//	ss := core.NewWithOptions(store,
//	    encrypted.SignOnSave(mySignPriv, mySignPub),
//	    encrypted.VerifySignatures(peerSignKeys),
//	)
//
package encrypted

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"github.com/readmedotmd/store.md/helpers/encryption"
	"github.com/readmedotmd/store.md/sync/core"
)

// SignOnSave returns a PreSetItem hook that signs items when I save them.
// The signature covers App, Key, Value, Timestamp, ID, and Deleted fields.
// Items from other peers (via SyncIn) are NOT signed by this hook - only items
// I create via SetItem are signed.
func SignOnSave(signPriv ed25519.PrivateKey, signPub ed25519.PublicKey) core.Option {
	return core.WithPreSetItem(func(item *core.SyncStoreItem) {
		// Only sign items that don't already have a signature
		// (avoids re-signing items received from other peers)
		if item.Signature != "" || item.PublicKey != "" {
			return
		}

		// Add my public key
		item.PublicKey = base64.StdEncoding.EncodeToString(signPub)

		// Sign the item data
		data := itemDataForSigning(item)
		sig := encryption.Sign(data, signPriv)
		item.Signature = base64.StdEncoding.EncodeToString(sig)
	})
}

// VerifySignatures returns a SyncInFilter that verifies item signatures on receive.
// Items with invalid/missing signatures are rejected.
func VerifySignatures(peerSignKeys map[string]ed25519.PublicKey) core.Option {
	return core.WithSyncInFilter(func(item core.SyncStoreItem, peerID string) bool {
		// If no signature, reject
		if item.Signature == "" || item.PublicKey == "" {
			return false
		}

		// Decode signature and pubkey
		sig, err := base64.StdEncoding.DecodeString(item.Signature)
		if err != nil {
			return false
		}
		pubKey, err := base64.StdEncoding.DecodeString(item.PublicKey)
		if err != nil {
			return false
		}

		// Verify this pubkey is trusted for this peer
		expectedPubKey, trusted := peerSignKeys[peerID]
		if !trusted {
			return false
		}
		if string(pubKey) != string(expectedPubKey) {
			return false
		}

		// Verify signature
		data := itemDataForSigning(&item)
		if !encryption.Verify(data, sig, pubKey) {
			return false
		}

		return true
	})
}

// AllowUnsigned returns a SyncInFilter that allows unsigned items.
func AllowUnsigned() core.Option {
	return core.WithSyncInFilter(func(item core.SyncStoreItem, peerID string) bool {
		return true
	})
}

// itemDataForSigning returns the data that should be signed for an item.
// This covers all content fields (not the signature itself).
func itemDataForSigning(item *core.SyncStoreItem) []byte {
	data := fmt.Sprintf("%s|%s|%s|%d|%s|%t",
		item.App,
		item.Key,
		item.Value,
		item.Timestamp,
		item.ID,
		item.Deleted,
	)
	return []byte(data)
}

// AccessControl provides a simple ACL plugin.
type AccessControl struct {
	AllowedOut   map[string]map[string]bool // peerID -> set of allowed apps
	BlockedPeers map[string]bool
}

// NewAccessControl creates an access control plugin.
func NewAccessControl() *AccessControl {
	return &AccessControl{
		AllowedOut:   make(map[string]map[string]bool),
		BlockedPeers: make(map[string]bool),
	}
}

// AllowPeer allows a peer to sync.
func (ac *AccessControl) AllowPeer(peerID string) {
	delete(ac.BlockedPeers, peerID)
}

// BlockPeer blocks a peer from syncing.
func (ac *AccessControl) BlockPeer(peerID string) {
	ac.BlockedPeers[peerID] = true
}

// AllowApp allows a peer to receive items from a specific app.
func (ac *AccessControl) AllowApp(peerID, app string) {
	if ac.AllowedOut[peerID] == nil {
		ac.AllowedOut[peerID] = make(map[string]bool)
	}
	ac.AllowedOut[peerID][app] = true
}

// SyncOutFilter returns a filter that enforces access control on outgoing items.
func (ac *AccessControl) SyncOutFilter() core.Option {
	return core.WithSyncOutFilter(func(item core.SyncStoreItem, peerID string) bool {
		if ac.BlockedPeers[peerID] {
			return false
		}
		if allowed, ok := ac.AllowedOut[peerID]; ok && len(allowed) > 0 {
			if !allowed[item.App] {
				return false
			}
		}
		return true
	})
}

// SyncInFilter returns a filter that enforces access control on incoming items.
func (ac *AccessControl) SyncInFilter() core.Option {
	return core.WithSyncInFilter(func(item core.SyncStoreItem, peerID string) bool {
		if ac.BlockedPeers[peerID] {
			return false
		}
		return true
	})
}

// Options returns all core.Options for this access control.
func (ac *AccessControl) Options() []core.Option {
	return []core.Option{
		ac.SyncOutFilter(),
		ac.SyncInFilter(),
	}
}
