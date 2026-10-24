package keyvault_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/keyvault"
)

func newKey(t testing.TB) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// keyfile is the text of a keyring file with these keys, in order.
func keyfile(ids []string, keys map[string][]byte) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(id + " " + base64.StdEncoding.EncodeToString(keys[id]) + "\n")
	}
	return b.String()
}

func keyring(t testing.TB, active string, ids []string, keys map[string][]byte) *keyvault.Keyring {
	t.Helper()
	k, err := keyvault.ParseKeyring([]byte(keyfile(ids, keys)), active)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return k
}

// fixture is a vault over a keyring with one key, v1.
func fixture(t testing.TB) (*keyvault.Vault, *keyvault.Keyring, map[string][]byte) {
	t.Helper()
	keys := map[string][]byte{"v1": newKey(t)}
	k := keyring(t, "v1", []string{"v1"}, keys)
	return keyvault.New(k), k, keys
}

var (
	aadA = keyvault.AAD(keyvault.PurposeProviderCredential, "tenant-a", "groq")
	aadB = keyvault.AAD(keyvault.PurposeProviderCredential, "tenant-b", "groq")
)

func seal(t testing.TB, v *keyvault.Vault, plaintext string, aad []byte) keyvault.Envelope {
	t.Helper()
	env, err := v.Seal(t.Context(), []byte(plaintext), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return env
}

// requireDecryptError checks that err is ErrDecrypt and says nothing more.
func requireDecryptError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, keyvault.ErrDecrypt) || err.Error() != keyvault.ErrDecrypt.Error() {
		t.Fatalf("err = %v, want exactly %v", err, keyvault.ErrDecrypt)
	}
}

func TestSealThenOpenReturnsThePlaintext(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "gsk_provider_secret", aadA)
	if env.KEKID != "v1" {
		t.Errorf("KEKID = %q, want v1", env.KEKID)
	}
	if bytes.Contains(env.Ciphertext, []byte("gsk_provider_secret")) {
		t.Error("the ciphertext contains the plaintext")
	}
	got, err := v.Open(t.Context(), env, aadA)
	if err != nil || string(got) != "gsk_provider_secret" {
		t.Fatalf("Open = %q, %v; want the plaintext", got, err)
	}
}

func TestOpenRefusesAnotherAAD(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret", aadA)
	_, err := v.Open(t.Context(), env, aadB)
	requireDecryptError(t, err)
}

// The DEK layer is rewrapped for another AAD, as someone holding the KEK
// could do, so only the ciphertext layer can notice.
func TestTheCiphertextIsBoundToItsAAD(t *testing.T) {
	t.Parallel()
	v, k, _ := fixture(t)
	env := seal(t, v, "secret", aadA)
	dek, err := k.Unwrap(t.Context(), env.KEKID, env.WrappedDEK, aadA)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	id, wrapped, err := k.Wrap(t.Context(), dek, aadB)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	_, err = v.Open(t.Context(), keyvault.Envelope{KEKID: id, WrappedDEK: wrapped, Ciphertext: env.Ciphertext}, aadB)
	requireDecryptError(t, err)
}

func TestTheWrappedDEKIsBoundToItsAAD(t *testing.T) {
	t.Parallel()
	_, k, _ := fixture(t)
	dek := newKey(t)
	id, wrapped, err := k.Wrap(t.Context(), dek, aadA)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	_, err = k.Unwrap(t.Context(), id, wrapped, aadB)
	requireDecryptError(t, err)
	got, err := k.Unwrap(t.Context(), id, wrapped, aadA)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("Unwrap with the right AAD = %x, %v; want the DEK", got, err)
	}
}

func TestOpenRefusesASwappedWrappedDEK(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret one", aadA)
	for name, other := range map[string]keyvault.Envelope{
		"from another AAD":       seal(t, v, "secret one", aadB),
		"from another plaintext": seal(t, v, "secret two", aadA),
	} {
		t.Run(name, func(t *testing.T) {
			swapped := env
			swapped.WrappedDEK = other.WrappedDEK
			_, err := v.Open(t.Context(), swapped, aadA)
			requireDecryptError(t, err)
		})
	}
}

