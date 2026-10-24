// Command prbody checks a pull request body against the rules in AGENTS.md
// ("Pull requests") before it is posted:
//
//	go run ./tools/prbody <file>
//
// It catches what drifts most. Plain words and one idea per sentence still
// need a reader.
package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	maxWords     = 25
	templatePath = ".github/pull_request_template.md"
)

// Product names that look like code.
var products = map[string]bool{
	"macOS": true, "iOS": true, "GitHub": true, "OpenAI": true, "GenAI": true,
	"OpenTelemetry": true, "PostgreSQL": true, "TraceContext": true, "vLLM": true,
	"JavaScript": true, "TypeScript": true, "KiB": true, "MiB": true, "GiB": true,
}

type rule struct {
	pattern *regexp.Regexp
	problem string
}

var rules = []rule{
	{regexp.MustCompile(`[\x{2014}\x{2013}]`), "an em or en dash"},
	{regexp.MustCompile(`\bS\d{2}[a-z]?\b|\bM\d{1,2}\b|\bADR[ -]?\d{4}\b`), "a plan code"},
	{
		regexp.MustCompile(`(?i)\b\w+n['’]t\b|\b\w+['’](?:re|ll|ve|m|d)\b|\b(?:it|that|there|what|here|who|let|he|she|where|how|when|why)['’]s\b`),
		"a contraction",
	},
	{regexp.MustCompile(`Generated with|claude\.ai/code`), "the tool footer or a session link"},
}

// Code-like words outside backticks: file names, camelCase, PascalCase,
// snake_case, UPPER_SNAKE, schema.table, package.Name and migration names.
var identifiers = []*regexp.Regexp{
	regexp.MustCompile(`\b((?:[\w.-]+/)*[\w-]+\.(?:go|mod|sum|md|sql|ya?ml|json|toml|sh|js|ts|env|proto|txt))\b`),
	regexp.MustCompile(`\b([a-z]+[A-Z]\w*)\b`),
	regexp.MustCompile(`\b([A-Z][a-z0-9]+(?:[A-Z][a-z0-9]*)+)\b`),
	regexp.MustCompile(`\b([a-z][a-z0-9]*_[a-z0-9_]+)\b`),
	regexp.MustCompile(`\b([A-Z][A-Z0-9]*_[A-Z0-9_]*[A-Z0-9])\b`),
	regexp.MustCompile(`\b([a-z_][a-z0-9_]+\.[a-z_][a-z0-9_]+)\b`),
	regexp.MustCompile(`\b([a-z][a-z0-9]*\.[A-Z]\w*)\b`),
	regexp.MustCompile(`\b(\d{5,}_\w+)\b`),
}

// Paths are matched word by word, because RE2 has no lookahead.
var (
	manySlashes = regexp.MustCompile(`^(?:[\w.@-]+/){2,}[\w.@-]*$`)
	folder      = regexp.MustCompile(`^[\w.-]+/$`)
	// One slash is prose ("light/dark") unless a side has a dot, an @ or a
	// hyphen, or it starts in one of the repo's own folders.
	oneSlash  = regexp.MustCompile(`^[\w.@-]+/[\w.@-]*[\w@-]$`)
	topFolder = regexp.MustCompile(`^(?:cmd|internal|docs|deploy|loadtests|tools)/`)
)

var (
	codeSpan   = regexp.MustCompile("`[^`]+`")
	url        = regexp.MustCompile(`https?://\S+`)
	link       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	marker     = regexp.MustCompile(`^#+ |^(?:[-*+]|\d+[.)]) (?:\[ \] )?`)
	quoteMark  = regexp.MustCompile(`^(?:>\s?)+`)
	listItem   = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s`)
	fenceRun   = regexp.MustCompile("^(?:`{3,}|~{3,})")
	sentenceAt = regexp.MustCompile(`[.!?](\s+)[A-Z0-9"]`)
	// A line that starts its own block. Any other line that is not blank
	// continues the paragraph before it, because GitHub joins wrapped lines.
	blockStart = regexp.MustCompile(`^(?:#|\||[-*+]\s|\d+[.)]\s)`)
	// The result of the checks is a list of numbers, not a sentence (AGENTS.md).
	resultLine = regexp.MustCompile(`\bpass on Go \d+\.\d+(?:\.\d+)?:`)
)

