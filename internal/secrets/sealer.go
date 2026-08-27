package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// sealVersion prefixes every ciphertext so the format can evolve (for example
// to add key rotation) without ambiguity about how to decrypt existing rows.
const sealVersion byte = 1

// ErrDecrypt is returned when a ciphertext fails authentication. The error is
// deliberately opaque: distinguishing "wrong key" from "tampered data" would
// hand an attacker an oracle.
var ErrDecrypt = errors.New("secrets: could not decrypt value")

// Sealer provides authenticated encryption for values stored at rest.
//
// It is an interface so that services can be tested with a fake and so that a
// KMS-backed implementation can be substituted without touching call sites.
type Sealer interface {
	// Seal encrypts plaintext, binding it to the given associated data.
	Seal(associatedData string, plaintext []byte) ([]byte, error)
	// Open decrypts a ciphertext produced by Seal with identical associated
	// data.
	Open(associatedData string, ciphertext []byte) ([]byte, error)
}

type aesSealer struct{ aead cipher.AEAD }

// NewSealer returns an AES-256-GCM sealer bound to the keyring's encryption
// subkey.
func (k *Keyring) NewSealer() (Sealer, error) {
	block, err := aes.NewCipher(k.secretBoxKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	return &aesSealer{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce.
//
// associatedData is authenticated but not encrypted; MCPaw passes
// "instanceID|secretName" so a ciphertext copied into a different row fails to
// decrypt rather than silently granting one instance another's credential.
func (s *aesSealer) Seal(associatedData string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, nonce...)
	return s.aead.Seal(out, nonce, plaintext, []byte(associatedData)), nil
}

// Open authenticates and decrypts a sealed value.
func (s *aesSealer) Open(associatedData string, ciphertext []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(ciphertext) < 1+ns+s.aead.Overhead() {
		return nil, ErrDecrypt
	}
	if ciphertext[0] != sealVersion {
		return nil, fmt.Errorf("%w: unsupported ciphertext version %d", ErrDecrypt, ciphertext[0])
	}
	nonce := ciphertext[1 : 1+ns]
	pt, err := s.aead.Open(nil, nonce, ciphertext[1+ns:], []byte(associatedData))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// SecretAAD builds the associated data that binds a ciphertext to one secret of
// one instance.
func SecretAAD(instanceID, name string) string { return "instance:" + instanceID + "|secret:" + name }
