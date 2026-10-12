package router_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/router"
)

// source is a fake routes table; set err to make the next loads fail.
type source struct {
	mu     sync.Mutex
	routes []router.Route
	err    error
	calls  atomic.Int32
}

func (s *source) EnabledRoutes(context.Context) ([]router.Route, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return append([]router.Route(nil), s.routes...), nil
}

func (s *source) set(routes []router.Route, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes, s.err = routes, err
}

type fakeProvider struct{ name string }

func (fakeProvider) Chat(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (fakeProvider) Stream(context.Context, provider.Request, func(provider.Event) error) error {
	return nil
}

var (
	groq   = fakeProvider{"groq"}
	ollama = fakeProvider{"ollama"}
	t0     = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func route(alias, prov, model string, priority int, created time.Time) router.Route {
	return router.Route{Alias: alias, Provider: prov, UpstreamModel: model, Priority: priority, Enabled: true, CreatedAt: created}
}

func newRouter(src router.Source, logs *bytes.Buffer) *router.Router {
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	return router.New(src, map[string]provider.Provider{"groq": groq, "ollama": ollama}, time.Hour,
		slog.New(slog.NewTextHandler(logs, nil)))
}

func models(ts []router.Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Route.Provider+"/"+t.Route.UpstreamModel)
	}
	return out
}

