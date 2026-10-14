package httpapi

import (
	"bytes"
	"context"
	"net/http"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// FakeKey is the one key FakeKeys accepts. The external tests use both too.
var FakeKey = func() string {
	key, err := tenant.NewKey()
	if err != nil {
		panic(err)
	}
	return key
}()

// FakeIdentity is the caller FakeKey authenticates as.
var FakeIdentity = tenant.Identity{TenantID: "0199e1a0-0000-7000-8000-00000000000a", KeyID: "0199e1a0-0000-7000-8000-00000000000b"}

// FakeKeys is a key store that holds FakeKey only.
type FakeKeys struct{}

func (FakeKeys) Lookup(_ context.Context, hash []byte) (tenant.Identity, error) {
	if bytes.Equal(hash, tenant.HashKey(FakeKey)) {
		return FakeIdentity, nil
	}
	return tenant.Identity{}, tenant.ErrNoActiveKey
}

// WithKey sends FakeKey with r.
func WithKey(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+FakeKey)
	return r
}
