-- +goose Up
-- When a notification ended: sent or failed (K20). The metrics count the
-- completions of the last 60 seconds by this column; cancellations and
-- retries that are still waiting are not completions.
ALTER TABLE notifications ADD COLUMN completed_at timestamptz;

-- Rows that ended before this migration: without the backfill they would be
-- missing from the first window after it.
UPDATE notifications SET completed_at = sent_at WHERE status = 'sent';
UPDATE notifications SET completed_at = updated_at WHERE status = 'failed';

CREATE INDEX notifications_completed_idx ON notifications (completed_at)
    WHERE completed_at IS NOT NULL;

-- +goose Down
DROP INDEX notifications_completed_idx;
ALTER TABLE notifications DROP COLUMN completed_at;