func main() {
	code, stdout, stderr := run(os.Args[1:])
	fmt.Fprint(os.Stderr, stderr)
	fmt.Print(stdout)
	os.Exit(code)
}

// run is the whole program, returning the exit code and the two outputs.
func run(args []string) (code int, stdout, stderr string) {
	if len(args) != 1 {
		return 2, "", "Usage: go run ./tools/prbody <file with the PR body>\n"
	}
	//nolint:gosec // G703: reading the file its user names is the whole job.
	body, err := os.ReadFile(args[0])
	if err != nil {
		return 2, "", fmt.Sprintf("prbody: %v\n", err)
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return 2, "", fmt.Sprintf("prbody: %v (run it from the repo root)\n", err)
	}
	list := problems(string(body), string(template))
	for _, p := range list {
		stderr += p + "\n"
	}
	switch len(list) {
	case 0:
		return 0, "PR body: no problems.\n", stderr
	case 1:
		return 1, "PR body: 1 problem.\n", stderr
	}
	return 1, fmt.Sprintf("PR body: %d problems.\n", len(list)), stderr
}

// templateLines returns the template's fixed text, so its own checklist
// wording passes.
func templateLines(template string) []string {
	var fixed []string
	for _, line := range strings.Split(template, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- [ ] ") && len(line) > len("- [ ] ") {
			fixed = append(fixed, line)
		}
	}
	return fixed
}

type part struct {
	line int
	text string
}

// sentences returns the sentences of a paragraph, each with the line it
// starts on.
func sentences(parts []part) []part {
	var text strings.Builder
	type start struct{ line, offset int }
	var starts []start
	for _, p := range parts {
		if p.text == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		starts = append(starts, start{p.line, text.Len()})
		text.WriteString(p.text)
	}
	joined := text.String()
	lineAt := func(offset int) int {
		line := 0
		for _, s := range starts {
			if s.offset <= offset {
				line = s.line
			}
		}
		return line
	}
	var result []part
	from := 0
	for _, m := range sentenceAt.FindAllStringSubmatchIndex(joined, -1) {
		result = append(result, part{lineAt(from), joined[from:m[2]]})
		from = m[3]
	}
	return append(result, part{lineAt(from), joined[from:]})
}

// identifierWords returns the code-like words in prose, in the order found.
func identifierWords(prose string) []string {
	var words []string
	add := func(word string) {
		if word != "" && !slices.Contains(words, word) {
			words = append(words, word)
		}
	}
	for _, pattern := range identifiers {
		for _, m := range pattern.FindAllStringSubmatch(prose, -1) {
			add(m[1])
		}
	}
	for _, field := range strings.Fields(prose) {
		word := strings.TrimRight(strings.TrimLeft(field, `("*`), `,.;:)"*`)
		switch {
		case manySlashes.MatchString(word), folder.MatchString(word):
			add(word)
		case oneSlash.MatchString(word) && (strings.ContainsAny(word, ".@-") || topFolder.MatchString(word)):
			add(word)
		}
	}
	return words
}

func isIndented(raw string) bool {
	return strings.HasPrefix(raw, "    ") || strings.HasPrefix(raw, "\t")
}

type problem struct {
	line int
	text string
}

