-- +goose Up
-- What the provider answered with its 202 (K15). provider_message_id is not
-- unique: webhook.site's static response gives every notification the same ID.
-- sent_at is the time we recorded the acceptance; provider_timestamp is the
-- time the provider reported.
ALTER TABLE notifications
    ADD COLUMN provider_message_id text,
    ADD COLUMN provider_timestamp  timestamptz,
    ADD COLUMN sent_at             timestamptz;

-- +goose Down
ALTER TABLE notifications
    DROP COLUMN sent_at,
    DROP COLUMN provider_timestamp,
    DROP COLUMN provider_message_id;
