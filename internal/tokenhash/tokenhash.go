// Package tokenhash hashes and checks bearer tokens. It imports nothing from
// vmtool, so a command that must stay libvirt-free (vmtool-store) can use it;
// internal/config re-exports Hash and Check for vmtool's own callers.
package tokenhash

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Hash returns the lowercase hex SHA-256 of token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Valid reports whether s is a SHA-256 in lowercase hex (64 characters).
func Valid(s string) bool { return hashRE.MatchString(s) }

// Check reports whether token hashes to hashHex, in constant time.
func Check(token, hashHex string) bool {
	if token == "" || hashHex == "" {
		return false
	}
	want, err := hex.DecodeString(hashHex)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}
