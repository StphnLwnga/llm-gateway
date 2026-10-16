package authcache_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/StphnLwnga/llm-gateway/internal/authcache"
	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

const ttl = 30 * time.Second

var discard = slog.New(slog.DiscardHandler)

// Every test runs in a synctest bubble, so time.Now is a fake clock that
// moves only when every goroutine in the test is blocked.

// store is a fake key store. A lookup waits while gate is open, and fails
// with err when it is set; a hash not in keys gets tenant.ErrNoActiveKey.
type store struct {
	mu       sync.Mutex
	keys     map[string]tenant.Identity
	err      error
	gate     chan struct{}
	calls    int
	ctxs     []context.Context
	touches  [][]string
	touchErr error
}

func newStore(ids ...tenant.Identity) *store {
	s := &store{keys: map[string]tenant.Identity{}}
	for _, id := range ids {
		s.keys[string(hashOf(id))] = id
	}
	return s
}

func hashOf(id tenant.Identity) []byte {
	return tenant.HashKey("key of " + id.KeyID)
}

func (s *store) Lookup(ctx context.Context, hash []byte) (tenant.Identity, error) {
	s.mu.Lock()
	s.calls++
	s.ctxs = append(s.ctxs, ctx)
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return tenant.Identity{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return tenant.Identity{}, s.err
	}
	id, ok := s.keys[string(hash)]
	if !ok {
		return tenant.Identity{}, tenant.ErrNoActiveKey
	}
	return id, nil
}

func (s *store) Touch(_ context.Context, keyIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touches = append(s.touches, slices.Clone(keyIDs))
	return s.touchErr
}

func (s *store) set(f func(s *store)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *store) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *store) touched() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.touches)
}

var (
	idA = tenant.Identity{TenantID: "tenant-a", KeyID: "key-a"}
	idB = tenant.Identity{TenantID: "tenant-b", KeyID: "key-b"}
)

type metered struct {
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
}

func newMetered(t *testing.T) metered {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return metered{reader, mp}
}

func (m metered) collect(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, x := range sm.Metrics {
			out[x.Name] = x
		}
	}
	return out
}

// size is the llm_gateway.auth.cache.size gauge.
func (m metered) size(t *testing.T) int64 {
	t.Helper()
	g, ok := m.collect(t)["llm_gateway.auth.cache.size"].Data.(metricdata.Gauge[int64])
	if !ok || len(g.DataPoints) != 1 {
		t.Fatalf("llm_gateway.auth.cache.size = %+v, want one point", g)
	}
	return g.DataPoints[0].Value
}

// lookups is llm_gateway.auth.cache.lookups by result.
func (m metered) lookups(t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	sum, _ := m.collect(t)["llm_gateway.auth.cache.lookups"].Data.(metricdata.Sum[int64])
	for _, p := range sum.DataPoints {
		v, _ := p.Attributes.Value("llm_gateway.cache.result")
		if p.Attributes.Len() != 1 {
			t.Errorf("lookups point has attributes %v, want only llm_gateway.cache.result", p.Attributes.ToSlice())
		}
		out[v.AsString()] += p.Value
	}
	return out
}

func newCache(t *testing.T, s authcache.Store, ttl time.Duration, m metered) *authcache.Cache {
	t.Helper()
	c, err := authcache.New(s, ttl, discard, m.mp)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wantServed reports an error unless the cache serves want. It does not stop
// the test, so a goroutine other than the test's may call it.
func wantServed(t *testing.T, c *authcache.Cache, want tenant.Identity) {
	t.Helper()
	got, err := c.Lookup(t.Context(), hashOf(want))
	if err != nil || got != want {
		t.Errorf("Lookup(%s) = %+v, %v; want %+v", want.KeyID, got, err, want)
	}
}

// lockedBuffer is a log the cache's goroutines and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAHitWithinTheTTLMakesNoStoreCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newMetered(t)
		s := newStore(idA)
		c := newCache(t, s, ttl, m)
		wantServed(t, c, idA)
		time.Sleep(ttl - time.Nanosecond)
		wantServed(t, c, idA)
		if n := s.callCount(); n != 1 {
			t.Errorf("store calls = %d, want 1: the second lookup is within the TTL", n)
		}
		if got := m.lookups(t); got["hit"] != 1 || got["miss"] != 1 {
			t.Errorf("lookups = %v, want 1 hit and 1 miss", got)
		}
		if n := m.size(t); n != 1 {
			t.Errorf("cache size = %d, want 1", n)
		}
		if d, _ := m.collect(t)["llm_gateway.auth.lookup.duration"].Data.(metricdata.Histogram[float64]); len(d.DataPoints) != 1 || d.DataPoints[0].Count != 1 {
			t.Errorf("llm_gateway.auth.lookup.duration = %+v, want one store lookup recorded", d.DataPoints)
		}
	})
}

