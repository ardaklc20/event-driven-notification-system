-- +goose Up
-- The listing order: newest first, the ID breaking ties (K13).
CREATE INDEX notifications_list_idx ON notifications (created_at DESC, id DESC);

-- +goose Down
DROP INDEX notifications_list_idx;
