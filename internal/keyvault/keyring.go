package keyvault

import (
	"context"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// Keyring is a KEK held in memory, loaded from a file of named AES-256 keys.
// It prints as a summary through fmt, log/slog and encoding, never its keys.
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

// LoadKeyring reads a keyring file: one "<id> <standard base64 of 32 bytes>"
// per line, with blank lines and lines starting with # skipped.
func LoadKeyring(path, activeID string) (*Keyring, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, fmt.Errorf("read keyring: %w", err)
	}
	k, err := ParseKeyring(data, activeID)
	if err != nil {
		return nil, fmt.Errorf("keyring %s:\n%w", path, err)
	}
	return k, nil
}

// ParseKeyring parses the contents of a keyring file. An error names each bad
// line and its reason, and never contains key text.
func ParseKeyring(data []byte, activeID string) (*Keyring, error) {
	k := &Keyring{active: activeID, keys: map[string]cipher.AEAD{}}
	seen := map[string]int{}
	var errs []error
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			errs = append(errs, fmt.Errorf("line %d: want a key ID and a base64 key separated by a space", n))
			continue
		}
		id, text := f[0], f[1]
		// The ID is not printed when it is bad: a line with its fields swapped would print the key.
		if !keyIDPattern.MatchString(id) {
			errs = append(errs, fmt.Errorf("line %d: the key ID must be 1 to 32 of A-Z, a-z, 0-9, '.', '_' and '-'", n))
			continue
		}
		if first, ok := seen[id]; ok {
			errs = append(errs, fmt.Errorf("line %d: key ID %s is also on line %d", n, id, first))
			continue
		}
		seen[id] = n
		key, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: key %s is not standard base64", n, id))
			continue
		}
		if len(key) != DEKSize {
			errs = append(errs, fmt.Errorf("line %d: key %s is %d bytes, want %d", n, id, len(key), DEKSize))
			continue
		}
		aead, err := newAEAD(key)
		if err != nil {
			return nil, err
		}
		k.keys[id] = aead
	}
	switch _, ok := seen[activeID]; {
	case !keyIDPattern.MatchString(activeID):
		errs = append(errs, errors.New("the active key ID must be 1 to 32 of A-Z, a-z, 0-9, '.', '_' and '-'"))
	case !ok:
		errs = append(errs, fmt.Errorf("the active key ID %s is not in the keyring", activeID))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return k, nil
}

// ActiveID returns the ID of the key that wraps new DEKs.
func (k Keyring) ActiveID() string { return k.active }

// Wrap wraps dek with the active key, bound to aad.
func (k Keyring) Wrap(_ context.Context, dek, aad []byte) (string, []byte, error) {
	if len(dek) != DEKSize {
		return "", nil, fmt.Errorf("keyvault: a DEK is %d bytes", DEKSize)
	}
	aead, ok := k.keys[k.active]
	if !ok {
		return "", nil, ErrUnknownKEK
	}
	return k.active, seal(aead, dek, aad), nil
}

// Unwrap returns the DEK that the key keyID wrapped with aad.
func (k Keyring) Unwrap(_ context.Context, keyID string, wrapped, aad []byte) ([]byte, error) {
	aead, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKEK, keyID)
	}
	dek, err := aead.Open(nil, nil, wrapped, aad)
	if err != nil || len(dek) != DEKSize {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// String implements fmt.Stringer.
func (k Keyring) String() string {
	noun := "keys"
	if len(k.keys) == 1 {
		noun = "key"
	}
	return fmt.Sprintf("keyring: %d %s, active %s", len(k.keys), noun, k.active)
}

// Format implements fmt.Formatter, so every verb prints the summary.
func (k Keyring) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, k.String())
}

// LogValue implements slog.LogValuer.
func (k Keyring) LogValue() slog.Value {
	return slog.StringValue(k.String())
}

// MarshalText implements encoding.TextMarshaler.
func (k Keyring) MarshalText() ([]byte, error) {
	return []byte(k.String()), nil
}
