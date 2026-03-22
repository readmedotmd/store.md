package encrypted

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/helpers/encryption"
	"github.com/readmedotmd/store.md/sync/core"
)

// Example demonstrates batch encryption with signatures.
// The entire payload is encrypted as a single batch for efficiency.
func Example() {
	// Alice and Bob each set up their encrypted sync

	// Generate keys for Alice
	aliceBox, _ := encryption.GenerateBoxKeys()
	aliceSignPriv, aliceSignPub, _ := encryption.GenerateSigningKeys()

	// Generate keys for Bob
	bobBox, _ := encryption.GenerateBoxKeys()
	bobSignPriv, bobSignPub, _ := encryption.GenerateSigningKeys()

	// ============================================
	// ALICE SETUP
	// ============================================

	aliceLocal := memory.New()
	aliceSync := core.New(aliceLocal)

	// Create encrypted sync with batch encryption
	aliceEnc := New(Config{
		Inner:        aliceSync,
		MyKeys:       aliceBox,
		SignPriv:     aliceSignPriv,
		SignPub:      aliceSignPub,
		PeerBoxKeys: map[string][32]byte{
			"bob": bobBox.PublicKey,
		},
		PeerSignKeys: map[string]ed25519.PublicKey{
			"bob": bobSignPub,
		},
	})

	fmt.Println("✓ Alice setup complete")

	// ============================================
	// BOB SETUP
	// ============================================

	bobLocal := memory.New()
	bobSync := core.New(bobLocal)

	bobEnc := New(Config{
		Inner:        bobSync,
		MyKeys:       bobBox,
		SignPriv:     bobSignPriv,
		SignPub:      bobSignPub,
		PeerBoxKeys: map[string][32]byte{
			"alice": aliceBox.PublicKey,
		},
		PeerSignKeys: map[string]ed25519.PublicKey{
			"alice": aliceSignPub,
		},
	})

	fmt.Println("✓ Bob setup complete")

	// ============================================
	// ALICE WRITES SOME DATA
	// ============================================

	ctx := context.Background()

	// Alice stores some items locally
	aliceEnc.SetItem(ctx, "chat", "msg/1", "Hello from Alice!")
	aliceEnc.SetItem(ctx, "chat", "msg/2", "How are you?")
	aliceEnc.SetItem(ctx, "chat", "msg/3", "This is a batch!")

	fmt.Println("✓ Alice stored 3 items locally")

	// ============================================
	// ALICE SYNCS TO BOB (INITIATE)
	// ============================================

	// Alice initiates sync - gets encrypted batch to send to Bob
	outgoing, err := aliceEnc.Sync(ctx, "bob", nil)
	if err != nil {
		fmt.Printf("Alice sync error: %v\n", err)
		return
	}
	if outgoing != nil && len(outgoing.Items) > 0 {
		fmt.Println("✓ Alice created encrypted batch for Bob")
	}

	// ============================================
	// BOB RECEIVES AND PROCESSES
	// ============================================

	// Bob receives and processes the encrypted batch
	_, err = bobEnc.Sync(ctx, "alice", outgoing)
	if err != nil {
		fmt.Printf("Bob receive error: %v\n", err)
		return
	}

	// Check Bob received the items (use empty prefix to get all)
	bobItems, _ := bobEnc.ListItems(ctx, "", "", 10)
	fmt.Printf("✓ Bob received batch with %d items:\n", len(bobItems))
	for _, item := range bobItems {
		fmt.Printf("  - %s: %s\n", item.Key, item.Value)
	}
	fmt.Println("  (batch encrypted + signed by Alice)")

	// ============================================
	// BOB WRITES REPLY
	// ============================================

	bobEnc.SetItem(ctx, "chat", "msg/4", "Hi Alice, it's Bob!")
	bobEnc.SetItem(ctx, "chat", "msg/5", "All good here!")

	fmt.Println("✓ Bob stored 2 reply items")

	// ============================================
	// BOB SENDS REPLY
	// ============================================

	bobReply, err := bobEnc.Sync(ctx, "alice", nil)
	if err != nil {
		fmt.Printf("Bob sync error: %v\n", err)
		return
	}
	if bobReply != nil && len(bobReply.Items) > 0 {
		fmt.Println("✓ Bob created encrypted reply batch")
	}

	// ============================================
	// ALICE RECEIVES REPLY
	// ============================================

	_, err = aliceEnc.Sync(ctx, "bob", bobReply)
	if err != nil {
		fmt.Printf("Alice receive error: %v\n", err)
		return
	}

	// Check Alice received Bob's messages
	aliceItems, _ := aliceEnc.ListItems(ctx, "", "", 10)
	// Count only Bob's messages (msg/4 and msg/5)
	bobMsgCount := 0
	for _, item := range aliceItems {
		if item.Key == "msg/4" || item.Key == "msg/5" {
			bobMsgCount++
		}
	}
	fmt.Printf("✓ Alice received batch with %d new items from Bob:\n", bobMsgCount)
	for _, item := range aliceItems {
		if item.Key == "msg/4" || item.Key == "msg/5" {
			fmt.Printf("  - %s: %s\n", item.Key, item.Value)
		}
	}
	fmt.Println("  (batch encrypted + signed by Bob)")

	// Output:
	// ✓ Alice setup complete
	// ✓ Bob setup complete
	// ✓ Alice stored 3 items locally
	// ✓ Alice created encrypted batch for Bob
	// ✓ Bob received batch with 3 items:
	//   - msg/1: Hello from Alice!
	//   - msg/2: How are you?
	//   - msg/3: This is a batch!
	//   (batch encrypted + signed by Alice)
	// ✓ Bob stored 2 reply items
	// ✓ Bob created encrypted reply batch
	// ✓ Alice received batch with 2 new items from Bob:
	//   - msg/4: Hi Alice, it's Bob!
	//   - msg/5: All good here!
	//   (batch encrypted + signed by Bob)
}