func TestResolveOrdersTargetsByPriority(t *testing.T) {
	t.Parallel()
	src := &source{routes: []router.Route{
		route("chat", "ollama", "llama3.2:3b", 20, t0),
		route("chat", "groq", "llama-8b", 10, t0),
		route("chat", "groq", "llama-70b", 15, t0),
	}}
	r := newRouter(src, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(models(r.Resolve("chat")), ",")
	if want := "groq/llama-8b,groq/llama-70b,ollama/llama3.2:3b"; got != want {
		t.Errorf("Resolve = %s, want %s", got, want)
	}
	if r.Resolve("nope") != nil {
		t.Error("Resolve of an unknown alias is not nil")
	}
}

func TestRoutesWithoutAnEnabledProviderAreSkippedAndLoggedOnce(t *testing.T) {
	t.Parallel()
	src := &source{routes: []router.Route{
		route("chat", "openai", "gpt-x", 1, t0),
		route("chat", "groq", "llama-8b", 2, t0),
		route("gpt-only", "openai", "gpt-x", 1, t0),
	}}
	var logs bytes.Buffer
	r := newRouter(src, &logs)
	for range 3 {
		if err := r.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(models(r.Resolve("chat")), ","); got != "groq/llama-8b" {
		t.Errorf("Resolve = %s, want only the groq target", got)
	}
	if r.Resolve("gpt-only") != nil {
		t.Error("an alias with no usable target resolved")
	}
	if n := strings.Count(logs.String(), "routes skipped"); n != 1 {
		t.Errorf("skip warning logged %d times over 3 refreshes, want 1:\n%s", n, logs.String())
	}
	src.set([]router.Route{route("chat", "groq", "llama-8b", 2, t0)}, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	src.set([]router.Route{route("chat", "openai", "gpt-x", 1, t0)}, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "routes skipped"); n != 2 {
		t.Errorf("skip warning logged %d times after the set changed, want 2", n)
	}
}

func TestModelsListsUsableAliasesWithTheEarliestCreatedAt(t *testing.T) {
	t.Parallel()
	src := &source{routes: []router.Route{
		route("b-chat", "groq", "m1", 1, t0.Add(48*time.Hour)),
		route("b-chat", "ollama", "m2", 2, t0.Add(24*time.Hour)),
		route("a-chat", "groq", "m3", 1, t0),
		route("hidden", "openai", "m4", 1, t0),
	}}
	r := newRouter(src, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := r.Models()
	if len(got) != 2 || got[0].Alias != "a-chat" || got[1].Alias != "b-chat" || !got[1].Created.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("Models = %+v, want a-chat, then b-chat created at the earlier route", got)
	}
}

func TestDisabledRoutesAreIgnored(t *testing.T) {
	t.Parallel()
	off := route("chat", "groq", "llama-8b", 1, t0)
	off.Enabled = false
	r := newRouter(&source{routes: []router.Route{off}}, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Resolve("chat") != nil || len(r.Models()) != 0 {
		t.Error("a disabled route is served")
	}
}

func TestAFailedRefreshKeepsTheLastGoodSnapshot(t *testing.T) {
	t.Parallel()
	src := &source{routes: []router.Route{route("chat", "groq", "llama-8b", 1, t0)}}
	var logs bytes.Buffer
	r := newRouter(src, &logs)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	src.set(nil, errors.New("connection refused"))
	if err := r.Refresh(t.Context()); err == nil {
		t.Fatal("Refresh succeeded with the source down")
	}
	if got := models(r.Resolve("chat")); len(got) != 1 {
		t.Errorf("after a failed refresh Resolve = %v, want the last good target", got)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check after a failed refresh = %v, want ready: the last good snapshot still serves", err)
	}
	if !strings.Contains(logs.String(), "keeping the last good snapshot") {
		t.Errorf("no warning logged:\n%s", logs.String())
	}
}

func TestCheckFailsUntilTheFirstLoad(t *testing.T) {
	t.Parallel()
	src := &source{err: errors.New("connection refused")}
	r := newRouter(src, nil)
	if err := r.Check(t.Context()); !errors.Is(err, router.ErrNotLoaded) {
		t.Fatalf("Check before any load = %v, want ErrNotLoaded", err)
	}
	_ = r.Refresh(t.Context())
	if err := r.Check(t.Context()); !errors.Is(err, router.ErrNotLoaded) {
		t.Fatalf("Check after a failed first load = %v, want ErrNotLoaded", err)
	}
	src.set([]router.Route{}, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Errorf("Check after the first load = %v, want nil (an empty table is a valid snapshot)", err)
	}
}

func TestRunRefreshesUntilTheContextEnds(t *testing.T) {
	t.Parallel()
	src := &source{routes: []router.Route{route("chat", "groq", "m", 1, t0)}}
	r := router.New(src, map[string]provider.Provider{"groq": groq}, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if n := src.calls.Load(); n < 3 {
		t.Errorf("Run refreshed %d times in 100ms at a 10ms interval, want several", n)
	}
}

// TestResolveDuringSwaps runs under -race: readers must never see a torn snapshot.
func TestResolveDuringSwaps(t *testing.T) {
	t.Parallel()
	a := []router.Route{route("chat", "groq", "a", 1, t0), route("chat", "ollama", "a2", 2, t0)}
	b := []router.Route{route("chat", "groq", "b", 1, t0), route("chat", "ollama", "b2", 2, t0)}
	src := &source{routes: a}
	r := newRouter(src, nil)
	if err := r.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ctx.Err() == nil; i++ {
			if i%2 == 0 {
				src.set(b, nil)
			} else {
				src.set(a, nil)
			}
			_ = r.Refresh(ctx)
		}
	})
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				got := models(r.Resolve("chat"))
				if len(got) != 2 || got[0][len(got[0])-1] != got[1][len(got[1])-2] {
					t.Errorf("torn snapshot: %v", got)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestValidate(t *testing.T) {
	t.Parallel()
	one, ten := 1, 10
	ok := router.Route{Alias: "chat", Provider: "groq", UpstreamModel: "m"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate of a good route: %v", err)
	}
	cases := map[string]router.Route{
		"unknown provider":  {Alias: "chat", Provider: "anthropic", UpstreamModel: "m"},
		"negative price":    {Alias: "chat", Provider: "groq", UpstreamModel: "m", OutputPrice: -1},
		"max below default": {Alias: "chat", Provider: "groq", UpstreamModel: "m", DefaultMaxTokens: &ten, MaxMaxTokens: &one},
		"no alias":          {Provider: "groq", UpstreamModel: "m"},
		"no upstream model": {Alias: "chat", Provider: "groq"},
	}
	for name, rt := range cases {
		if err := rt.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded", name)
		}
	}
}