// problems returns each problem in body, as "line N: ...".
func problems(body, template string) []string {
	fixed := templateLines(template)
	var found []problem
	var paragraphs [][]part
	joins, quoted, inList, indented := false, false, false, false
	code := 0
	fence, fenceLine := "", 0 // the opening run of the open fence
	blank := true

	for index, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		at := index + 1
		report := func(p string) { found = append(found, problem{at, p}) }
		trimmed := strings.TrimSpace(raw)
		afterBlank := blank
		blank = trimmed == ""

		// A fence closes on a run of its own mark at least as long as the run
		// that opened it, with nothing after the run.
		if fence != "" {
			if run := fenceRun.FindString(trimmed); run != "" && run[0] == fence[0] &&
				len(run) >= len(fence) && strings.TrimSpace(trimmed[len(run):]) == "" {
				fence = ""
			}
			continue
		}
		if indented && (blank || isIndented(raw)) {
			continue
		}
		indented = false
		// An indented block after a blank line is code, unless it continues
		// a list item.
		if afterBlank && !inList && !blank && isIndented(raw) {
			indented = true
			if code++; code > 1 {
				report("a second command or code block; name at most one")
			}
			joins, quoted = false, false
			continue
		}
		// A backtick fence has no backtick after its run, or GitHub shows prose.
		if run := fenceRun.FindString(trimmed); run != "" &&
			(run[0] == '~' || !strings.Contains(trimmed[len(run):], "`")) {
			if code++; code > 1 {
				report("a second command or code block; name at most one")
			}
			fence, fenceLine = run, at
			joins, quoted = false, false
			continue
		}
		if blank {
			joins, quoted = false, false
			continue
		}

		line := trimmed
		isQuote := strings.HasPrefix(line, ">")
		if isQuote {
			line = strings.TrimSpace(quoteMark.ReplaceAllString(line, ""))
			if line == "" {
				joins, quoted = false, true
				continue
			}
		}
		switch {
		case listItem.MatchString(line):
			inList = true
		case strings.HasPrefix(line, "#"), afterBlank && raw == strings.TrimLeft(raw, " \t"):
			inList = false
		}

		if strings.HasPrefix(line, "- [x] ") || strings.HasPrefix(line, "- [X] ") {
			line = "- [ ] " + line[len("- [x] "):]
		}
		for _, t := range fixed {
			if strings.HasPrefix(line, t) {
				line = line[len(t):]
				break
			}
		}

		for _, r := range rules {
			if r.pattern.MatchString(line) {
				report(r.problem)
			}
		}

		// An identifier goes only where the reader must type or find it, so
		// one at most.
		for _, span := range codeSpan.FindAllString(line, -1) {
			if code++; code > 1 {
				report(span + ": a second command or code name; name at most one")
			}
		}
		prose := codeSpan.ReplaceAllString(line, " ")
		prose = url.ReplaceAllString(prose, " ")
		prose = link.ReplaceAllString(prose, "$1")
		words := identifierWords(prose)
		for _, word := range words {
			// "adr.md" inside "docs/adr.md" is the same identifier.
			if products[word] || slices.ContainsFunc(words, func(other string) bool {
				return other != word && strings.Contains(other, word)
			}) {
				continue
			}
			report(fmt.Sprintf("%q: an identifier in the prose; say what it is", word))
		}

		if strings.HasPrefix(line, "|") {
			joins, quoted = false, isQuote
			continue
		}
		text := strings.TrimSpace(strings.ReplaceAll(marker.ReplaceAllString(prose, ""), "**", ""))
		// A quote line interrupts a paragraph, and the next quote line joins it.
		if joins && !blockStart.MatchString(line) && (!isQuote || quoted) {
			paragraphs[len(paragraphs)-1] = append(paragraphs[len(paragraphs)-1], part{at, text})
		} else {
			paragraphs = append(paragraphs, []part{{at, text}})
		}
		joins, quoted = !strings.HasPrefix(line, "#"), isQuote
	}
	// An open fence hides everything after it from the checks above.
	if fence != "" {
		found = append(found, problem{fenceLine, "a code block that is not closed"})
	}

	for _, paragraph := range paragraphs {
		for _, s := range sentences(paragraph) {
			if resultLine.MatchString(s.text) {
				continue
			}
			if n := len(strings.Fields(s.text)); n > maxWords {
				found = append(found, problem{s.line, fmt.Sprintf("a sentence of %d words; the most is %d", n, maxWords)})
			}
		}
	}

	slices.SortStableFunc(found, func(a, b problem) int { return a.line - b.line })
	list := make([]string, len(found))
	for i, p := range found {
		list[i] = fmt.Sprintf("line %d: %s", p.line, p.text)
	}
	return list
}
