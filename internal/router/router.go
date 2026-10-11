// Package router resolves a model alias to the providers that serve it, from
// an in-memory snapshot of the routes table.
package router

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/money"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

// Providers lists every provider name a route may use.
var Providers = []string{"groq", "openai", "ollama"}

// Route is one row of the routes table.
type Route struct {
	ID            int64
	Alias         string // the model name clients send
	Provider      string
	UpstreamModel string
	Priority      int // lower is tried first
	Enabled       bool
	// Prices are per million tokens.
	InputPrice       money.MicroUSD
	OutputPrice      money.MicroUSD
	DefaultMaxTokens *int
	MaxMaxTokens     *int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Validate reports every problem with a route that is about to be stored.
func (rt Route) Validate() error {
	var errs []error
	if rt.Alias == "" {
		errs = append(errs, errors.New("alias: required"))
	}
	if !slices.Contains(Providers, rt.Provider) {
		errs = append(errs, fmt.Errorf("provider %q: must be one of %s", rt.Provider, strings.Join(Providers, ", ")))
	}
	if rt.UpstreamModel == "" {
		errs = append(errs, errors.New("upstream model: required"))
	}
	if rt.InputPrice < 0 || rt.OutputPrice < 0 {
		errs = append(errs, errors.New("prices: must not be negative"))
	}
	if rt.DefaultMaxTokens != nil && *rt.DefaultMaxTokens < 1 {
		errs = append(errs, errors.New("default max tokens: must be 1 or more"))
	}
	if rt.MaxMaxTokens != nil && *rt.MaxMaxTokens < 1 {
		errs = append(errs, errors.New("max max tokens: must be 1 or more"))
	}
	if rt.DefaultMaxTokens != nil && rt.MaxMaxTokens != nil && *rt.MaxMaxTokens < *rt.DefaultMaxTokens {
		errs = append(errs, errors.New("max max tokens: must not be below default max tokens"))
	}
	return errors.Join(errs...)
}

// Source loads the enabled routes.
type Source interface {
	EnabledRoutes(ctx context.Context) ([]Route, error)
}

// Target is one way to serve an alias: a route and the provider that runs it.
type Target struct {
	Route    Route
	Provider provider.Provider
}

// Model is one alias that has at least one usable target.
type Model struct {
	Alias   string
	Created time.Time // the earliest created_at among its usable routes
}

type snapshot struct {
	targets map[string][]Target
	models  []Model
}

// Router serves Resolve and Models from a snapshot that Run refreshes. The
// request path never queries Postgres.
type Router struct {
	source    Source
	providers map[string]provider.Provider
	interval  time.Duration
	log       *slog.Logger

	snap atomic.Pointer[snapshot]

	mu      sync.Mutex
	skipped string // the last logged set of routes without a provider
}

// ErrNotLoaded means no snapshot has loaded yet.
var ErrNotLoaded = errors.New("routes not loaded yet")

// New returns a router. providers holds the enabled providers by name.
func New(source Source, providers map[string]provider.Provider, interval time.Duration, log *slog.Logger) *Router {
	return &Router{source: source, providers: providers, interval: interval, log: log}
}

// Run loads the routes now and then every interval, until ctx ends.
func (r *Router) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		_ = r.Refresh(ctx) // Refresh logs its own failures
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh loads the routes and swaps the snapshot. On failure it keeps the
// last good snapshot, logs a warning and returns the error.
func (r *Router) Refresh(ctx context.Context) error {
	routes, err := r.source.EnabledRoutes(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "routes refresh failed, keeping the last good snapshot", "err", err, "loaded", r.snap.Load() != nil)
		return fmt.Errorf("refresh routes: %w", err)
	}
	s := &snapshot{targets: map[string][]Target{}}
	var skipped []string
	for _, rt := range routes {
		if !rt.Enabled {
			continue
		}
		p, ok := r.providers[rt.Provider]
		if !ok {
			skipped = append(skipped, fmt.Sprintf("%s -> %s/%s", rt.Alias, rt.Provider, rt.UpstreamModel))
			continue
		}
		s.targets[rt.Alias] = append(s.targets[rt.Alias], Target{Route: rt, Provider: p})
	}
	for alias, ts := range s.targets {
		slices.SortFunc(ts, func(a, b Target) int { return cmp.Compare(a.Route.Priority, b.Route.Priority) })
		created := ts[0].Route.CreatedAt
		for _, t := range ts[1:] {
			if t.Route.CreatedAt.Before(created) {
				created = t.Route.CreatedAt
			}
		}
		s.models = append(s.models, Model{Alias: alias, Created: created})
	}
	slices.SortFunc(s.models, func(a, b Model) int { return cmp.Compare(a.Alias, b.Alias) })
	r.snap.Store(s)
	r.logSkipped(ctx, skipped)
	return nil
}

// logSkipped logs routes whose provider is not enabled, only when that set changes.
func (r *Router) logSkipped(ctx context.Context, skipped []string) {
	slices.Sort(skipped)
	key := strings.Join(skipped, ", ")
	r.mu.Lock()
	changed := key != r.skipped
	r.skipped = key
	r.mu.Unlock()
	if changed && key != "" {
		r.log.WarnContext(ctx, "routes skipped: provider not enabled", "routes", skipped)
	}
}

// Resolve returns the usable targets for alias, lowest priority first, or nil.
func (r *Router) Resolve(alias string) []Target {
	s := r.snap.Load()
	if s == nil {
		return nil
	}
	return slices.Clone(s.targets[alias]) // callers must not change the shared snapshot
}

// Models returns the aliases that have at least one usable target, sorted.
func (r *Router) Models() []Model {
	s := r.snap.Load()
	if s == nil {
		return nil
	}
	return slices.Clone(s.models)
}

// Check is a readiness check: it fails until the first snapshot has loaded.
// A later failed refresh does not fail it, because the last good snapshot
// still serves.
func (r *Router) Check(context.Context) error {
	if r.snap.Load() == nil {
		return ErrNotLoaded
	}
	return nil
}
