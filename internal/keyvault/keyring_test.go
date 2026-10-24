package keyvault_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/keyvault"
)

func b64(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseKeyringSkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	data := "# rotated 2026-10\n\n  v1 " + b64(32) + "\r\n   \n\t# v0 retired\nv2\t" + b64(32) + "\n"
	k, err := keyvault.ParseKeyring([]byte(data), "v2")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	if k.ActiveID() != "v2" || k.String() != "keyring: 2 keys, active v2" {
		t.Errorf("keyring = %s, active %s; want 2 keys, active v2", k, k.ActiveID())
	}
}

func TestParseKeyringErrorsNameTheLineAndNeverTheKey(t *testing.T) {
	t.Parallel()
	good := b64(32)
	cases := []struct {
		name, data, active, want string
		secret                   string // text that must not be in the error
	}{
		{"31-byte key", "v1 " + b64(31), "v1", "line 1: key v1 is 31 bytes, want 32", b64(31)},
		{"33-byte key", "# c\nv1 " + b64(33), "v1", "line 2: key v1 is 33 bytes, want 32", b64(33)},
		{"not base64", "v1 " + good[:20] + "*" + good[21:], "v1", "line 1: key v1 is not standard base64", good[:20]},
		{"URL-safe base64", "v1 " + base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)), "v1", "line 1: key v1 is not standard base64",
			base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))},
		{"bad ID", "v/1 " + good, "v1", "line 1: the key ID must be 1 to 32 of", good},
		{"ID too long", strings.Repeat("a", 33) + " " + good, "v1", "line 1: the key ID must be 1 to 32 of", good},
		{"fields swapped", good + " v1", "v1", "line 1: the key ID must be 1 to 32 of", good[:30]},
		{"key only", good, "v1", "line 1: want a key ID and a base64 key separated by a space", good},
		{"three fields", "v1 " + good + " extra", "v1", "line 1: want a key ID and a base64 key separated by a space", good},
		{"duplicate ID", "v1 " + good + "\nv1 " + b64(32), "v1", "line 2: key ID v1 is also on line 1", good},
		{"active ID missing", "v1 " + good, "v2", "the active key ID v2 is not in the keyring", good},
		{"active ID empty", "v1 " + good, "", "the active key ID must be 1 to 32 of", good},
		{"empty file", "", "v1", "the active key ID v1 is not in the keyring", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k, err := keyvault.ParseKeyring([]byte(tc.data), tc.active)
			if err == nil {
				t.Fatalf("ParseKeyring accepted %q: %v", tc.data, k)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error %q contains key text", err)
			}
		})
	}
}

func TestParseKeyringReportsEveryBadLine(t *testing.T) {
	t.Parallel()
	_, err := keyvault.ParseKeyring([]byte("v1 "+b64(31)+"\nv2 "+b64(32)+"\nv3 "+b64(16)), "v2")
	if err == nil {
		t.Fatal("ParseKeyring accepted two bad keys")
	}
	for _, want := range []string{"line 1: key v1 is 31 bytes", "line 3: key v3 is 16 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadKeyringReadsTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kek")
	if err := os.WriteFile(path, []byte("v1 "+b64(32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := keyvault.LoadKeyring(path, "v1")
	if err != nil || k.ActiveID() != "v1" {
		t.Fatalf("LoadKeyring = %v, %v; want active v1", k, err)
	}

	if err := os.WriteFile(path, []byte("v1 "+b64(31)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := keyvault.LoadKeyring(path, "v1"); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("LoadKeyring of a bad file: %v, want the path and the line", err)
	}
	if _, err := keyvault.LoadKeyring(filepath.Join(t.TempDir(), "missing"), "v1"); err == nil {
		t.Error("LoadKeyring of a missing file succeeded")
	}
}

func TestKeyringPrintsOnlyASummary(t *testing.T) {
	t.Parallel()
	text := "v1 " + b64(32) + "\nv2 " + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 32)) + "\n"
	k, err := keyvault.ParseKeyring([]byte(text), "v2")
	if err != nil {
		t.Fatal(err)
	}
	const want = "keyring: 2 keys, active v2"
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%d", "%x", "%q"} {
		if got := fmt.Sprintf(verb, k); got != want {
			t.Errorf("%s printed %q, want %q", verb, got, want)
		}
		if got := fmt.Sprintf(verb, *k); got != want {
			t.Errorf("%s of the value printed %q, want %q", verb, got, want)
		}
	}
	holder := struct{ KEK *keyvault.Keyring }{k}
	if got := fmt.Sprintf("%+v", holder); got != "{KEK:"+want+"}" {
		t.Errorf("%%+v of a struct holding the keyring = %q", got)
	}

	logged := func(h func(*bytes.Buffer) slog.Handler, v any) string {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("kek ready", "kek", v)
		return buf.String()
	}
	jsonH := func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }
	textH := func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }
	marshalled, err := json.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ out, want string }{
		"slog JSON":              {logged(jsonH, k), `"kek":"` + want + `"`},
		"slog JSON, holder":      {logged(jsonH, holder), `"kek":{"KEK":"` + want + `"}`},
		"slog text":              {logged(textH, k), `kek="` + want + `"`},
		"json.Marshal of holder": {string(marshalled), `{"KEK":"` + want + `"}`},
		"slog text, value":       {logged(textH, *k), `kek="` + want + `"`},
		"slog JSON, single key":  {logged(jsonH, keyring(t, "v1", []string{"v1"}, map[string][]byte{"v1": newKey(t)})), `"kek":"keyring: 1 key, active v1"`},
	} {
		if !strings.Contains(tc.out, tc.want) {
			t.Errorf("%s: %s does not contain %s", name, tc.out, tc.want)
		}
	}
}
