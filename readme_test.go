package llmgateway_test

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
)

// docLines returns a doc's lines; each item must start a line, so a mention
// inside another row does not count as documenting it.
func docLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
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

func TestConfigurationDocListsEverySetting(t *testing.T) {
	var keys []string
	record := func(key string) (string, bool) {
		keys = append(keys, key)
		return "", false
	}
	_, _ = config.Load(record) // the missing DATABASE_URL error does not matter here
	if len(keys) == 0 {
		t.Fatal("config.Load asked for no settings")
	}
	lines := docLines(t, "docs/configuration.md")
	for _, key := range keys {
		if !hasLinePrefix(lines, "| `"+key+"` |") {
			t.Errorf("docs/configuration.md has no row for %s", key)
		}
	}
}

func TestAPIDocListsEveryRoute(t *testing.T) {
	routes := httpapi.New(slog.New(slog.DiscardHandler), httpapi.Deps{}).Routes()
	if len(routes) == 0 {
		t.Fatal("httpapi registered no routes")
	}
	lines := docLines(t, "docs/api.md")
	for _, r := range routes {
		if !hasLinePrefix(lines, "| `"+r+"` |") {
			t.Errorf("docs/api.md has no endpoint row for %s", r)
		}
	}
}

func TestFakeProviderDocListsEveryDirective(t *testing.T) {
	lines := docLines(t, "docs/fake-provider.md")
	for _, d := range fakeupstream.Directives() {
		if !hasLinePrefix(lines, "| `"+d+"` |") {
			t.Errorf("docs/fake-provider.md has no fault mode row for %s", d)
		}
	}
}

func TestREADMELayoutListsEveryPackage(t *testing.T) {
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
	paths := layoutPaths(t, docLines(t, "README.md"))
	for dir := range dirs {
		if !paths[dir+"/"] {
			t.Errorf("README.md project layout does not list %s/", dir)
		}
	}
}

// layoutPaths rebuilds each full path from the indent depth of the tree under
// "## Project layout", so an entry nested under the wrong folder does not count.
func layoutPaths(t *testing.T, lines []string) map[string]bool {
	t.Helper()
	start := slices.Index(lines, "## Project layout")
	if start < 0 {
		t.Fatal("README.md has no Project layout section")
	}
	paths := map[string]bool{}
	var stack []string
	inTree := false
	for _, l := range lines[start+1:] {
		switch {
		case !inTree && l == "```text":
			inTree = true
			continue
		case !inTree:
			continue
		case l == "```":
			return paths
		case l == ".":
			continue
		}
		depth := 0
		rest := l
		for {
			r, ok := strings.CutPrefix(rest, "│   ")
			if !ok {
				r, ok = strings.CutPrefix(rest, "    ")
			}
			if !ok {
				break
			}
			rest = r
			depth++
		}
		name, ok := strings.CutPrefix(rest, "├── ")
		if !ok {
			name, ok = strings.CutPrefix(rest, "└── ")
		}
		if !ok || depth > len(stack) {
			t.Fatalf("README.md project layout: malformed tree line %q", l)
		}
		name, _, _ = strings.Cut(name, " ")
		stack = append(stack[:depth], name)
		paths[strings.Join(stack, "")] = true
	}
	t.Fatal("README.md project layout tree is not closed")
	return nil
}
