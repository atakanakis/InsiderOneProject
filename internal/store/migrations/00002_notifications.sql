-- +goose Up
CREATE TABLE notifications (
    id                  uuid        PRIMARY KEY,
    batch_id            uuid        REFERENCES batches (id),
    recipient           text        NOT NULL,
    channel             text        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    content             text        NOT NULL,
    -- 0 high, 1 normal, 2 low: the claim takes the smaller number first.
    priority            smallint    NOT NULL CHECK (priority IN (0, 1, 2)),
    status              text        NOT NULL CHECK (status IN (
                            'scheduled', 'pending', 'processing', 'retrying',
                            'sent', 'failed', 'cancelled')),
    -- The number of claims so far. Only ever goes up; it is also the fencing token of a
    -- result (architecture §5).
    attempts            int         NOT NULL DEFAULT 0,
    max_attempts        int         NOT NULL,
    -- Earliest time the row may be claimed. Always computed by Postgres (now() + interval).
    ready_at            timestamptz NOT NULL,
    -- End of the lease while the row is processing.
    locked_until        timestamptz,
    idempotency_key     text        UNIQUE,
    request_hash        bytea,
    correlation_id      text        NOT NULL DEFAULT '',
    traceparent         text        NOT NULL DEFAULT '',
    provider_message_id text,
    last_error          text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz
);

-- The claim: for one channel and priority, due rows in ready_at order. Only waiting rows
-- are in the index, so it does not grow with the history.
CREATE INDEX notifications_claim_idx ON notifications (channel, priority, ready_at)
    WHERE status IN ('scheduled', 'pending', 'retrying');

-- The reaper: processing rows by the end of their lease.
CREATE INDEX notifications_reap_idx ON notifications (locked_until)
    WHERE status = 'processing';

-- The list endpoint (newest first, cursor on created_at and id) and batch queries.
CREATE INDEX notifications_list_idx ON notifications (created_at DESC, id);
CREATE INDEX notifications_batch_idx ON notifications (batch_id);
