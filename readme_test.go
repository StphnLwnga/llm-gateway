package llmgateway_test

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
)

// readmeLines returns the README's lines; each item must start a line, so a
// mention inside another row does not count as documenting it.
func readmeLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return strings.Split(string(b), "\n")
}

func hasLinePrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestREADMEDocumentsEverySetting(t *testing.T) {
	var keys []string
	record := func(key string) (string, bool) {
		keys = append(keys, key)
		return "", false
	}
	_, _ = config.Load(record) // the missing DATABASE_URL error does not matter here
	if len(keys) == 0 {
		t.Fatal("config.Load asked for no settings")
	}
	lines := readmeLines(t)
	for _, key := range keys {
		if !hasLinePrefix(lines, "| `"+key+"` |") {
			t.Errorf("README.md has no configuration row for %s", key)
		}
	}
}

func TestREADMEDocumentsEveryRoute(t *testing.T) {
	routes := httpapi.New(slog.New(slog.DiscardHandler)).Routes()
	if len(routes) == 0 {
		t.Fatal("httpapi registered no routes")
	}
	lines := readmeLines(t)
	for _, r := range routes {
		if !hasLinePrefix(lines, "| `"+r+"` |") {
			t.Errorf("README.md has no endpoint row for %s", r)
		}
	}
}

func TestREADMEDocumentsEveryPackage(t *testing.T) {
	dirs := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && path != filepath.Base(path) {
			dirs[filepath.ToSlash(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("found no Go packages")
	}
	lines := readmeLines(t)
	for dir := range dirs {
		if !hasLinePrefix(lines, dir+"/ ") {
			t.Errorf("README.md project layout does not mention %s/", dir)
		}
	}
}
