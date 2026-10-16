-- The gateway role has no rights on api_keys, so it records use through a
-- SECURITY DEFINER function, as auth_lookup does. Two gateways can flush the
-- same key; the condition keeps a flush that started earlier from writing an
-- older time over a newer one.

-- +goose Up
ALTER TABLE api_keys ADD COLUMN last_used_at timestamptz;

-- +goose StatementBegin
CREATE FUNCTION touch_keys(p_ids uuid[])
RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = ''
AS $$
    UPDATE public.api_keys
    SET last_used_at = now()
    WHERE id = ANY (p_ids) AND (last_used_at IS NULL OR last_used_at < now())
$$;
-- +goose StatementEnd

REVOKE EXECUTE ON FUNCTION touch_keys(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION touch_keys(uuid[]) TO gateway_app;

-- +goose Down
DROP FUNCTION touch_keys(uuid[]);
ALTER TABLE api_keys DROP COLUMN last_used_at;
