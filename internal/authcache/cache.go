// Package authcache serves accepted API keys from memory for a bounded time,
// and writes the keys' last-used times in batches.
package authcache

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// Store looks up API keys and records when they were used.
type Store interface {
	Lookup(ctx context.Context, hash []byte) (tenant.Identity, error)
	Touch(ctx context.Context, keyIDs []string) error
}

const (
	sweepInterval = time.Minute
	flushInterval = time.Minute

	// storeTimeout bounds a shared lookup, which no longer ends with the
	// request that started it, and a flush. The readiness check gives
	// Postgres the same 2 s.
	storeTimeout = 2 * time.Second
)

const (
	scope      = "github.com/StphnLwnga/llm-gateway/internal/authcache"
	attrResult = "llm_gateway.cache.result"
)

// The default HTTP buckets start at 5 ms, above a whole lookup on a local network.
var lookupBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

type entry struct {
	id tenant.Identity
	// expires counts from the start of the lookup, so a lookup that read the
	// row just before a revoke does not add its own duration to the key's life.
	expires time.Time
}

// Cache serves a key that the store accepted until the TTL has passed since
// the lookup started. It caches no rejection and no error. A TTL of 0 turns
// the cache off, and every lookup goes to the store.
type Cache struct {
	store Store
	ttl   time.Duration
	log   *slog.Logger

	flights singleflight.Group

	mu      sync.Mutex
	entries map[string]entry    // by key hash
	used    map[string]struct{} // IDs of the keys served since the last flush

	// flushMu stops a shutdown flush from passing a periodic one that may still put IDs back.
	flushMu sync.Mutex

	lookups   metric.Int64Counter
	hit, miss metric.AddOption
	duration  metric.Float64Histogram
}

// New returns a cache in front of store. A nil mp uses the global meter provider.
func New(store Store, ttl time.Duration, log *slog.Logger, mp metric.MeterProvider) (*Cache, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	c := &Cache{
		store: store, ttl: ttl, log: log,
		entries: map[string]entry{}, used: map[string]struct{}{},
		hit:  metric.WithAttributeSet(attribute.NewSet(attribute.String(attrResult, "hit"))),
		miss: metric.WithAttributeSet(attribute.NewSet(attribute.String(attrResult, "miss"))),
	}
	m := mp.Meter(scope)
	var errs [3]error
	c.lookups, errs[0] = m.Int64Counter("llm_gateway.auth.cache.lookups", metric.WithUnit("{lookup}"),
		metric.WithDescription("API key checks made through the auth cache, by whether the key was in it."))
	c.duration, errs[1] = m.Float64Histogram("llm_gateway.auth.lookup.duration", metric.WithUnit("s"),
		metric.WithDescription("Time of one API key lookup in Postgres."),
		metric.WithExplicitBucketBoundaries(lookupBuckets...))
	_, errs[2] = m.Int64ObservableGauge("llm_gateway.auth.cache.size", metric.WithUnit("{entry}"),
		metric.WithDescription("API keys held in the auth cache, expired ones included until the sweep removes them."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			c.mu.Lock()
			n := len(c.entries)
			c.mu.Unlock()
			o.Observe(int64(n))
			return nil
		}))
	return c, errors.Join(errs[:]...)
}

// Lookup returns the identity of the active key with this hash, or tenant.ErrNoActiveKey.
func (c *Cache) Lookup(ctx context.Context, hash []byte) (tenant.Identity, error) {
	if c.ttl == 0 {
		id, err := c.lookup(ctx, hash, time.Now())
		if err != nil {
			return tenant.Identity{}, err
		}
		c.markUsed(id.KeyID)
		return id, nil
	}

	k := string(hash)
	c.mu.Lock()
	if e, ok := c.entries[k]; ok && time.Now().Before(e.expires) {
		c.used[e.id.KeyID] = struct{}{}
		c.mu.Unlock()
		c.lookups.Add(ctx, 1, c.hit)
		return e.id, nil
	}
	c.mu.Unlock()
	c.lookups.Add(ctx, 1, c.miss)

	// Other callers may wait on this lookup, so it must not end when the
	// caller that started it leaves. WithoutCancel keeps the trace.
	res := c.flights.DoChan(k, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer cancel()
		start := time.Now()
		id, err := c.lookup(ctx, []byte(k), start)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.entries[k] = entry{id: id, expires: start.Add(c.ttl)}
		c.mu.Unlock()
		return id, nil
	})
	select {
	case <-ctx.Done():
		return tenant.Identity{}, ctx.Err()
	case r := <-res:
		if r.Err != nil {
			return tenant.Identity{}, r.Err
		}
		id := r.Val.(tenant.Identity)
		c.markUsed(id.KeyID)
		return id, nil
	}
}

func (c *Cache) lookup(ctx context.Context, hash []byte, start time.Time) (tenant.Identity, error) {
	id, err := c.store.Lookup(ctx, hash)
	c.duration.Record(ctx, time.Since(start).Seconds())
	return id, err
}

func (c *Cache) markUsed(keyID string) {
	c.mu.Lock()
	c.used[keyID] = struct{}{}
	c.mu.Unlock()
}

// Run removes expired entries and flushes the last-used times every minute
// until ctx ends. It does not flush when it returns: the caller flushes after
// the server has stopped, so the last requests are in that flush.
func (c *Cache) Run(ctx context.Context) {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			c.sweep()
		case <-flush.C:
			c.Flush(ctx)
		}
	}
}

func (c *Cache) sweep() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.entries, func(_ string, e entry) bool { return !now.Before(e.expires) })
}

// Flush writes the last-used time of every key served since the last flush.
// A failure is logged, and those keys go into the next flush.
func (c *Cache) Flush(ctx context.Context) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	used := c.used
	c.used = map[string]struct{}{}
	c.mu.Unlock()
	if len(used) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := c.store.Touch(ctx, slices.Sorted(maps.Keys(used))); err != nil {
		c.log.WarnContext(ctx, "last-used times not saved, the next flush retries them", "err", err, "keys", len(used))
		c.mu.Lock()
		maps.Copy(c.used, used)
		c.mu.Unlock()
	}
}
