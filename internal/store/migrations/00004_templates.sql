-- +goose Up
CREATE TABLE templates (
    name       text        PRIMARY KEY,
    channel    text        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    body       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
