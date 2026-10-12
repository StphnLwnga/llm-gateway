-- Prices are numeric, never float, so every amount is exact.

-- +goose Up
CREATE TABLE routes (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alias               text NOT NULL CHECK (alias <> ''),
    provider            text NOT NULL CHECK (provider IN ('groq', 'openai', 'ollama')),
    upstream_model      text NOT NULL CHECK (upstream_model <> ''),
    priority            integer NOT NULL,
    enabled             boolean NOT NULL DEFAULT true,
    input_usd_per_mtok  numeric(14, 6) NOT NULL DEFAULT 0 CHECK (input_usd_per_mtok >= 0),
    output_usd_per_mtok numeric(14, 6) NOT NULL DEFAULT 0 CHECK (output_usd_per_mtok >= 0),
    default_max_tokens  integer CHECK (default_max_tokens > 0),
    max_max_tokens      integer CHECK (max_max_tokens > 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (default_max_tokens IS NULL OR max_max_tokens IS NULL OR max_max_tokens >= default_max_tokens),
    UNIQUE (alias, priority),
    UNIQUE (alias, provider, upstream_model)
);

-- +goose Down
DROP TABLE routes;