func TestOpenRefusesASwappedCiphertext(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret one", aadA)
	swapped := env
	swapped.Ciphertext = seal(t, v, "secret two", aadA).Ciphertext
	_, err := v.Open(t.Context(), swapped, aadA)
	requireDecryptError(t, err)
}

func TestOpenRefusesAChangedByte(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret", aadA)
	for name, field := range map[string][]byte{"wrapped DEK": env.WrappedDEK, "ciphertext": env.Ciphertext} {
		t.Run(name, func(t *testing.T) {
			for _, i := range []int{0, len(field) / 2, len(field) - 1} {
				changed := keyvault.Envelope{KEKID: env.KEKID, WrappedDEK: bytes.Clone(env.WrappedDEK), Ciphertext: bytes.Clone(env.Ciphertext)}
				target := changed.Ciphertext
				if name == "wrapped DEK" {
					target = changed.WrappedDEK
				}
				target[i] ^= 1
				_, err := v.Open(t.Context(), changed, aadA)
				requireDecryptError(t, err)
			}
		})
	}
	_, err := v.Open(t.Context(), keyvault.Envelope{KEKID: env.KEKID, WrappedDEK: env.WrappedDEK[:10], Ciphertext: env.Ciphertext}, aadA)
	requireDecryptError(t, err)
}

func TestAnUnknownKEKIDIsNotTheActiveKey(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret", aadA)
	env.KEKID = "v9"
	_, err := v.Open(t.Context(), env, aadA)
	if !errors.Is(err, keyvault.ErrUnknownKEK) || errors.Is(err, keyvault.ErrDecrypt) {
		t.Fatalf("err = %v, want ErrUnknownKEK", err)
	}
	if !strings.Contains(err.Error(), `"v9"`) {
		t.Errorf("error %q does not name the key ID", err)
	}
}

func TestUnwrapRefusesADEKThatIsNot32Bytes(t *testing.T) {
	t.Parallel()
	_, k, keys := fixture(t)
	block, err := aes.NewCipher(keys["v1"])
	if err != nil {
		t.Fatal(err)
	}
	kek, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(n int) []byte {
		return kek.Seal(nil, nil, make([]byte, n), aadA) //nolint:gosec // G407: the nonce argument is empty, not hardcoded
	}
	for _, n := range []int{16, 31, 33} {
		wrapped := wrap(n)
		_, err := k.Unwrap(t.Context(), "v1", wrapped, aadA)
		requireDecryptError(t, err)
	}
	// The same format with 32 bytes is accepted, so the sizes above are what was refused.
	if _, err := k.Unwrap(t.Context(), "v1", wrap(32), aadA); err != nil {
		t.Fatalf("Unwrap of a 32-byte DEK: %v", err)
	}
}

func TestWrapRefusesADEKThatIsNot32Bytes(t *testing.T) {
	t.Parallel()
	_, k, _ := fixture(t)
	if _, _, err := k.Wrap(t.Context(), make([]byte, 16), aadA); err == nil {
		t.Fatal("Wrap accepted a 16-byte DEK")
	}
}

func TestEverySealHasItsOwnDEK(t *testing.T) {
	t.Parallel()
	v, k, _ := fixture(t)
	a := seal(t, v, "same secret", aadA)
	b := seal(t, v, "same secret", aadA)
	if bytes.Equal(a.WrappedDEK, b.WrappedDEK) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("two seals of the same input gave the same bytes")
	}
	dekA, errA := k.Unwrap(t.Context(), a.KEKID, a.WrappedDEK, aadA)
	dekB, errB := k.Unwrap(t.Context(), b.KEKID, b.WrappedDEK, aadA)
	if errA != nil || errB != nil {
		t.Fatalf("Unwrap: %v, %v", errA, errB)
	}
	if bytes.Equal(dekA, dekB) {
		t.Fatal("two seals used the same DEK")
	}
}

