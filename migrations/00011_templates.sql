-- +goose Up
-- Stored message templates (K33, K47). A template never changes: the code
-- has no UPDATE or DELETE for this table, and a changed message is a new
-- template. Names are not unique; created_at tells templates apart.
CREATE TABLE templates (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    name       text        NOT NULL,
    channel    text        NOT NULL,
    body       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT templates_channel_check CHECK (channel IN ('sms', 'email', 'push')),
    CONSTRAINT templates_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT templates_body_length CHECK (char_length(body) BETWEEN 1 AND 10000)
);

-- GET /templates pages newest first with the K13 cursor.
CREATE INDEX templates_list_idx ON templates (created_at DESC, id DESC);

-- The template a notification's content was rendered from, or null. The
-- variables are not stored: the template cannot change, so its ID and the
-- rendered content are enough (K33).
ALTER TABLE notifications ADD COLUMN template_id uuid REFERENCES templates (id);

-- +goose Down
ALTER TABLE notifications DROP COLUMN template_id;
DROP TABLE templates;
