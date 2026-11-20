-- +goose Up
-- One row per saved batch (K12). A batch is written in one transaction with
-- all its notifications, so a batch row never exists without its items.
CREATE TABLE batches (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    item_count     integer     NOT NULL,
    correlation_id text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT batches_item_count_check CHECK (item_count BETWEEN 1 AND 1000)
);

-- Both null for a single create. batch_position is the item's 0-based index
-- in the request, so the 202 response can list the IDs in request order:
-- PostgreSQL documents no order for the rows RETURNING gives back.
ALTER TABLE notifications
    ADD COLUMN batch_id       uuid    REFERENCES batches (id),
    ADD COLUMN batch_position integer,
    ADD CONSTRAINT notifications_batch_position_unique UNIQUE (batch_id, batch_position),
    ADD CONSTRAINT notifications_batch_fields_together CHECK ((batch_id IS NULL) = (batch_position IS NULL));

-- A batch's items in the listing order (K13); single creates are not indexed.
CREATE INDEX notifications_batch_idx ON notifications (batch_id, created_at DESC, id DESC)
    WHERE batch_id IS NOT NULL;

-- +goose Down
DROP INDEX notifications_batch_idx;
ALTER TABLE notifications
    DROP CONSTRAINT notifications_batch_fields_together,
    DROP CONSTRAINT notifications_batch_position_unique,
    DROP COLUMN batch_position,
    DROP COLUMN batch_id;
DROP TABLE batches;
