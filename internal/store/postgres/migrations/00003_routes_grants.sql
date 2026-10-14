-- No default privileges exist, so each table grants its own rights: the
-- gateway reads routes, gatewayctl manages them. The identity column needs no
-- sequence grant, because Postgres does not check sequence rights for it.

-- +goose Up
GRANT SELECT ON routes TO gateway_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON routes TO gateway_admin;

-- +goose Down
REVOKE ALL ON routes FROM gateway_app, gateway_admin;
