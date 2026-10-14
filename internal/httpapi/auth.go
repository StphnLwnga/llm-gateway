package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// KeyLookup finds the active API key with a hash.
type KeyLookup interface {
	Lookup(ctx context.Context, hash []byte) (tenant.Identity, error)
}

var (
	errMissingKey = errors.New("no Authorization header")
	errNotBearer  = errors.New("authorization scheme is not Bearer")
)

// invalidKeyMessage is the one message for every key failure, so a response
// never tells a caller which check its key failed.
const invalidKeyMessage = "Incorrect API key provided. Send the key in the Authorization header with the Bearer scheme."

// requireKey authenticates every /v1/ request and puts the caller's identity
// in its context. It runs after the mux has matched, so the span and the
// metrics keep the route.
func (h *Handler) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next(w, r)
			return
		}
		id, err := h.authenticate(r)
		if reason := authFailure(err); reason != "" {
			h.log.WarnContext(r.Context(), "authentication failed", "reason", reason, "method", r.Method, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", "Bearer")
			code := "invalid_api_key"
			writeJSON(w, http.StatusUnauthorized, errorEnvelope{Error: errorBody{
				Message: invalidKeyMessage, Type: "invalid_request_error", Code: &code}})
			return
		}
		if err != nil {
			writeError(w, r, h.log, fmt.Errorf("authenticate: %w", err))
			return
		}
		ctx := tenant.WithIdentity(r.Context(), id)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(tenant.SpanAttribute, id.TenantID))
		next(w, r.WithContext(ctx))
	}
}

// authenticate checks the key's format and checksum before it asks the database.
func (h *Handler) authenticate(r *http.Request) (tenant.Identity, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return tenant.Identity{}, errMissingKey
	}
	scheme, key, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return tenant.Identity{}, errNotBearer
	}
	key = strings.TrimSpace(key)
	if err := tenant.ParseKey(key); err != nil {
		return tenant.Identity{}, err
	}
	if h.keys == nil {
		return tenant.Identity{}, tenant.ErrNoActiveKey
	}
	return h.keys.Lookup(r.Context(), tenant.HashKey(key))
}

// authFailure names the reason for the log when err means the key is not
// accepted, and returns "" for success or a failure to check it.
func authFailure(err error) string {
	switch {
	case errors.Is(err, errMissingKey):
		return "missing"
	case errors.Is(err, errNotBearer):
		return "not_bearer"
	case errors.Is(err, tenant.ErrMalformedKey):
		return "malformed"
	case errors.Is(err, tenant.ErrBadChecksum):
		return "bad_checksum"
	case errors.Is(err, tenant.ErrNoActiveKey):
		return "no_active_key"
	}
	return ""
}
