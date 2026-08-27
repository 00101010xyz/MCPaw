package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// TokenPrefix identifies MCPaw bearer tokens in logs and secret scanners.
const TokenPrefix = "mcpaw_"

// tokenEntropyBytes is the raw randomness in a bearer token. 256 bits makes
// online and offline guessing equally hopeless, which is why the stored MAC
// needs no password-hashing cost.
const tokenEntropyBytes = 32

// NewToken mints a bearer token, returning the plaintext (shown to the operator
// exactly once) and a short display prefix used to identify it in listings.
func NewToken() (plaintext, displayPrefix string, err error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("secrets: generating token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	return TokenPrefix + body, body[:8], nil
}

// TokenLookupKey maps a plaintext token to the keyed digest stored in the
// database. Lookup is therefore an indexed equality search rather than a scan,
// which keeps authentication O(1) and free of timing side channels.
func (k *Keyring) TokenLookupKey(plaintext string) string {
	return mac(k.tokenMACKey, strings.TrimPrefix(plaintext, TokenPrefix))
}

// NewSessionID mints a web session identifier, returning the cookie value and
// the keyed digest to store.
func (k *Keyring) NewSessionID() (cookieValue, storedID string, err error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("secrets: generating session id: %w", err)
	}
	cookieValue = base64.RawURLEncoding.EncodeToString(b)
	return cookieValue, k.SessionLookupKey(cookieValue), nil
}

// SessionLookupKey maps a session cookie value to its stored digest.
func (k *Keyring) SessionLookupKey(cookieValue string) string {
	return mac(k.sessionMAC, cookieValue)
}

// NewCSRFToken returns a random CSRF token bound to a session.
func NewCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secrets: generating csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
