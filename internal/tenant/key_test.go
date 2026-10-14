package tenant_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newKey(t *testing.T) string {
	t.Helper()
	key, err := tenant.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestNewKeyRoundTrips(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		key := newKey(t)
		if len(key) != tenant.KeyLen || !strings.HasPrefix(key, "gw_") {
			t.Fatalf("key %q: want %d characters starting with gw_", key, tenant.KeyLen)
		}
		if err := tenant.ParseKey(key); err != nil {
			t.Fatalf("ParseKey(%q) = %v", key, err)
		}
		if seen[key] {
			t.Fatalf("NewKey repeated %q", key)
		}
		seen[key] = true
	}
}

func TestEverySingleCharacterChangeFailsTheChecksum(t *testing.T) {
	t.Parallel()
	for range 5 {
		key := newKey(t)
		for i := len("gw_"); i < len(key); i++ {
			for _, c := range []byte(base62) {
				if c == key[i] {
					continue
				}
				changed := key[:i] + string(c) + key[i+1:]
				if err := tenant.ParseKey(changed); !errors.Is(err, tenant.ErrBadChecksum) {
					t.Fatalf("ParseKey with position %d changed to %q = %v, want ErrBadChecksum", i, c, err)
				}
			}
		}
	}
}

func TestParseKeyRejectsTheWrongShape(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	for name, bad := range map[string]string{
		"empty":            "",
		"wrong prefix":     "sk_" + key[3:],
		"no prefix":        key[3:] + "abc",
		"one short":        key[:len(key)-1],
		"one long":         key + "0",
		"non-base62":       key[:10] + "-" + key[11:],
		"non-ASCII":        key[:10] + "é" + key[12:],
		"space inside":     key[:10] + " " + key[11:],
		"uppercase prefix": "GW_" + key[3:],
	} {
		if err := tenant.ParseKey(bad); !errors.Is(err, tenant.ErrMalformedKey) {
			t.Errorf("%s: ParseKey(%q) = %v, want ErrMalformedKey", name, bad, err)
		}
	}
}

func TestKeyHasAFixedLengthWhenTheRandomPartStartsWithZeros(t *testing.T) {
	t.Parallel()
	for _, random := range [][]byte{make([]byte, 32), append(make([]byte, 31), 1), bytes.Repeat([]byte{0xff}, 32)} {
		key, err := tenant.NewKeyFrom(bytes.NewReader(random))
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != tenant.KeyLen {
			t.Errorf("key from %x has %d characters, want %d", random, len(key), tenant.KeyLen)
		}
		if err := tenant.ParseKey(key); err != nil {
			t.Errorf("ParseKey(%q) = %v", key, err)
		}
	}
	if _, err := tenant.NewKeyFrom(bytes.NewReader(make([]byte, 31))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("NewKey with 31 random bytes = %v, want a read error", err)
	}
}

func TestHashKeyAndDisplayPrefix(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	want := sha256.Sum256([]byte(key))
	if !bytes.Equal(tenant.HashKey(key), want[:]) {
		t.Error("HashKey is not the SHA-256 of the full key")
	}
	if got := tenant.DisplayPrefix(key); got != key[3:11] {
		t.Errorf("DisplayPrefix = %q, want %q", got, key[3:11])
	}
}
