-- pgvector backs the semantic cache (S11).

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down
-- No-op: deploy/postgres/initdb/10-roles.sh creates the extension as the
-- superuser, so the migrator does not own it and cannot drop it.
