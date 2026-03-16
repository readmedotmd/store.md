// Package encryption provides NaCl-based cryptographic primitives.
//
// This is a generic encryption helper library, not tied to any specific
// storage or sync implementation.
//
package encryption

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"
)

// BoxKeyPair holds Curve25519 keys for public key encryption.
type BoxKeyPair struct {
	PublicKey  [32]byte
	PrivateKey [32]byte
}

// GenerateBoxKeys creates a new Curve25519 keypair.
func GenerateBoxKeys() (*BoxKeyPair, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate box keys: %w", err)
	}
	return &BoxKeyPair{
		PublicKey:  *pub,
		PrivateKey: *priv,
	}, nil
}

// GenerateSharedSecret derives a shared secret via ECDH.
// Both parties will derive the SAME secret:
//   Alice: GenerateSharedSecret(alicePriv, bobPub)
//   Bob:   GenerateSharedSecret(bobPriv, alicePub)
//   Result: aliceSecret == bobSecret
func GenerateSharedSecret(myPrivKey, peerPubKey *[32]byte) [32]byte {
	var shared [32]byte
	box.Precompute(&shared, peerPubKey, myPrivKey)
	return shared
}

// DeriveKey derives a key from a shared secret using HKDF-like construction.
// Different contexts produce completely different keys.
func DeriveKey(sharedSecret [32]byte, context string) [32]byte {
	h := sha256.New()
	h.Write(sharedSecret[:])
	h.Write([]byte(context))
	h.Write([]byte("store.md encryption v1"))
	
	var key [32]byte
	sum := h.Sum(nil)
	copy(key[:], sum)
	return key
}

// GenerateSigningKeys creates a new Ed25519 keypair for signatures.
func GenerateSigningKeys() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, pub, err
}

// GenerateSecretKey creates a random symmetric key for secretbox.
func GenerateSecretKey() ([32]byte, error) {
	var key [32]byte
	_, err := io.ReadFull(rand.Reader, key[:])
	return key, err
}

// GenerateNonce creates a random nonce for secretbox.
func GenerateNonce() ([24]byte, error) {
	var nonce [24]byte
	_, err := io.ReadFull(rand.Reader, nonce[:])
	return nonce, err
}

// BoxEncrypt encrypts a message using NaCl box.
// Returns: ephemeralPub (32 bytes) + nonce (24 bytes) + ciphertext.
func BoxEncrypt(plaintext []byte, recipientPub, senderPriv *[32]byte) ([]byte, error) {
	// Generate ephemeral keypair
	ephPub, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	
	// Generate nonce
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	
	// Encrypt
	ciphertext := box.Seal(nil, plaintext, &nonce, recipientPub, ephPriv)
	
	// Format: ephemeralPub(32) + nonce(24) + ciphertext
	result := make([]byte, 0, 32+24+len(ciphertext))
	result = append(result, ephPub[:]...)
	result = append(result, nonce[:]...)
	result = append(result, ciphertext...)
	
	return result, nil
}

// BoxDecrypt decrypts a message encrypted with BoxEncrypt.
func BoxDecrypt(data []byte, senderPub, recipientPriv *[32]byte) ([]byte, error) {
	if len(data) < 32+24+box.Overhead {
		return nil, fmt.Errorf("ciphertext too short")
	}
	
	var ephemeralPub [32]byte
	copy(ephemeralPub[:], data[:32])
	
	var nonce [24]byte
	copy(nonce[:], data[32:56])
	
	ciphertext := data[56:]
	
	plaintext, ok := box.Open(nil, ciphertext, &nonce, &ephemeralPub, recipientPriv)
	if !ok {
		return nil, fmt.Errorf("decryption failed")
	}
	
	return plaintext, nil
}

// SecretboxEncrypt encrypts using NaCl secretbox.
func SecretboxEncrypt(plaintext []byte, key *[32]byte) ([]byte, [24]byte, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, nonce, fmt.Errorf("generate nonce: %w", err)
	}
	
	ciphertext := secretbox.Seal(nil, plaintext, &nonce, key)
	return ciphertext, nonce, nil
}

// SecretboxDecrypt decrypts using NaCl secretbox.
func SecretboxDecrypt(ciphertext []byte, nonce *[24]byte, key *[32]byte) ([]byte, error) {
	plaintext, ok := secretbox.Open(nil, ciphertext, nonce, key)
	if !ok {
		return nil, fmt.Errorf("decryption failed")
	}
	return plaintext, nil
}

// Sign signs a message with Ed25519.
func Sign(message []byte, privateKey ed25519.PrivateKey) []byte {
	return ed25519.Sign(privateKey, message)
}

// Verify verifies an Ed25519 signature.
func Verify(message, sig []byte, publicKey ed25519.PublicKey) bool {
	return ed25519.Verify(publicKey, message, sig)
}
