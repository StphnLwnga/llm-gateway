// Package tenant holds the API key format and the caller's identity.
package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"hash/crc32"
	"io"
	"math/big"
	"strings"
)

const (
	keyPrefix     = "gw_"
	base62        = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	randomBytes   = 32
	randomWidth   = 43 // 62^43 > 2^256
	checksumWidth = 6  // 62^6 > 2^32
	displayWidth  = 8

	// KeyLen is the length of every API key.
	KeyLen = len(keyPrefix) + randomWidth + checksumWidth
)

// Key parse errors. Both mean the key never reaches the database.
var (
	ErrMalformedKey = errors.New("malformed API key")
	ErrBadChecksum  = errors.New("API key checksum mismatch")
)

// NewKey returns a new random API key.
func NewKey() (string, error) {
	return newKey(rand.Reader)
}

func newKey(r io.Reader) (string, error) {
	b := make([]byte, randomBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	random := encode(new(big.Int).SetBytes(b), randomWidth)
	return keyPrefix + random + checksum(random), nil
}

// ParseKey checks the format and the checksum of key.
func ParseKey(key string) error {
	if len(key) != KeyLen || !strings.HasPrefix(key, keyPrefix) {
		return ErrMalformedKey
	}
	body := key[len(keyPrefix):]
	for i := range len(body) {
		if strings.IndexByte(base62, body[i]) < 0 {
			return ErrMalformedKey
		}
	}
	// The checksum covers the text, not the bytes: a one-character change is
	// then a burst of at most 8 bits, which CRC32 always detects.
	if body[randomWidth:] != checksum(body[:randomWidth]) {
		return ErrBadChecksum
	}
	return nil
}

// HashKey returns the SHA-256 of the full key, the only form the database stores.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// DisplayPrefix returns the part of key that is safe to show and store in clear.
func DisplayPrefix(key string) string {
	return key[len(keyPrefix) : len(keyPrefix)+displayWidth]
}

func checksum(random string) string {
	return encode(new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(random)))), checksumWidth)
}

// encode writes n in base62, left-padded with zeros to width.
func encode(n *big.Int, width int) string {
	out := make([]byte, width)
	base := big.NewInt(int64(len(base62)))
	rem := new(big.Int)
	n = new(big.Int).Set(n)
	for i := width - 1; i >= 0; i-- {
		n.QuoRem(n, base, rem)
		out[i] = base62[rem.Int64()]
	}
	return string(out)
}