func TestRotation(t *testing.T) {
	t.Parallel()
	keys := map[string][]byte{"v1": newKey(t), "v2": newKey(t)}
	old := keyvault.New(keyring(t, "v1", []string{"v1"}, keys))
	env := seal(t, old, "secret", aadA)

	both := keyvault.New(keyring(t, "v2", []string{"v1", "v2"}, keys))
	if got, err := both.Open(t.Context(), env, aadA); err != nil || string(got) != "secret" {
		t.Fatalf("Open of a v1 envelope after v2 became active = %q, %v", got, err)
	}
	if env := seal(t, both, "new", aadA); env.KEKID != "v2" {
		t.Errorf("a new seal used %q, want the active v2", env.KEKID)
	}
	moved, err := both.Rewrap(t.Context(), env, aadA)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if moved.KEKID != "v2" {
		t.Errorf("rewrapped KEKID = %q, want v2", moved.KEKID)
	}
	if !bytes.Equal(moved.Ciphertext, env.Ciphertext) {
		t.Error("Rewrap changed the ciphertext")
	}

	onlyV2 := keyvault.New(keyring(t, "v2", []string{"v2"}, keys))
	if got, err := onlyV2.Open(t.Context(), moved, aadA); err != nil || string(got) != "secret" {
		t.Fatalf("Open of the rewrapped envelope without v1 = %q, %v", got, err)
	}
	if _, err := onlyV2.Open(t.Context(), env, aadA); !errors.Is(err, keyvault.ErrUnknownKEK) {
		t.Errorf("Open of the v1 envelope without v1: %v, want ErrUnknownKEK", err)
	}
}

func TestRewrapLeavesAnEnvelopeOnTheActiveKeyAsItIs(t *testing.T) {
	t.Parallel()
	v, _, _ := fixture(t)
	env := seal(t, v, "secret", aadA)
	got, err := v.Rewrap(t.Context(), env, aadA)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if got.KEKID != env.KEKID || !bytes.Equal(got.WrappedDEK, env.WrappedDEK) || !bytes.Equal(got.Ciphertext, env.Ciphertext) {
		t.Error("Rewrap changed an envelope that already uses the active key")
	}
}

func TestRewrapRefusesAnotherAAD(t *testing.T) {
	t.Parallel()
	keys := map[string][]byte{"v1": newKey(t), "v2": newKey(t)}
	env := seal(t, keyvault.New(keyring(t, "v1", []string{"v1"}, keys)), "secret", aadA)
	_, err := keyvault.New(keyring(t, "v2", []string{"v1", "v2"}, keys)).Rewrap(t.Context(), env, aadB)
	requireDecryptError(t, err)
}

func TestAADBytes(t *testing.T) {
	t.Parallel()
	got := keyvault.AAD(keyvault.PurposeProviderCredential, "t1", "groq")
	want := "\x00\x00\x00\x22llm-gateway/provider-credential/v1" + "\x00\x00\x00\x02t1" + "\x00\x00\x00\x04groq"
	if string(got) != want {
		t.Fatalf("AAD = %q, want %q", got, want)
	}
}

func TestAADKeepsFieldsApart(t *testing.T) {
	t.Parallel()
	pairs := [][2][]byte{
		{keyvault.AAD("p", "a|b", "c"), keyvault.AAD("p", "a", "b|c")},
		{keyvault.AAD("p", "ab", ""), keyvault.AAD("p", "a", "b")},
		{keyvault.AAD("p", "a"), keyvault.AAD("p", "a", "")},
		{keyvault.AAD("pa", "b"), keyvault.AAD("p", "ab")},
	}
	for _, p := range pairs {
		if bytes.Equal(p[0], p[1]) {
			t.Errorf("two different inputs gave the AAD %q", p[0])
		}
	}
}

// A provider API key, as (e) will store one, with its real AAD.
var (
	benchPlaintext = []byte("gsk_" + strings.Repeat("x", 52))
	benchAAD       = keyvault.AAD(keyvault.PurposeProviderCredential, "0b6d4c52-55d1-4b8e-9f0a-3c2f1e7d9a10", "groq")
)

func BenchmarkSeal(b *testing.B) {
	v, _, _ := fixture(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := v.Seal(b.Context(), benchPlaintext, benchAAD); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpen(b *testing.B) {
	v, _, _ := fixture(b)
	env := seal(b, v, string(benchPlaintext), benchAAD)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := v.Open(b.Context(), env, benchAAD); err != nil {
			b.Fatal(err)
		}
	}
}
