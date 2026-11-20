-- +goose Up
-- The provider's circuit breaker, shared by every worker (K34). One row: the
-- key is a boolean that can only be true.
--
-- generation goes up each time the breaker opens; a result counts only for
-- the generation its claim read. consecutive_failures counts outage failures
-- in a row within the current generation. open_until is null while closed.
-- cooldown is the current pause; the worker sets it to BREAKER_COOLDOWN when
-- the breaker opens from closed and doubles it on each reopening, up to
-- BREAKER_MAX_COOLDOWN. probe_token and probe_expires_at hold the one probe
-- after the cooldown: the probe's lease token and its lease end.
CREATE TABLE provider_breaker (
    id                   boolean PRIMARY KEY DEFAULT true,
    generation           bigint      NOT NULL DEFAULT 0,
    consecutive_failures integer     NOT NULL DEFAULT 0,
    open_until           timestamptz,
    cooldown             interval    NOT NULL DEFAULT interval '30 seconds',
    probe_token          uuid,
    probe_expires_at     timestamptz,
    CONSTRAINT provider_breaker_single_row CHECK (id),
    CONSTRAINT provider_breaker_failures_check CHECK (consecutive_failures >= 0),
    CONSTRAINT provider_breaker_cooldown_check CHECK (cooldown > interval '0'),
    CONSTRAINT provider_breaker_probe_complete CHECK ((probe_token IS NULL) = (probe_expires_at IS NULL)),
    CONSTRAINT provider_breaker_probe_only_open CHECK (probe_token IS NULL OR open_until IS NOT NULL)
);

INSERT INTO provider_breaker DEFAULT VALUES;

-- +goose Down
DROP TABLE provider_breaker;