// A lookup that reads the row just before a revoke and returns after it must
// not keep the key for the TTL plus its own duration.
func TestAnEntryExpiresAtTheLookupsStartPlusTheTTL(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA)
		gate := make(chan struct{})
		s.set(func(s *store) { s.gate = gate })
		c := newCache(t, s, ttl, newMetered(t))
		start := time.Now()
		done := make(chan error)
		go func() {
			_, err := c.Lookup(t.Context(), hashOf(idA))
			done <- err
		}()
		synctest.Wait()
		time.Sleep(1500 * time.Millisecond) // within the 2 s bound on a shared lookup
		s.set(func(s *store) { s.gate = nil })
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		time.Sleep(time.Until(start.Add(ttl)) - time.Nanosecond)
		wantServed(t, c, idA)
		if n := s.callCount(); n != 1 {
			t.Fatalf("store calls just before start + TTL = %d, want 1", n)
		}
		time.Sleep(time.Nanosecond)
		wantServed(t, c, idA)
		if n := s.callCount(); n != 2 {
			t.Errorf("store calls at start + TTL = %d, want 2: the entry expires at the lookup's start plus the TTL, not its end", n)
		}
	})
}

func TestRejectionsAndErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newMetered(t)
		s := newStore()
		c := newCache(t, s, ttl, m)
		for range 2 {
			if _, err := c.Lookup(t.Context(), hashOf(idA)); !errors.Is(err, tenant.ErrNoActiveKey) {
				t.Fatalf("Lookup(unknown) = %v, want ErrNoActiveKey", err)
			}
		}
		if n := s.callCount(); n != 2 {
			t.Errorf("store calls for 2 rejected lookups = %d, want 2: a rejection is not cached", n)
		}
		down := errors.New("connection refused")
		s.set(func(s *store) { s.err = down })
		for range 2 {
			if _, err := c.Lookup(t.Context(), hashOf(idA)); !errors.Is(err, down) {
				t.Fatalf("Lookup with the store down = %v, want its error", err)
			}
		}
		if n := s.callCount(); n != 4 {
			t.Errorf("store calls = %d, want 4: an error is not cached", n)
		}
		s.set(func(s *store) { s.keys[string(hashOf(idA))] = idA; s.err = nil })
		wantServed(t, c, idA) // a key created after a rejection works at once
		if n := m.size(t); n != 1 {
			t.Errorf("cache size = %d, want 1: only the accepted key", n)
		}
	})
}

func TestATTLOfZeroSendsEveryLookupToTheStore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newMetered(t)
		s := newStore(idA)
		c := newCache(t, s, 0, m)
		for range 3 {
			wantServed(t, c, idA)
		}
		if n := s.callCount(); n != 3 {
			t.Fatalf("store calls for 3 lookups = %d, want 3", n)
		}

		gate := make(chan struct{})
		s.set(func(s *store) { s.gate = gate })
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { wantServed(t, c, idA) })
		}
		synctest.Wait()
		if n := s.callCount(); n != 5 {
			t.Errorf("store calls with 2 concurrent lookups = %d, want 5: with the cache off, lookups are not shared", n)
		}
		close(gate)
		wg.Wait()

		if n := m.size(t); n != 0 {
			t.Errorf("cache size = %d, want 0", n)
		}
		if got := m.lookups(t); len(got) != 0 {
			t.Errorf("lookups = %v, want none counted with the cache off", got)
		}
		c.Flush(t.Context())
		if got := s.touched(); !slices.EqualFunc(got, [][]string{{"key-a"}}, slices.Equal) {
			t.Errorf("touches = %v, want key-a: last-used times do not need the cache", got)
		}
	})
}

func TestTheSweepRemovesExpiredEntries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newMetered(t)
		s := newStore(idA, idB)
		c := newCache(t, s, ttl, m)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go c.Run(ctx)

		wantServed(t, c, idA)
		time.Sleep(40 * time.Second)
		wantServed(t, c, idB)
		if n := m.size(t); n != 2 {
			t.Errorf("size at 40 s = %d, want 2: the expired entry stays until the sweep", n)
		}
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if n := m.size(t); n != 1 {
			t.Errorf("size after the sweep at 60 s = %d, want 1: only the entry from 40 s is left", n)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := m.size(t); n != 0 {
			t.Errorf("size after the sweep at 120 s = %d, want 0", n)
		}
	})
}

