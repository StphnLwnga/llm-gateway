#!/usr/bin/env bash
# Runs once, on an empty data directory. It creates the login roles;
# migrations create the tables and grant rights on each one.
set -euo pipefail

: "${MIGRATOR_PASSWORD:?MIGRATOR_PASSWORD is required}"
: "${APP_PASSWORD:?APP_PASSWORD is required}"
: "${ADMIN_PASSWORD:?ADMIN_PASSWORD is required}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv migrator_pw MIGRATOR_PASSWORD
\getenv app_pw APP_PASSWORD
\getenv admin_pw ADMIN_PASSWORD

CREATE ROLE gateway_migrator LOGIN BYPASSRLS PASSWORD :'migrator_pw';
CREATE ROLE gateway_app LOGIN PASSWORD :'app_pw';
CREATE ROLE gateway_admin LOGIN BYPASSRLS PASSWORD :'admin_pw';

-- The database owner may create tables in schema public (Postgres 15+).
ALTER DATABASE :"DBNAME" OWNER TO gateway_migrator;

-- pgvector is not a trusted extension: only a superuser can create it.
CREATE EXTENSION IF NOT EXISTS vector;

REVOKE ALL ON DATABASE :"DBNAME" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"DBNAME" TO gateway_migrator, gateway_app, gateway_admin;
GRANT USAGE ON SCHEMA public TO gateway_app, gateway_admin;
SQL