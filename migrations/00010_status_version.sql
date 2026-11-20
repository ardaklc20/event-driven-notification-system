-- +goose Up
-- Real-time status events (K24). version counts the status changes of one
-- notification; row locks serialize the updates to a row, so it only grows.
-- updated_at cannot order events: a transaction can take now() early, wait for
-- the row lock and write an older timestamp.
ALTER TABLE notifications ADD COLUMN version bigint NOT NULL DEFAULT 0;

-- +goose StatementBegin
CREATE FUNCTION notifications_status_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.version := OLD.version + 1;
    ELSE
        NEW.version := OLD.version;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- pg_notify delivers only when the transaction commits, in commit order. The
-- payload is six short fields, far below the 8000-byte limit.
CREATE FUNCTION notifications_status_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('notification_status', json_build_object(
        'notification_id', NEW.id,
        'batch_id', NEW.batch_id,
        'status', NEW.status,
        'attempts', NEW.attempts,
        'version', NEW.version,
        'updated_at', NEW.updated_at
    )::text);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER notifications_status_version
    BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION notifications_status_version();

CREATE TRIGGER notifications_status_notify
    AFTER UPDATE ON notifications
    FOR EACH ROW
    WHEN (NEW.status IS DISTINCT FROM OLD.status)
    EXECUTE FUNCTION notifications_status_notify();

-- +goose Down
DROP TRIGGER notifications_status_notify ON notifications;
DROP TRIGGER notifications_status_version ON notifications;
DROP FUNCTION notifications_status_notify();
DROP FUNCTION notifications_status_version();
ALTER TABLE notifications DROP COLUMN version;
