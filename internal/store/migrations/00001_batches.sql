-- Architecture §4. Migrations are append-only: never edit a file after it is pushed;
-- add a new one (TestMigrationsAreAppendOnly checks the recorded checksums).

-- +goose Up
CREATE TABLE batches (
    id              uuid        PRIMARY KEY,
    -- The batch's Idempotency-Key header. A separate column from the notification key,
    -- so the two kinds of key cannot collide (architecture §8).
    idempotency_key text        UNIQUE,
    request_hash    bytea,
    total           int         NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
