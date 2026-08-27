// Package secrets implements every cryptographic primitive MCPaw relies on:
// the master keyring, authenticated encryption for stored credentials,
// password hashing for administrators, and generation/verification of bearer
// tokens.
//
// Concentrating this in one package means the security-critical code is small,
// reviewable in a single sitting, and impossible to accidentally reimplement
// elsewhere with weaker parameters.
package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// masterKeyLen is the length in bytes of the root key material.
const masterKeyLen = 32

// Purposes for key derivation. Distinct purposes guarantee that a key used for
// one function is cryptographically unrelated to the others, so a weakness in
// one context cannot be pivoted into another.
const (
	purposeSecretBox = "mcpaw/v1/instance-secret-encryption"
	purposeTokenMAC  = "mcpaw/v1/bearer-token-mac"
	purposeSessionID = "mcpaw/v1/session-id-mac"
)

// Keyring owns the master key and derives purpose-specific subkeys from it.
type Keyring struct {
	master []byte

	secretBoxKey []byte
	tokenMACKey  []byte
	sessionMAC   []byte
}

// NewKeyring derives all subkeys from a 32-byte master key.
func NewKeyring(master []byte) (*Keyring, error) {
	if len(master) != masterKeyLen {
		return nil, fmt.Errorf("secrets: master key must be %d bytes, got %d", masterKeyLen, len(master))
	}
	k := &Keyring{master: append([]byte(nil), master...)}
	var err error
	if k.secretBoxKey, err = derive(master, purposeSecretBox); err != nil {
		return nil, err
	}
	if k.tokenMACKey, err = derive(master, purposeTokenMAC); err != nil {
		return nil, err
	}
	if k.sessionMAC, err = derive(master, purposeSessionID); err != nil {
		return nil, err
	}
	return k, nil
}

func derive(master []byte, purpose string) ([]byte, error) {
	r := hkdf.New(sha256.New, master, nil, []byte(purpose))
	out := make([]byte, 32)
	if _, err := r.Read(out); err != nil {
		return nil, fmt.Errorf("secrets: deriving %s: %w", purpose, err)
	}
	return out, nil
}

// LoadOrCreateMasterKey resolves the master key from an explicit base64 value
// or, failing that, from a key file inside dataDir which it creates on first
// run with 0600 permissions.
//
// It returns the key and whether the key was newly generated, so the caller can
// warn operators that they must back it up: losing it makes every stored
// credential unrecoverable.
func LoadOrCreateMasterKey(encoded, dataDir string) (key []byte, generated bool, err error) {
	if encoded != "" {
		k, err := decodeKey(encoded)
		if err != nil {
			return nil, false, fmt.Errorf("secrets: MCPAW_MASTER_KEY: %w", err)
		}
		return k, false, nil
	}

	path := filepath.Join(dataDir, "master.key")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		k, derr := decodeKey(strings.TrimSpace(string(data)))
		if derr != nil {
			return nil, false, fmt.Errorf("secrets: %s: %w", path, derr)
		}
		if err := checkKeyFilePerms(path); err != nil {
			return nil, false, err
		}
		return k, false, nil
	case errors.Is(err, fs.ErrNotExist):
		// fall through to generation
	default:
		return nil, false, fmt.Errorf("secrets: reading %s: %w", path, err)
	}

	k := make([]byte, masterKeyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, false, fmt.Errorf("secrets: generating master key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("secrets: creating data dir: %w", err)
	}
	// O_EXCL closes the race where two processes start simultaneously and both
	// believe they generated the canonical key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreateMasterKey("", dataDir)
		}
		return nil, false, fmt.Errorf("secrets: creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		return nil, false, fmt.Errorf("secrets: writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return nil, false, fmt.Errorf("secrets: syncing %s: %w", path, err)
	}
	return k, true, nil
}

func checkKeyFilePerms(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secrets: stat %s: %w", path, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secrets: %s is group/world accessible (mode %04o); tighten it to 0600", path, fi.Mode().Perm())
	}
	return nil
}

func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == masterKeyLen {
			return b, nil
		}
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == masterKeyLen {
		return b, nil
	}
	return nil, fmt.Errorf("must be a base64- or hex-encoded %d-byte key", masterKeyLen)
}

// GenerateMasterKey returns a fresh base64-encoded master key, used by the
// `mcpaw keygen` command.
func GenerateMasterKey() (string, error) {
	k := make([]byte, masterKeyLen)
	if _, err := rand.Read(k); err != nil {
		return "", fmt.Errorf("secrets: generating master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

// mac computes a keyed digest. Used for token and session lookup keys: the
// stored value is useless to an attacker who dumps the database but does not
// also hold the master key.
func mac(key []byte, value string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
