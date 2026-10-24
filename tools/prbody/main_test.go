package main

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("."), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func template(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("../.."), templatePath)
	if err != nil {
		t.Fatalf("read the template: %v", err)
	}
	return string(b)
}

func expect(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func lines(l ...string) string { return strings.Join(l, "\n") }

func TestTheModelBodyAndTheTemplatePass(t *testing.T) {
	tmpl := template(t)
	expect(t, problems(readFile(t, "testdata/model.md"), tmpl), nil)
	expect(t, problems(tmpl, tmpl), nil)
}

func TestADriftingBodyIsCaughtLineByLine(t *testing.T) {
	body := lines(
		"## Summary",
		"",
		"- **S03 adds budgets.** The router reads `internal/router/router.go` and the tenant_id column sets the budgetCents value.",
		"- The request starts when the client sends a prompt, and then the gateway picks a provider and the provider sends the reply to the client over one open stream.",
		"- It doesn't change the API in M3 \u2014 nothing else.",
		"- The notes in docs/ and in internal/router/ say how. Light/dark and read/write are prose.",
		"- The RateLimiter type, the DATABASE_URL setting, errors.Is and budgets.tenant change.",
		"- See ADR 0016 and internal/config for the rule.",
		"",
		"## Test plan",
		"",
		"- [x] `go test -race ./...` passes, then `gh pr view` shows it.",
		"",
		"## Checklist",
		"",
		"- [x] `go run ./tools/prbody` passes on this body, and it reads as plain ASD-STE100 with no identifiers in the prose",
		"",
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)",
	)
	expect(t, problems(body, template(t)), []string{
		"line 3: a plan code",
		`line 3: "budgetCents": an identifier in the prose; say what it is`,
		`line 3: "tenant_id": an identifier in the prose; say what it is`,
		"line 4: a sentence of 29 words; the most is 25",
		"line 5: an em or en dash",
		"line 5: a plan code",
		"line 5: a contraction",
		`line 6: "docs/": an identifier in the prose; say what it is`,
		`line 6: "internal/router/": an identifier in the prose; say what it is`,
		`line 7: "RateLimiter": an identifier in the prose; say what it is`,
		`line 7: "DATABASE_URL": an identifier in the prose; say what it is`,
		`line 7: "budgets.tenant": an identifier in the prose; say what it is`,
		`line 7: "errors.Is": an identifier in the prose; say what it is`,
		"line 8: a plan code",
		`line 8: "internal/config": an identifier in the prose; say what it is`,
		"line 12: `go test -race ./...`: a second command or code name; name at most one",
		"line 12: `gh pr view`: a second command or code name; name at most one",
		"line 18: the tool footer or a session link",
	})
}

func TestTheFooterASessionLinkAndATrailerAreProblems(t *testing.T) {
	body := lines(
		"The gateway stays the same.",
		"",
		"\U0001F916 Generated with [Claude Code](https://claude.com/claude-code)",
		"",
		"https://claude.ai/code/session_01AbCdEf",
		"",
		"Co-Authored-By: Claude <noreply@anthropic.com>",
		"claude-session: https://example.com/a",
	)
	expect(t, problems(body, ""), []string{
		"line 3: the tool footer or a session link",
		"line 5: the tool footer or a session link",
		"line 7: a commit trailer; a body has none",
		`line 7: "anthropic.com": an identifier in the prose; say what it is`,
		"line 8: a commit trailer; a body has none",
	})
}

func TestTemplateLinesPassAndTheTextAfterThemIsChecked(t *testing.T) {
	tmpl := template(t)
	line := "- [x] README or docs/ updated in this PR for any change to routes, settings, packages, behavior or measurements."
	expect(t, problems(line+" The design document has the rule.", tmpl), nil)
	expect(t, problems(line+" See docs/adr/0016-observability.md.", tmpl), []string{
		`line 1: "docs/adr/0016-observability.md": an identifier in the prose; say what it is`,
	})
}

func TestTheResultLineIsNumbersAndASecondCodeBlockIsOneTooMany(t *testing.T) {
	result := "- [x] The checks before every commit pass on Go 1.26.10: 401 tests in 18 packages, 0 lint issues, 0 vulnerabilities, and gofmt and vet print nothing."
	expect(t, problems(result, ""), nil)
	expect(t, problems("```\nmake up\n```\n\n```\nrm -rf x\n```", ""), []string{
		"line 5: a second command or code block; name at most one",
	})
}

