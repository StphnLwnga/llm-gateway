package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// Not-found errors for the admin commands.
var (
	ErrTenantNotFound = errors.New("tenant not found")
	ErrKeyNotFound    = errors.New("API key not found")
)

// ErrTenantDisabled is returned when a key is requested for a disabled tenant.
var ErrTenantDisabled = errors.New("a key for it could never authenticate")

// Tenant is one row of the tenants table.
type Tenant struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// APIKey is one row of the api_keys table, without its hash.
type APIKey struct {
	ID        string
	TenantID  string
	Prefix    string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Tenants manages tenants and their keys. It needs the admin role.
type Tenants struct {
	Pool *pgxpool.Pool
}

// Create adds a tenant and returns its ID.
func (s Tenants) Create(ctx context.Context, name string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ($1) RETURNING id::text", name).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return "", fmt.Errorf("tenant %q already exists: %w", name, err)
		}
		return "", fmt.Errorf("insert tenant: %w", err)
	}
	return id, nil
}

// List returns every tenant, oldest first.
func (s Tenants) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id::text, name, created_at, disabled_at FROM tenants ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		return t, row.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.DisabledAt)
	})
	if err != nil {
		return nil, fmt.Errorf("read tenants: %w", err)
	}
	return out, nil
}

// Disable stops every key of the tenant from authenticating. The first disable time is kept.
func (s Tenants) Disable(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE tenants SET disabled_at = coalesce(disabled_at, now()) WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("disable tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %s", ErrTenantNotFound, id)
	}
	return nil
}

// CreateKey stores a new key for the tenant and returns the key in full, once.
func (s Tenants) CreateKey(ctx context.Context, tenantID string) (APIKey, string, error) {
	key, err := tenant.NewKey()
	if err != nil {
		return APIKey{}, "", fmt.Errorf("generate key: %w", err)
	}
	k := APIKey{TenantID: tenantID, Prefix: tenant.DisplayPrefix(key)}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// FOR SHARE makes a disable running at the same time wait for this
		// transaction, or this one wait for it and then see disabled_at.
		var disabled bool
		err := tx.QueryRow(ctx, "SELECT disabled_at IS NOT NULL FROM tenants WHERE id = $1 FOR SHARE", tenantID).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: id %s", ErrTenantNotFound, tenantID)
		}
		if err != nil {
			return fmt.Errorf("check tenant: %w", err)
		}
		if disabled {
			return fmt.Errorf("tenant %s is disabled; %w", tenantID, ErrTenantDisabled)
		}
		err = tx.QueryRow(ctx, "INSERT INTO api_keys (tenant_id, prefix, hash) VALUES ($1, $2, $3) RETURNING id::text, created_at",
			tenantID, k.Prefix, tenant.HashKey(key)).Scan(&k.ID, &k.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert key: %w", err)
		}
		return nil
	})
	if err != nil {
		return APIKey{}, "", err
	}
	return k, key, nil
}

// ListKeys returns the tenant's keys, oldest first.
func (s Tenants) ListKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text, tenant_id::text, prefix, created_at, revoked_at
		FROM api_keys WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query keys: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		return k, row.Scan(&k.ID, &k.TenantID, &k.Prefix, &k.CreatedAt, &k.RevokedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("read keys: %w", err)
	}
	return out, nil
}

// RevokeKey stops the key from authenticating. The first revoke time is kept.
func (s Tenants) RevokeKey(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %s", ErrKeyNotFound, id)
	}
	return nil
}

// Keys authenticates API keys for the gateway.
type Keys struct {
	Pool *pgxpool.Pool
}

// Lookup returns the identity of the active key with this hash, or tenant.ErrNoActiveKey.
func (k Keys) Lookup(ctx context.Context, hash []byte) (tenant.Identity, error) {
	var id tenant.Identity
	err := k.Pool.QueryRow(ctx, "SELECT tenant_id::text, key_id::text FROM auth_lookup($1)", hash).Scan(&id.TenantID, &id.KeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.Identity{}, tenant.ErrNoActiveKey
	}
	if err != nil {
		return tenant.Identity{}, fmt.Errorf("look up key: %w", err)
	}
	return id, nil
}

// InTenant runs fn in a transaction where row-level security shows only the
// tenant's rows. The setting ends with the transaction, so a pooled
// connection never carries it into another request.
func InTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			return fmt.Errorf("set tenant: %w", err)
		}
		return fn(tx)
	})
}