// Key X is cached 25 s before both keys are revoked and key Y 1 s before.
// No revoked key is served from 30 s after the revoke.
func TestARevokedKeyStopsWithinTheTTL(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		x := tenant.Identity{TenantID: "tenant-x", KeyID: "key-x"}
		y := tenant.Identity{TenantID: "tenant-y", KeyID: "key-y"}
		s := newStore(x, y)
		c := newCache(t, s, ttl, newMetered(t))
		wantServed(t, c, x)
		time.Sleep(24 * time.Second)
		wantServed(t, c, y)
		time.Sleep(time.Second)
		revoked := time.Now()
		s.set(func(s *store) { clear(s.keys) })

		const second = time.Second
		for _, at := range []time.Duration{0, 5*second - 1, 5 * second, 29*second - 1, 29 * second, ttl, ttl + second, 2 * ttl} {
			time.Sleep(time.Until(revoked.Add(at)))
			for _, k := range []struct {
				id      tenant.Identity
				stopsAt time.Duration // after the revoke
			}{{x, 5 * second}, {y, 29 * second}} {
				_, err := c.Lookup(t.Context(), hashOf(k.id))
				if served := err == nil; served != (at < k.stopsAt) {
					t.Errorf("%s %v after the revoke: served = %v (%v); want served only before %v", k.id.KeyID, at, served, err, k.stopsAt)
				}
				if err != nil && !errors.Is(err, tenant.ErrNoActiveKey) {
					t.Errorf("%s %v after the revoke = %v, want ErrNoActiveKey", k.id.KeyID, at, err)
				}
			}
		}
	})
}

func TestConcurrentMissesShareOneLookup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA)
		gate := make(chan struct{})
		s.set(func(s *store) { s.gate = gate })
		c := newCache(t, s, ttl, newMetered(t))
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() { wantServed(t, c, idA) })
		}
		synctest.Wait()
		if n := s.callCount(); n != 1 {
			t.Errorf("store calls for 100 concurrent misses = %d, want 1", n)
		}
		close(gate)
		wg.Wait()
		wantServed(t, c, idA)
		if n := s.callCount(); n != 1 {
			t.Errorf("store calls after the shared lookup = %d, want 1", n)
		}
	})
}

func TestACallerThatLeavesDoesNotFailTheOthers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		type marker struct{}
		s := newStore(idA)
		gate := make(chan struct{})
		s.set(func(s *store) { s.gate = gate })
		c := newCache(t, s, ttl, newMetered(t))

		leaderCtx, leave := context.WithCancel(context.WithValue(t.Context(), marker{}, "leader"))
		leader := make(chan error, 1)
		go func() {
			_, err := c.Lookup(leaderCtx, hashOf(idA))
			leader <- err
		}()
		synctest.Wait()
		started := time.Now()
		var wg sync.WaitGroup
		for range 99 {
			wg.Go(func() { wantServed(t, c, idA) })
		}
		synctest.Wait()

		leave()
		synctest.Wait()
		select {
		case err := <-leader:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the leaving caller got %v, want its own context.Canceled", err)
			}
		default:
			t.Fatal("the leaving caller is still waiting for the shared lookup")
		}

		close(gate)
		wg.Wait()
		if n := s.callCount(); n != 1 {
			t.Errorf("store calls = %d, want 1", n)
		}
		var ctx context.Context
		s.set(func(s *store) { ctx = s.ctxs[0] })
		if ctx.Value(marker{}) != "leader" {
			t.Error("the shared lookup lost the first caller's context values, and with them its trace")
		}
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(started.Add(2*time.Second)) {
			t.Errorf("shared lookup deadline = %v, %v; want 2 s after it started", deadline, ok)
		}
	})
}

func TestASharedLookupIsBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA)
		s.set(func(s *store) { s.gate = make(chan struct{}) })
		c := newCache(t, s, ttl, newMetered(t))
		start := time.Now()
		_, err := c.Lookup(t.Context(), hashOf(idA))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2*time.Second {
			t.Errorf("Lookup with the store hung = %v after %v, want DeadlineExceeded after 2s", err, time.Since(start))
		}
	})
}

