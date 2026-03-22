package encrypted

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/helpers/encryption"
	"github.com/readmedotmd/store.md/sync/core"
)

func TestSignOnSave_SignsMyItems(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Generate keys
	signPriv, signPub, err := encryption.GenerateSigningKeys()
	if err != nil {
		t.Fatalf("generate keys: %v", err)
	}

	// Create store with signing hook
	ss := core.NewWithOptions(store, SignOnSave(signPriv, signPub))

	// Save an item
	if err := ss.SetItem(ctx, "test", "key1", "value1"); err != nil {
		t.Fatalf("set item: %v", err)
	}

	// Retrieve item
	item, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}

	// Check that PublicKey and Signature are set
	if item.PublicKey == "" {
		t.Error("PublicKey not set")
	}
	if item.Signature == "" {
		t.Error("Signature not set")
	}

	// Verify the public key matches
	expectedPub := base64.StdEncoding.EncodeToString(signPub)
	if item.PublicKey != expectedPub {
		t.Errorf("PublicKey mismatch: got %s, want %s", item.PublicKey, expectedPub)
	}
}

func TestSignOnSave_DoesNotReSign(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Generate keys
	signPriv, signPub, _ := encryption.GenerateSigningKeys()

	// Create store with signing hook
	ss := core.NewWithOptions(store, SignOnSave(signPriv, signPub))

	// Simulate receiving an item from another peer (already signed)
	otherPriv, otherPub, _ := encryption.GenerateSigningKeys()
	item := core.SyncStoreItem{
		App:       "test",
		Key:       "key1",
		Value:     "from-peer",
		PublicKey: base64.StdEncoding.EncodeToString(otherPub),
	}
	data := itemDataForSigning(&item)
	sig := encryption.Sign(data, otherPriv)
	item.Signature = base64.StdEncoding.EncodeToString(sig)

	// Inject via SyncIn (simulating receive from peer)
	if err := ss.SyncIn(ctx, "peer1", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Retrieve item
	retrieved, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}

	// Should keep original signature, not be re-signed with my key
	if retrieved.Signature != item.Signature {
		t.Error("Item was re-signed - should keep original signature")
	}
	if retrieved.PublicKey != item.PublicKey {
		t.Error("PublicKey was changed - should keep original")
	}
}

func TestVerifySignatures_AcceptsValid(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Generate keys for peer
	peerPriv, peerPub, _ := encryption.GenerateSigningKeys()

	// Create store that requires signatures
	ss := core.NewWithOptions(store,
		VerifySignatures(map[string]ed25519.PublicKey{
			"peer1": peerPub,
		}),
	)

	// Create a properly signed item
	item := core.SyncStoreItem{
		App:   "test",
		Key:   "key1",
		Value: "signed-value",
		PublicKey: base64.StdEncoding.EncodeToString(peerPub),
	}
	data := itemDataForSigning(&item)
	sig := encryption.Sign(data, peerPriv)
	item.Signature = base64.StdEncoding.EncodeToString(sig)

	// Should accept valid signature
	if err := ss.SyncIn(ctx, "peer1", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Item should be stored
	_, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Errorf("item not stored: %v", err)
	}
}

