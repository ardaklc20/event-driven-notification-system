-- +goose Up
-- One permit row per channel, shared by every worker (K5, K42). A claim locks
-- its channel's row, takes the permit when next_permit_at has come and moves
-- it PERMIT_INTERVAL ahead. The first permit of each channel is due at once.
CREATE TABLE channel_limits (
    channel        text        PRIMARY KEY,
    next_permit_at timestamptz NOT NULL,

    CONSTRAINT channel_limits_channel_check CHECK (channel IN ('sms', 'email', 'push'))
);

INSERT INTO channel_limits (channel, next_permit_at)
VALUES ('sms', now()), ('email', now()), ('push', now());

-- The claim's order within a channel: priority as the enum declares it
-- (high, normal, low), then available_at, then id (K8, K37). Only queued rows
-- are ever claimed, so only they are indexed.
CREATE INDEX notifications_claim_idx
    ON notifications (channel, priority, available_at, id)
    WHERE status = 'queued';

-- +goose Down
DROP INDEX notifications_claim_idx;
DROP TABLE channel_limits;
