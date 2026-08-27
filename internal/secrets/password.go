package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters follow the OWASP Password Storage Cheat Sheet
// recommendation (19 MiB memory, 2 iterations, 1 degree of parallelism), which
// balances GPU resistance against the modest RAM budget of a small container.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// Password policy. Length is the control that actually matters; composition
// rules mostly push users toward predictable substitutions, so we require
// length and reject the obviously weak rather than mandating character classes.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

// ErrWeakPassword indicates the supplied password fails policy.
var ErrWeakPassword = errors.New("password does not meet policy")

// ErrBadCredentials is returned for any authentication failure. It is
// intentionally identical whether the user is unknown or the password is wrong.
var ErrBadCredentials = errors.New("invalid credentials")

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrWeakPassword, MinPasswordLength)
	}
	if len(pw) > MaxPasswordLength {
		return fmt.Errorf("%w: must be at most %d bytes", ErrWeakPassword, MaxPasswordLength)
	}
	if strings.TrimSpace(pw) == "" {
		return fmt.Errorf("%w: must not be only whitespace", ErrWeakPassword)
	}
	return nil
}

// HashPassword derives an Argon2id hash and returns it in the standard PHC
// string format, which embeds the parameters so stored hashes stay verifiable
// after the cost parameters are raised.
func HashPassword(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secrets: password salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks a password against a PHC-encoded Argon2id hash in
// constant time with respect to the digest contents.
func VerifyPassword(encoded, pw string) error {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return ErrBadCredentials
	}
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrBadCredentials
	}
	return nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parsePHC(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errors.New("secrets: malformed password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, errors.New("secrets: unsupported argon2 version")
	}
	var p argonParams
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return argonParams{}, nil, nil, errors.New("secrets: malformed argon2 parameters")
	}
	for _, item := range kv {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			return argonParams{}, nil, nil, errors.New("secrets: malformed argon2 parameter")
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return argonParams{}, nil, nil, errors.New("secrets: malformed argon2 parameter value")
		}
		switch name {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.time = uint32(n)
		case "p":
			if n == 0 || n > 255 {
				return argonParams{}, nil, nil, errors.New("secrets: argon2 parallelism out of range")
			}
			p.threads = uint8(n)
		default:
			return argonParams{}, nil, nil, errors.New("secrets: unknown argon2 parameter")
		}
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return argonParams{}, nil, nil, errors.New("secrets: incomplete argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, errors.New("secrets: malformed salt")
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) == 0 {
		return argonParams{}, nil, nil, errors.New("secrets: malformed digest")
	}
	return p, salt, hash, nil
}