func TestVerifySignatures_RejectsInvalid(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Generate peer's keys
	peerPriv, peerPub, _ := encryption.GenerateSigningKeys()

	// Create store that requires signatures
	ss := core.NewWithOptions(store,
		VerifySignatures(map[string]ed25519.PublicKey{
			"peer1": peerPub,
		}),
	)

	// Create item with tampered data after signing
	item := core.SyncStoreItem{
		App:   "test",
		Key:   "key2",
		Value: "original-value",
		PublicKey: base64.StdEncoding.EncodeToString(peerPub),
	}
	data := itemDataForSigning(&item)
	sig := encryption.Sign(data, peerPriv)
	item.Signature = base64.StdEncoding.EncodeToString(sig)
	item.Value = "tampered-value" // Tamper after signing

	// Should reject tampered item
	if err := ss.SyncIn(ctx, "peer1", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Item should NOT be stored (signature invalid due to tampering)
	_, err := ss.GetItem(ctx, "key2")
	if err == nil {
		t.Error("tampered item was stored - should have been rejected")
	}
}

func TestVerifySignatures_RejectsMissing(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Generate keys
	_, peerPub, _ := encryption.GenerateSigningKeys()

	// Create store that requires signatures
	ss := core.NewWithOptions(store,
		VerifySignatures(map[string]ed25519.PublicKey{
			"peer1": peerPub,
		}),
	)

	// Create unsigned item
	item := core.SyncStoreItem{
		App:   "test",
		Key:   "key1",
		Value: "unsigned-value",
	}

	// Should reject (no signature)
	if err := ss.SyncIn(ctx, "peer1", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Item should NOT be stored
	_, err := ss.GetItem(ctx, "key1")
	if err == nil {
		t.Error("unsigned item was stored - should have been rejected")
	}
}

func TestAllowUnsigned_AcceptsAll(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Create store that allows unsigned
	ss := core.NewWithOptions(store, AllowUnsigned())

	// Create unsigned item
	item := core.SyncStoreItem{
		App:   "test",
		Key:   "key1",
		Value: "unsigned-value",
	}

	// Should accept
	if err := ss.SyncIn(ctx, "peer1", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Item should be stored
	_, err := ss.GetItem(ctx, "key1")
	if err != nil {
		t.Errorf("item not stored: %v", err)
	}
}

func TestAccessControl_BlockPeer(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Create access control
	ac := NewAccessControl()
	ac.BlockPeer("bad-peer")

	ss := core.NewWithOptions(store, ac.Options()...)

	// Try to send item from blocked peer
	item := core.SyncStoreItem{
		App:   "test",
		Key:   "key1",
		Value: "blocked",
	}

	// Should be blocked
	if err := ss.SyncIn(ctx, "bad-peer", core.SyncPayload{Items: []core.SyncStoreItem{item}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	// Item should NOT be stored
	_, err := ss.GetItem(ctx, "key1")
	if err == nil {
		t.Error("item from blocked peer was stored")
	}

	// Item from allowed peer should work
	item2 := core.SyncStoreItem{
		App:   "test",
		Key:   "key2",
		Value: "allowed",
	}
	if err := ss.SyncIn(ctx, "good-peer", core.SyncPayload{Items: []core.SyncStoreItem{item2}}); err != nil {
		t.Fatalf("sync in: %v", err)
	}

	_, err = ss.GetItem(ctx, "key2")
	if err != nil {
		t.Error("item from allowed peer was not stored")
	}
}

func TestAccessControl_AllowApp(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	// Create access control - peer1 can only receive "chat" app
	ac := NewAccessControl()
	ac.AllowApp("peer1", "chat")

	ss := core.NewWithOptions(store, ac.Options()...)

	// Add items to sync
	ss.SetItem(ctx, "chat", "msg1", "hello")
	ss.SetItem(ctx, "files", "doc1", "content")

	// Sync out to peer1 - should only get "chat" items
	payload, err := ss.SyncOut(ctx, "peer1", 100)
	if err != nil {
		t.Fatalf("sync out: %v", err)
	}

	// Should only have chat item
	for _, item := range payload.Items {
		if item.App != "chat" {
			t.Errorf("got item from app %q, expected only 'chat'", item.App)
		}
	}
}

func TestEndToEnd_SignAndVerify(t *testing.T) {
	ctx := context.Background()

	// Generate keys for Alice and Bob
	alicePriv, alicePub, _ := encryption.GenerateSigningKeys()
	bobPriv, bobPub, _ := encryption.GenerateSigningKeys()

	// Alice's setup - signs her items, verifies Bob's
	aliceStore := memory.New()
	alice := core.NewWithOptions(aliceStore,
		SignOnSave(alicePriv, alicePub),
		VerifySignatures(map[string]ed25519.PublicKey{
			"bob": bobPub,
		}),
	)

	// Bob's setup - signs his items, verifies Alice's
	bobStore := memory.New()
	bob := core.NewWithOptions(bobStore,
		SignOnSave(bobPriv, bobPub),
		VerifySignatures(map[string]ed25519.PublicKey{
			"alice": alicePub,
		}),
	)

	// Alice saves an item
	if err := alice.SetItem(ctx, "chat", "msg1", "Hello from Alice!"); err != nil {
		t.Fatalf("alice set: %v", err)
	}

	// Alice syncs to Bob
	outgoing, err := alice.Sync(ctx, "bob", nil)
	if err != nil {
		t.Fatalf("alice sync: %v", err)
	}

	// Bob receives
	if _, err := bob.Sync(ctx, "alice", outgoing); err != nil {
		t.Fatalf("bob receive: %v", err)
	}

	// Bob should have the item
	item, err := bob.GetItem(ctx, "msg1")
	if err != nil {
		t.Fatalf("bob get item: %v", err)
	}
	if item.Value != "Hello from Alice!" {
		t.Errorf("wrong value: %s", item.Value)
	}

	// Verify signature is from Alice
	if item.PublicKey != base64.StdEncoding.EncodeToString(alicePub) {
		t.Error("signature not from alice")
	}
}

func TestEndToEnd_RejectTampered(t *testing.T) {
	ctx := context.Background()

	// Setup Alice
	aliceStore := memory.New()
	alicePriv, alicePub, _ := encryption.GenerateSigningKeys()
	alice := core.NewWithOptions(aliceStore, SignOnSave(alicePriv, alicePub))

	// Setup Bob (requires signatures from Alice)
	bobStore := memory.New()
	bob := core.NewWithOptions(bobStore,
		VerifySignatures(map[string]ed25519.PublicKey{
			"alice": alicePub,
		}),
	)

	// Alice saves item
	alice.SetItem(ctx, "chat", "msg1", "Original")

	// Get the outgoing payload
	outgoing, _ := alice.Sync(ctx, "bob", nil)

	// Tamper with the item in transit
	if len(outgoing.Items) > 0 {
		outgoing.Items[0].Value = "Tampered!"
	}

	// Bob receives tampered item
	bob.Sync(ctx, "alice", outgoing)

	// Tampered item should be rejected
	_, err := bob.GetItem(ctx, "msg1")
	if err == nil {
		t.Error("tampered item was accepted - should be rejected")
	}
}

func mustGeneratePubKey(t *testing.T) ed25519.PublicKey {
	_, pub, err := encryption.GenerateSigningKeys()
	if err != nil {
		t.Fatalf("generate keys: %v", err)
	}
	return pub
}
