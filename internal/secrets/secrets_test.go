package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	k, err := NewKeyring(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return k
}

func TestNewKeyringRejectsWrongLength(t *testing.T) {
	if _, err := NewKeyring([]byte("short")); err == nil {
		t.Fatal("expected error for short master key")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := testKeyring(t).NewSealer()
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	aad := SecretAAD("inst_1", "apiKey")
	ct, err := s.Seal(aad, []byte("hunter2-zotero"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	pt, err := s.Open(aad, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(pt) != "hunter2-zotero" {
		t.Fatalf("round trip mismatch: %q", pt)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	s, _ := testKeyring(t).NewSealer()
	a, _ := s.Seal("aad", []byte("same"))
	b, _ := s.Seal("aad", []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

// A ciphertext lifted from one instance's row must not decrypt under another
// instance's associated data. This is the control that stops a database write
// primitive from becoming credential theft across instances.
func TestOpenRejectsForeignAssociatedData(t *testing.T) {
	s, _ := testKeyring(t).NewSealer()
	ct, _ := s.Seal(SecretAAD("inst_a", "apiKey"), []byte("secret"))
	if _, err := s.Open(SecretAAD("inst_b", "apiKey"), ct); err == nil {
		t.Fatal("ciphertext decrypted under a different instance's AAD")
	}
	if _, err := s.Open(SecretAAD("inst_a", "otherKey"), ct); err == nil {
		t.Fatal("ciphertext decrypted under a different secret name")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	s, _ := testKeyring(t).NewSealer()
	ct, _ := s.Seal("aad", []byte("secret"))
	ct[len(ct)-1] ^= 0xff
	if _, err := s.Open("aad", ct); err == nil {
		t.Fatal("tampered ciphertext authenticated successfully")
	}
}

func TestOpenRejectsTruncatedAndUnknownVersion(t *testing.T) {
	s, _ := testKeyring(t).NewSealer()
	if _, err := s.Open("aad", []byte{1, 2, 3}); err == nil {
		t.Fatal("expected failure on truncated ciphertext")
	}
	ct, _ := s.Seal("aad", []byte("secret"))
	ct[0] = 99
	if _, err := s.Open("aad", ct); err == nil {
		t.Fatal("expected failure on unknown ciphertext version")
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	if err := VerifyPassword(hash, "correct horse battery staple"); err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if err := VerifyPassword(hash, "wrong password entirely"); err == nil {
		t.Fatal("verification succeeded with the wrong password")
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("correct horse battery staple")
	b, _ := HashPassword("correct horse battery staple")
	if a == b {
		t.Fatal("identical passwords produced identical hashes (missing salt)")
	}
}

func TestPasswordPolicy(t *testing.T) {
	for _, pw := range []string{"", "short", strings.Repeat(" ", 20)} {
		if err := ValidatePassword(pw); err == nil {
			t.Fatalf("weak password %q accepted", pw)
		}
	}
	if err := ValidatePassword("a-perfectly-fine-password"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{"", "not-a-hash", "$argon2id$v=19$m=1$salt$hash", "$bcrypt$x$y$z$w$v"} {
		if err := VerifyPassword(h, "whatever"); err == nil {
			t.Fatalf("malformed hash %q verified successfully", h)
		}
	}
}

func TestTokenGenerationAndLookup(t *testing.T) {
	k := testKeyring(t)
	tok, prefix, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) {
		t.Fatalf("token missing prefix: %q", tok)
	}
	if len(prefix) != 8 {
		t.Fatalf("unexpected display prefix %q", prefix)
	}
	if k.TokenLookupKey(tok) != k.TokenLookupKey(tok) {
		t.Fatal("lookup key is not deterministic")
	}
	other, _, _ := NewToken()
	if k.TokenLookupKey(tok) == k.TokenLookupKey(other) {
		t.Fatal("distinct tokens collided")
	}
	if strings.Contains(k.TokenLookupKey(tok), strings.TrimPrefix(tok, TokenPrefix)) {
		t.Fatal("stored lookup key contains the plaintext token")
	}
}

// Two keyrings with different master keys must not produce interchangeable
// lookup keys, otherwise tokens would survive a key rotation.
func TestTokenLookupKeyIsKeyBound(t *testing.T) {
	k1 := testKeyring(t)
	k2, _ := NewKeyring(bytes.Repeat([]byte{0x11}, 32))
	tok, _, _ := NewToken()
	if k1.TokenLookupKey(tok) == k2.TokenLookupKey(tok) {
		t.Fatal("lookup key is independent of the master key")
	}
}

func TestSessionIDRoundTrip(t *testing.T) {
	k := testKeyring(t)
	cookie, stored, err := k.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	if cookie == stored {
		t.Fatal("stored session id equals the cookie value")
	}
	if k.SessionLookupKey(cookie) != stored {
		t.Fatal("session lookup key does not match the stored id")
	}
}

func TestLoadOrCreateMasterKeyGeneratesThenReuses(t *testing.T) {
	dir := t.TempDir()
	k1, generated, err := LoadOrCreateMasterKey("", dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !generated {
		t.Fatal("expected the first call to generate a key")
	}
	fi, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode is %04o, want 0600", fi.Mode().Perm())
	}
	k2, generated, err := LoadOrCreateMasterKey("", dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if generated {
		t.Fatal("second call regenerated the key")
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key changed between loads")
	}
}

func TestLoadMasterKeyRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreateMasterKey("", dir); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	path := filepath.Join(dir, "master.key")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, err := LoadOrCreateMasterKey("", dir); err == nil {
		t.Fatal("world-readable key file was accepted")
	}
}

func TestLoadMasterKeyFromEnvValue(t *testing.T) {
	encoded, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	k, generated, err := LoadOrCreateMasterKey(encoded, t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateMasterKey: %v", err)
	}
	if generated || len(k) != 32 {
		t.Fatalf("unexpected result generated=%v len=%d", generated, len(k))
	}
	if _, _, err := LoadOrCreateMasterKey("not-base64!!", t.TempDir()); err == nil {
		t.Fatal("invalid master key accepted")
	}
}
