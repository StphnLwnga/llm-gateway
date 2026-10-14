package tenant

import (
	"context"
	"errors"
)

// ErrNoActiveKey is returned when no unrevoked key of an enabled tenant has the hash.
var ErrNoActiveKey = errors.New("no active API key")

// SpanAttribute names the tenant ID on spans. It never goes on a metric
// (ADR 0016): every tenant would be a new series.
const SpanAttribute = "llm_gateway.tenant.id"

// Identity is the authenticated caller.
type Identity struct {
	TenantID string
	KeyID    string
}

type identityKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the identity in ctx, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}
