package fakeupstream

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Model is the one model the fake serves. Directives follow a "?", for
// example "fake?ttfb=2s&tokens=50&tpot=20ms".
const Model = "fake"

const (
	defaultTokens = 5
	maxTokens     = 100_000
	maxDelay      = 10 * time.Minute
)

var errUnknownModel = errors.New("unknown model")

// plan is what one request asks the fake to do.
type plan struct {
	ttfb       time.Duration
	firstToken time.Duration
	tpot       time.Duration
	tokens     int

	// -1 when not set
	stallAfter     int
	dropAfter      int
	malformedAfter int

	status     int
	retryAfter int // seconds; -1 when not set
}

var directives = map[string]func(p *plan, v string) error{
	"ttfb":            func(p *plan, v string) (err error) { p.ttfb, err = delay(v); return err },
	"first_token":     func(p *plan, v string) (err error) { p.firstToken, err = delay(v); return err },
	"tpot":            func(p *plan, v string) (err error) { p.tpot, err = delay(v); return err },
	"tokens":          func(p *plan, v string) (err error) { p.tokens, err = count(v); return err },
	"stall_after":     func(p *plan, v string) (err error) { p.stallAfter, err = count(v); return err },
	"drop_after":      func(p *plan, v string) (err error) { p.dropAfter, err = count(v); return err },
	"malformed_after": func(p *plan, v string) (err error) { p.malformedAfter, err = count(v); return err },
	"status":          func(p *plan, v string) (err error) { p.status, err = status(v); return err },
	"retry_after":     func(p *plan, v string) (err error) { p.retryAfter, err = count(v); return err },
}

// Directives lists every directive the model name accepts, sorted.
func Directives() []string {
	out := make([]string, 0, len(directives))
	for k := range directives {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func parseModel(model string) (plan, error) {
	name, query, hasQuery := strings.Cut(model, "?")
	if name != Model {
		return plan{}, errUnknownModel
	}
	p := plan{tokens: defaultTokens, stallAfter: -1, dropAfter: -1, malformedAfter: -1, retryAfter: -1}
	if !hasQuery {
		return p, nil
	}
	vals, err := url.ParseQuery(query)
	if err != nil {
		return plan{}, fmt.Errorf("directives %q: %w", query, err)
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		set, ok := directives[k]
		if !ok {
			return plan{}, fmt.Errorf("unknown directive %q; known: %s", k, strings.Join(Directives(), ", "))
		}
		if len(vals[k]) != 1 {
			return plan{}, fmt.Errorf("directive %s is set more than once", k)
		}
		if err := set(&p, vals[k][0]); err != nil {
			return plan{}, fmt.Errorf("directive %s=%q: %w", k, vals[k][0], err)
		}
	}
	return p, p.validate()
}

func (p plan) validate() error {
	faults := 0
	for name, n := range map[string]int{"stall_after": p.stallAfter, "drop_after": p.dropAfter, "malformed_after": p.malformedAfter} {
		if n < 0 {
			continue
		}
		faults++
		if n > p.tokens {
			return fmt.Errorf("%s=%d is more than tokens=%d", name, n, p.tokens)
		}
	}
	if faults > 1 {
		return errors.New("set at most one of stall_after, drop_after and malformed_after")
	}
	if p.retryAfter >= 0 && p.status == 0 {
		return errors.New("retry_after needs status")
	}
	return nil
}

func delay(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || d > maxDelay {
		return 0, fmt.Errorf("must be a duration from 0 to %s, for example 300ms", maxDelay)
	}
	return d, nil
}

func count(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maxTokens {
		return 0, fmt.Errorf("must be a whole number from 0 to %d", maxTokens)
	}
	return n, nil
}

func status(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 400 || n > 599 {
		return 0, errors.New("must be an error status from 400 to 599")
	}
	return n, nil
}
