-- +goose Up
-- The correlation ID of the request that created the notification (K21).
-- No API wrote notifications before this migration, so the table is empty.
ALTER TABLE notifications ADD COLUMN correlation_id text NOT NULL;

-- One row per Idempotency-Key (K10). The row is inserted first, as the
-- reservation, and completed with the first response in the same transaction.
CREATE TABLE idempotency_keys (
    key             text        PRIMARY KEY,
    operation       text        NOT NULL,
    request_hash    bytea       NOT NULL,
    response_status smallint,
    response_body   bytea,
    notification_id uuid        REFERENCES notifications (id),
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- A second guard behind the header rule.
    CONSTRAINT idempotency_keys_key_length CHECK (octet_length(key) BETWEEN 1 AND 128),
    CONSTRAINT idempotency_keys_operation_check CHECK (operation IN ('create', 'batch')),
    CONSTRAINT idempotency_keys_response_complete CHECK ((response_status IS NULL) = (response_body IS NULL))
);

-- +goose Down
DROP TABLE idempotency_keys;
ALTER TABLE notifications DROP COLUMN correlation_id;