// Example_optional shows how to use encryption/access control as optional plugins.
func Example_optional() {
	// Generate keys
	myBox, _ := encryption.GenerateBoxKeys()
	mySignPriv, mySignPub, _ := encryption.GenerateSigningKeys()
	_, peerSignPub, _ := encryption.GenerateSigningKeys()

	store := memory.New()

	// Option 1: Encryption only (no signatures)
	ss1 := core.New(store)
	encOnly := New(Config{
		Inner:  ss1,
		MyKeys: myBox,
		PeerBoxKeys: map[string][32]byte{
			"peer1": {}, // their public key
		},
	})
	_ = encOnly
	fmt.Println("✓ Encryption-only mode")

	// Option 2: Signatures only (no encryption)
	ss2 := core.NewWithOptions(memory.New(),
		SignOnSave(mySignPriv, mySignPub),
		VerifySignatures(map[string]ed25519.PublicKey{
			"peer1": peerSignPub,
		}),
	)
	_ = ss2
	fmt.Println("✓ Signatures-only mode")

	// Option 3: Both encryption and signatures
	ss3 := core.NewWithOptions(memory.New(),
		SignOnSave(mySignPriv, mySignPub),
		VerifySignatures(map[string]ed25519.PublicKey{
			"peer1": peerSignPub,
		}),
	)
	both := New(Config{
		Inner:        ss3,
		MyKeys:       myBox,
		SignPriv:     mySignPriv,
		SignPub:      mySignPub,
		PeerBoxKeys:  map[string][32]byte{"peer1": {}},
		PeerSignKeys: map[string]ed25519.PublicKey{"peer1": peerSignPub},
	})
	_ = both
	fmt.Println("✓ Full encryption + signatures mode")

	// Option 4: Passthrough (no encryption, no signatures)
	ss4 := core.New(memory.New())
	passThrough := New(Config{
		Inner: ss4,
		// No keys = passthrough
	})
	_ = passThrough
	fmt.Println("✓ Passthrough mode (no encryption)")

	// Output:
	// ✓ Encryption-only mode
	// ✓ Signatures-only mode
	// ✓ Full encryption + signatures mode
	// ✓ Passthrough mode (no encryption)
}

// Example_accessControl shows access control as an optional plugin.
func Example_accessControl() {
	// Create access control rules
	ac := NewAccessControl()

	// Allow peer1 to receive "chat" app items only
	ac.AllowApp("peer1", "chat")

	// Block peer2 completely
	ac.BlockPeer("peer2")

	// Apply as filters
	store := memory.New()
	ss := core.NewWithOptions(store, ac.Options()...)

	fmt.Println("Access control configured:")
	fmt.Println("  - peer1: can only receive 'chat' app items")
	fmt.Println("  - peer2: blocked completely")
	_ = ss

	// Output:
	// Access control configured:
	//   - peer1: can only receive 'chat' app items
	//   - peer2: blocked completely
}

// Example_batchEncryption explains batch encryption benefits.
func Example_batchEncryption() {
	fmt.Println("Batch Encryption Benefits:")
	fmt.Println("  - Encrypt entire payload as single blob")
	fmt.Println("  - One signature per batch (not per item)")
	fmt.Println("  - Better compression (common headers deduplicated)")
	fmt.Println("  - Less overhead for small items")
	fmt.Println("")
	fmt.Println("Wire format:")
	fmt.Println("  {SenderPubKey, EphemeralPub, Nonce, Ciphertext, Signature}")
	fmt.Println("  where Ciphertext = encrypt(JSON(SyncPayload))")

	// Output:
	// Batch Encryption Benefits:
	//   - Encrypt entire payload as single blob
	//   - One signature per batch (not per item)
	//   - Better compression (common headers deduplicated)
	//   - Less overhead for small items
	//
	// Wire format:
	//   {SenderPubKey, EphemeralPub, Nonce, Ciphertext, Signature}
	//   where Ciphertext = encrypt(JSON(SyncPayload))
}
