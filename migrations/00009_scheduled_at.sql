-- +goose Up
-- When a scheduled notification may first be sent (K26): null when the
-- notification is not scheduled. The first available_at is the same time;
-- retries move only available_at, so scheduled_at keeps the planned time and
-- the metrics can measure delivery from it.
ALTER TABLE notifications ADD COLUMN scheduled_at timestamptz;

-- +goose Down
ALTER TABLE notifications DROP COLUMN scheduled_at;
