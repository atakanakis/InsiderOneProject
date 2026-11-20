-- +goose Up
-- The attempts of a recent window, for GET /v1/metrics (architecture §10). Attempt rows are
-- only inserted, in time order, so a BRIN index is a few pages and costs the queue nothing.
CREATE INDEX delivery_attempts_started_at ON delivery_attempts USING brin (started_at);