func TestTildeFencesAndIndentedBlocksAreCodeBlocks(t *testing.T) {
	expect(t, problems("~~~\nmake up\n~~~\n\n```\nrm -rf x\n```", ""), []string{
		"line 5: a second command or code block; name at most one",
	})
	expect(t, problems("Run this:\n\n    make up\n    make logs\n\nThen this:\n\n\tmake down", ""), []string{
		"line 8: a second command or code block; name at most one",
	})
	// Inside a list item, an indented line continues the item.
	expect(t, problems("- The first item.\n\n    It goes on with tenant_id.\n\n```\nmake up\n```", ""), []string{
		`line 3: "tenant_id": an identifier in the prose; say what it is`,
	})
}

func TestMilestoneNumbersArePlanCodes(t *testing.T) {
	expect(t, problems("This finishes M3, and M4 adds the cache.", ""), []string{"line 1: a plan code"})
	expect(t, problems("The gateway uses 3 MB of memory.", ""), nil)
}

func TestAFenceClosesOnARunAsLongAsItsOpeningRun(t *testing.T) {
	expect(t, problems("````\n```\nmake up\n```\n````\n\nIt doesn't change.", ""), []string{
		"line 7: a contraction",
	})
	// A backtick after the opening run makes it prose, so the lines after it
	// are checked.
	expect(t, problems("```x`\nIt doesn't change.", ""), []string{"line 2: a contraction"})
}

func TestACodeBlockThatIsNotClosedIsAProblem(t *testing.T) {
	expect(t, problems("Run this:\n\n```\nmake up\n\nThe tenant_id column in S03 \u2014 doesn't change.", ""), []string{
		"line 3: a code block that is not closed",
	})
	expect(t, problems("~~~\nmake up\n```\nThe tenant_id column.", ""), []string{
		"line 1: a code block that is not closed",
	})
}

func TestASentenceWrappedOverLinesCountsAsOne(t *testing.T) {
	body := lines(
		"- The gateway reads the route from the store and checks its tenant and its budget.",
		"  Then it sends the request to the first provider when the route passes every one",
		"  of the checks that the budget rules give for each tenant on the route.",
		"- A new item starts a new paragraph.",
		"This line continues that item, and it is short.",
	)
	expect(t, problems(body, ""), []string{"line 2: a sentence of 29 words; the most is 25"})
}

func TestQuoteLinesJoinAndTheMarkIsNotAWord(t *testing.T) {
	quote := lines(
		"> The gateway reads the route from the store and checks its tenant and its budget, and then it",
		"> sends the request to the first open provider.",
	)
	expect(t, problems(quote, ""), []string{"line 1: a sentence of 26 words; the most is 25"})
	// Twenty-five words over two quote lines, so the marks would make it 27.
	short := lines(
		"> The gateway reads the route from the store and checks its tenant and its budget, and then it",
		"> sends the request to the first provider.",
	)
	expect(t, problems(short, ""), nil)
	// A quote line after a paragraph starts a new block.
	split := lines(
		"The gateway reads the route from the store and checks its tenant and its budget and then",
		"> sends the request to the first provider that passes every check.",
	)
	expect(t, problems(split, ""), nil)
}

func TestPackagePathsHyphenPathsAndMigrationNamesAreIdentifiers(t *testing.T) {
	body := lines(
		"Let's see. The github.com/StphnLwnga/llm-gateway module and 00003_budgets change docs/benchmarks.",
		"The fake-provider/server path, the light/dark screenshots and read/write access are prose too, except the first.",
	)
	expect(t, problems(body, ""), []string{
		"line 1: a contraction",
		`line 1: "00003_budgets": an identifier in the prose; say what it is`,
		`line 1: "github.com/StphnLwnga/llm-gateway": an identifier in the prose; say what it is`,
		`line 1: "docs/benchmarks": an identifier in the prose; say what it is`,
		`line 2: "fake-provider/server": an identifier in the prose; say what it is`,
	})
}

func TestRunExitsOneOnProblemsAndTwoOnMisuse(t *testing.T) {
	dir := t.TempDir()
	good, bad := dir+"/good.md", dir+"/bad.md"
	if err := os.WriteFile(good, []byte("## Summary\n\nThe gateway stays the same.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("It doesn't \u2014 at all.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir("../..")
	for _, c := range []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{[]string{good}, 0, "PR body: no problems.\n", ""},
		{[]string{bad}, 1, "PR body: 2 problems.\n", "line 1: an em or en dash\nline 1: a contraction\n"},
		{nil, 2, "", "Usage: go run ./tools/prbody <file with the PR body>\n"},
	} {
		code, stdout, stderr := run(c.args)
		if code != c.code || stdout != c.stdout || stderr != c.stderr {
			t.Errorf("run(%q) = %d, %q, %q; want %d, %q, %q", c.args, code, stdout, stderr, c.code, c.stdout, c.stderr)
		}
	}
}
