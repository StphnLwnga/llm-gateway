-- FORCE makes the policies apply to the table owner as well. Roles with
-- BYPASSRLS (the migrator and gatewayctl's admin role) still see every row.
-- nullif turns the '' that a finished set_config(..., true) leaves behind into
-- NULL, so a connection with no tenant matches no row instead of failing.

-- +goose Up
CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    name        text NOT NULL UNIQUE CHECK (name <> ''),
    created_at  timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz
);

CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id  uuid NOT NULL REFERENCES tenants,
    prefix     text NOT NULL,
    hash       bytea NOT NULL UNIQUE CHECK (octet_length(hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX api_keys_tenant_id ON api_keys (tenant_id);

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
    USING (id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_keys
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The key is checked before any tenant is known, so the lookup runs as its
-- owner, the migrator, whose BYPASSRLS sees every key. The empty search_path
-- stops a caller's objects from shadowing the tables it reads.
-- +goose StatementBegin
CREATE FUNCTION auth_lookup(p_hash bytea)
RETURNS TABLE (tenant_id uuid, key_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = ''
AS $$
    SELECT k.tenant_id, k.id
    FROM public.api_keys k
    JOIN public.tenants t ON t.id = k.tenant_id
    WHERE k.hash = p_hash AND k.revoked_at IS NULL AND t.disabled_at IS NULL
$$;
-- +goose StatementEnd

-- Functions are executable by PUBLIC by default.
REVOKE EXECUTE ON FUNCTION auth_lookup(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup(bytea) TO gateway_app;

-- No DELETE: a disabled tenant or a revoked key stays for audit.
GRANT SELECT, INSERT, UPDATE ON tenants, api_keys TO gateway_admin;

-- +goose Down
DROP FUNCTION auth_lookup(bytea);
DROP TABLE api_keys;
DROP TABLE tenants;
