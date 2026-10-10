-- Extensions only. The first real tables (tenants, api_keys) arrive in S3.
-- pgvector backs the semantic cache in S11.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down
DROP EXTENSION IF EXISTS vector;
