-- +goose Up
-- The W3C trace context of the API request that created the notification
-- (K27): the server span's own trace and span IDs, not the header a client
-- sent. Every worker attempt links to it. Null when tracing is off.
ALTER TABLE notifications
    ADD COLUMN traceparent text,
    ADD CONSTRAINT notifications_traceparent_format
        CHECK (traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$');

-- +goose Down
ALTER TABLE notifications DROP COLUMN traceparent;
