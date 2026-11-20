package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// The figures the API reads for its metrics (architecture §10). Every time is Postgres's:
// Go passes the window as a duration (rule 7).

// QueueStats are the queue's figures now.
type QueueStats struct {
	// Depth counts the rows of each channel in scheduled, pending, retrying and processing.
	Depth map[domain.Channel]map[domain.Status]int64
	// OldestDue is, per channel, how long its oldest due row has been due; 0 when none is.
	OldestDue map[domain.Channel]time.Duration
}

// depthSQL counts the rows in the four statuses; the claim index and the reaper's index hold
// them, so the rows that left the queue are not read.
const depthSQL = `SELECT channel, status, count(*) FROM notifications
WHERE status IN ('scheduled', 'pending', 'retrying', 'processing')
GROUP BY channel, status`

// oldestDueSQL reads the oldest due row of each channel and priority from the claim index:
// one probe each, in the claim's own order.
const oldestDueSQL = `SELECT c.channel, coalesce(extract(epoch FROM now() - min(o.ready_at)), 0)::float8
FROM unnest($1::text[]) AS c(channel)
CROSS JOIN unnest($2::smallint[]) AS p(priority)
LEFT JOIN LATERAL (
    SELECT ready_at FROM notifications
    WHERE channel = c.channel AND priority = p.priority
      AND status IN ('scheduled', 'pending', 'retrying')
      AND ready_at <= now()
    ORDER BY ready_at
    LIMIT 1) o ON true
GROUP BY c.channel`

// QueueStats reads the queue's depth and the age of its oldest due row.
func (s *Notifications) QueueStats(ctx context.Context) (QueueStats, error) {
	st := QueueStats{Depth: map[domain.Channel]map[domain.Status]int64{}, OldestDue: map[domain.Channel]time.Duration{}}
	for _, ch := range domain.Channels {
		st.Depth[ch] = map[domain.Status]int64{domain.Scheduled: 0, domain.Pending: 0, domain.Retrying: 0, domain.Processing: 0}
		st.OldestDue[ch] = 0
	}
	rows, err := s.pool.Query(ctx, depthSQL)
	if err != nil {
		return QueueStats{}, fmt.Errorf("queue depth: %w", err)
	}
	var ch, status string
	var n int64
	if _, err := pgx.ForEachRow(rows, []any{&ch, &status, &n}, func() error {
		if d, ok := st.Depth[domain.Channel(ch)]; ok {
			d[domain.Status(status)] = n
		}
		return nil
	}); err != nil {
		return QueueStats{}, fmt.Errorf("queue depth: %w", err)
	}
	channels := make([]string, len(domain.Channels))
	for i, c := range domain.Channels {
		channels[i] = string(c)
	}
	priorities := make([]int16, len(domain.Priorities))
	for i, p := range domain.Priorities {
		priorities[i] = int16(p)
	}
	rows, err = s.pool.Query(ctx, oldestDueSQL, channels, priorities)
	if err != nil {
		return QueueStats{}, fmt.Errorf("oldest due row: %w", err)
	}
	var age float64
	if _, err := pgx.ForEachRow(rows, []any{&ch, &age}, func() error {
		st.OldestDue[domain.Channel(ch)] = time.Duration(age * float64(time.Second))
		return nil
	}); err != nil {
		return QueueStats{}, fmt.Errorf("oldest due row: %w", err)
	}
	return st, nil
}

// AttemptStats are one channel's attempts that started within a window.
type AttemptStats struct {
	ByOutcome map[string]int64 // the stored outcome ("error response"), with its count
	Total     int64
	// Duration and Latency are the 50th, 95th and 99th percentiles, in milliseconds, of the
	// attempts' duration and of the delivery latency of the attempts that delivered: from the
	// notification's creation to the end of the attempt. nil when there is none.
	Duration, Latency []float64
}

// attemptsSQL counts the attempts of the window by channel and outcome. The BRIN index on
// started_at finds them; the join to their notification gives the channel.
const attemptsSQL = `SELECT n.channel, a.outcome, count(*)
FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id
WHERE a.started_at >= now() - $1::bigint * interval '1 microsecond'
GROUP BY n.channel, a.outcome`

// percentilesSQL measures the window's attempts per channel: the duration of every attempt,
// and the delivery latency of the attempts that delivered.
const percentilesSQL = `SELECT n.channel,
    percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms),
    percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (
        ORDER BY extract(epoch FROM a.started_at - n.created_at) * 1000 + a.duration_ms)
        FILTER (WHERE a.outcome = 'sent')
FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id
WHERE a.started_at >= now() - $1::bigint * interval '1 microsecond'
GROUP BY n.channel`

// AttemptStats reads the attempts that started within window, per channel; every channel is
// present.
func (s *Notifications) AttemptStats(ctx context.Context, window time.Duration) (map[domain.Channel]*AttemptStats, error) {
	out := map[domain.Channel]*AttemptStats{}
	for _, ch := range domain.Channels {
		out[ch] = &AttemptStats{ByOutcome: map[string]int64{}}
	}
	rows, err := s.pool.Query(ctx, attemptsSQL, window.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("attempts: %w", err)
	}
	var ch, outcome string
	var n int64
	if _, err := pgx.ForEachRow(rows, []any{&ch, &outcome, &n}, func() error {
		if a, ok := out[domain.Channel(ch)]; ok {
			a.ByOutcome[outcome] += n
			a.Total += n
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("attempts: %w", err)
	}
	rows, err = s.pool.Query(ctx, percentilesSQL, window.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("attempt percentiles: %w", err)
	}
	var duration, latency []float64
	if _, err := pgx.ForEachRow(rows, []any{&ch, &duration, &latency}, func() error {
		if a, ok := out[domain.Channel(ch)]; ok {
			a.Duration, a.Latency = duration, latency
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("attempt percentiles: %w", err)
	}
	return out, nil
}

// StatusesOf returns the status and batch of each notification of ids that exists, for a
// WebSocket's current state (architecture §9).
func (s *Notifications) StatusesOf(ctx context.Context, ids []uuid.UUID) ([]Change, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+changeColumns+` FROM notifications WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("statuses: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Change, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("statuses: %w", err)
	}
	return out, nil
}
