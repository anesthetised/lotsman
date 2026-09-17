package awgconf

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// Key is a Curve25519 key. Config files use base64, UAPI uses hex.
type Key [32]byte

func GeneratePrivateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, err
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	return k, nil
}

func (k Key) Public() Key {
	var pub Key
	curve25519.ScalarBaseMult((*[32]byte)(&pub), (*[32]byte)(&k))
	return pub
}

func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("key is not base64: %w", err)
	}
	if len(b) != 32 {
		return Key{}, fmt.Errorf("key is %d bytes, want 32", len(b))
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
func (k Key) Hex() string    { return hex.EncodeToString(k[:]) }
func (k Key) IsZero() bool   { return k == Key{} }
