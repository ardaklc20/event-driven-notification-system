-- +goose Up
-- The declared order high, normal, low is the order ORDER BY priority uses (K8, K37).
CREATE TYPE notification_priority AS ENUM ('high', 'normal', 'low');
CREATE TYPE notification_status AS ENUM ('queued', 'processing', 'sent', 'failed', 'cancelled');

-- The table is the queue (K2).
CREATE TABLE notifications (
    id               uuid                  PRIMARY KEY DEFAULT uuidv7(),
    recipient        text                  NOT NULL,
    channel          text                  NOT NULL,
    content          text                  NOT NULL,
    priority         notification_priority NOT NULL,
    status           notification_status   NOT NULL DEFAULT 'queued',
    attempts         integer               NOT NULL DEFAULT 0,
    available_at     timestamptz           NOT NULL DEFAULT now(),
    lease_token      uuid,
    lease_expires_at timestamptz,
    last_error       text,
    created_at       timestamptz           NOT NULL DEFAULT now(),
    updated_at       timestamptz           NOT NULL DEFAULT now(),

    CONSTRAINT notifications_channel_check CHECK (channel IN ('sms', 'email', 'push')),
    CONSTRAINT notifications_attempts_check CHECK (attempts >= 0),
    -- Both lease columns are set exactly when the row is processing (K3).
    CONSTRAINT notifications_lease_matches_status CHECK (
        (status = 'processing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'processing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    )
);

-- +goose Down
DROP TABLE notifications;
DROP TYPE notification_status;
DROP TYPE notification_priority;