func TestAnExpiredEntryIsNotServedWhenTheStoreFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA)
		c := newCache(t, s, ttl, newMetered(t))
		wantServed(t, c, idA)
		time.Sleep(ttl)
		down := errors.New("connection refused")
		s.set(func(s *store) { s.err = down })
		if id, err := c.Lookup(t.Context(), hashOf(idA)); !errors.Is(err, down) || id != (tenant.Identity{}) {
			t.Errorf("Lookup of an expired entry with the store down = %+v, %v; want no identity and the store's error", id, err)
		}
	})
}

func TestServedKeysAreTouchedOncePerInterval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA, idB)
		c := newCache(t, s, ttl, newMetered(t))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go c.Run(ctx)

		wantServed(t, c, idB)
		wantServed(t, c, idA)
		wantServed(t, c, idA)
		if _, err := c.Lookup(t.Context(), tenant.HashKey("unknown")); !errors.Is(err, tenant.ErrNoActiveKey) {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := s.touched(); !slices.EqualFunc(got, [][]string{{"key-a", "key-b"}}, slices.Equal) {
			t.Fatalf("touches after one interval = %v, want one call with key-a and key-b", got)
		}
		time.Sleep(10 * time.Second)
		wantServed(t, c, idB)
		time.Sleep(10 * time.Second)
		wantServed(t, c, idB)
		time.Sleep(40 * time.Second)
		synctest.Wait()
		if got := s.touched(); !slices.EqualFunc(got, [][]string{{"key-a", "key-b"}, {"key-b"}}, slices.Equal) {
			t.Errorf("touches after two intervals = %v, want a second call with key-b only", got)
		}
	})
}

func TestNothingIsTouchedWhenNoKeyWasServed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore()
		c := newCache(t, s, ttl, newMetered(t))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go c.Run(ctx)
		_, _ = c.Lookup(t.Context(), hashOf(idA))
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if got := s.touched(); len(got) != 0 {
			t.Errorf("touches = %v, want none", got)
		}
	})
}

func TestAFailedFlushIsLoggedAndRetried(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA, idB)
		s.set(func(s *store) { s.touchErr = errors.New("connection refused") })
		var logs lockedBuffer
		c, err := authcache.New(s, ttl, slog.New(slog.NewTextHandler(&logs, nil)), newMetered(t).mp)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go c.Run(ctx)

		wantServed(t, c, idA)
		time.Sleep(time.Minute)
		synctest.Wait()
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "connection refused") {
			t.Errorf("a failed flush is not logged as a warning:\n%s", logs.String())
		}
		s.set(func(s *store) { s.touchErr = nil })
		wantServed(t, c, idB)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := s.touched(); !slices.EqualFunc(got, [][]string{{"key-a"}, {"key-a", "key-b"}}, slices.Equal) {
			t.Errorf("touches = %v, want key-a retried with key-b", got)
		}
	})
}

func TestRunLeavesTheLastFlushToTheCaller(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idA)
		c := newCache(t, s, ttl, newMetered(t))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		wantServed(t, c, idA)
		cancel()
		<-done
		if got := s.touched(); len(got) != 0 {
			t.Fatalf("touches when Run returned = %v, want none", got)
		}
		c.Flush(t.Context())
		if got := s.touched(); !slices.EqualFunc(got, [][]string{{"key-a"}}, slices.Equal) {
			t.Errorf("touches after the shutdown flush = %v, want key-a", got)
		}
	})
}

func TestAFlushThatHangsEndsAfterTheBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := newCache(t, &hangingStore{store: newStore(idA)}, ttl, newMetered(t))
		wantServed(t, c, idA)
		start := time.Now()
		c.Flush(t.Context())
		if took := time.Since(start); took != 2*time.Second {
			t.Errorf("a flush with the store hung took %v, want 2s", took)
		}
	})
}

// hangingStore never finishes a Touch before its context ends.
type hangingStore struct{ *store }

func (hangingStore) Touch(ctx context.Context, _ []string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestLookupSweepAndFlushRunTogether(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ids := []tenant.Identity{idA, idB, {TenantID: "tenant-c", KeyID: "key-c"}}
		s := newStore(ids...)
		c := newCache(t, s, ttl, newMetered(t))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				for j := range 200 {
					wantServed(t, c, ids[(i+j)%len(ids)])
					time.Sleep(time.Second)
				}
			})
		}
		wg.Go(func() {
			for range 10 {
				c.Flush(ctx)
				time.Sleep(15 * time.Second)
			}
		})
		wg.Wait()
		cancel()
		<-done
		if len(s.touched()) == 0 {
			t.Error("no flush reached the store")
		}
	})
}
