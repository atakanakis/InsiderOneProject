-- +goose Up
-- One row per finished attempt, written once by Finish (ON CONFLICT DO NOTHING).
CREATE TABLE delivery_attempts (
    notification_id uuid        NOT NULL REFERENCES notifications (id),
    attempt_number  int         NOT NULL,
    started_at      timestamptz NOT NULL,
    duration_ms     int         NOT NULL,
    -- NULL when no response arrived.
    status_code     int,
    outcome         text        NOT NULL,
    error           text,
    PRIMARY KEY (notification_id, attempt_number)
);
