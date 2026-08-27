// Package id generates unguessable identifiers.
//
// Identifiers are 128 bits of crypto/rand rendered as lowercase base32 without
// padding, which keeps them URL-safe, case-insensitive and free of the
// ambiguity that hex or base64 introduce when a human retypes them.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// New returns a new random identifier, optionally prefixed for readability in
// logs and URLs (e.g. New("inst") -> "inst_k3v…").
func New(prefix string) string {
	var b [16]byte
	// crypto/rand.Read is documented never to fail on supported platforms; a
	// failure here means the system CSPRNG is broken and continuing would
	// produce predictable identifiers, so we refuse to continue.
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("id: system CSPRNG unavailable: %v", err))
	}
	s := strings.ToLower(encoding.EncodeToString(b[:]))
	if prefix == "" {
		return s
	}
	return prefix + "_" + s
}
