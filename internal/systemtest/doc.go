// Package systemtest holds tests across the API, Redis, Postgres and the worker
// (architecture §9, §10): one correlation id from the request to the provider, one trace from
// the request to the delivery, and a WebSocket event from a real delivery. It has no code of
// its own.
package systemtest
