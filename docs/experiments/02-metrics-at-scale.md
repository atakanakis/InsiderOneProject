# Experiment 02: the API's reads from Postgres on a large queue

Session 7, 2026-11-20. What the figures of `/metrics` and `GET /v1/metrics` (architecture §10)
cost when the tables are large: 1,000,000 waiting rows (pending, retrying, scheduled; due and
not due), 1,080,000 sent rows, and one attempt for each sent row, spread over the last hour
(300 per second, the full rate of three channels). Postgres 17 in Docker on the author's
machine (Apple silicon, 16 GB); three runs of each query, `EXPLAIN (ANALYZE)`.

| Query | Time |
|---|---|
| Queue depth by channel and status (`store.depthSQL`) | 120.8 to 122.3 ms |
| Oldest due row per channel (`store.oldestDueSQL`, nine index-only probes) | 0.09 to 0.15 ms |
| Attempts of 5 minutes by channel and outcome, hash join (as shipped) | 105.7 to 110.5 ms |
| Percentiles of 5 minutes, hash join (as shipped) | 143.0 to 163.2 ms |
| Attempts of 1 hour, hash join (as shipped) | 307.5 to 336.8 ms |
| Percentiles of 1 hour, hash join (as shipped) | 911.8 to 1,033.3 ms |
| Attempts of 5 minutes, joined by primary key (`LATERAL ... OFFSET 0`) | 216.2 to 291.3 ms |
| Percentiles of 5 minutes, joined by primary key | 265.8 to 275.3 ms |
| Attempts of 1 hour, joined by primary key | 1,621.1 to 1,672.9 ms |
| Percentiles of 1 hour, joined by primary key | 2,168.6 to 2,214.2 ms |

The window's attempts are found through the BRIN index (`Bitmap Index Scan on
delivery_attempts_started_at`, 8,780 rows for 5 minutes); the hash join then reads every
notification (`Parallel Seq Scan on notifications`), so its cost grows with the table. The
join by primary key is bounded by the window but was slower at this size. Kept as shipped;
the class is the missing retention (architecture §16).

## The commands

The first script builds the tables and prints each plan's lines that name a scan and the
execution time; the second ran every query above three times and printed the times. Both
remove their container.

```sh
#!/bin/bash
# Measures the API's reads from Postgres on a large queue: 1,000,000 waiting rows,
# 1,000,000 sent rows and 1,080,000 attempts within the last hour (300 per second for 1 h).
set -e
cd "$(git rev-parse --show-toplevel)"
docker rm -f s7pg >/dev/null 2>&1 || true
docker run -d --name s7pg -e POSTGRES_PASSWORD=p -p 55432:5432 postgres:17-alpine >/dev/null
until docker exec s7pg pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2
DATABASE_URL='postgres://postgres:p@localhost:55432/postgres?sslmode=disable' go run ./cmd/migrate
P="docker exec -i s7pg psql -U postgres -q"
$P <<'SQL'
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', i % 3,
       (ARRAY['pending','pending','retrying','scheduled'])[1 + i % 4], 0, 6,
       now() + ((i % 4) - 2) * interval '1 minute'
FROM generate_series(1, 1000000) i;
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1, 'sent', 1, 6,
       now() - interval '2 hours', now() - (i * interval '3333 microseconds') - interval '2 seconds', now()
FROM generate_series(1, 1080000) i;
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT id, 1, created_at + interval '2 seconds', 20 + (random() * 80)::int, 202, 'sent'
FROM notifications WHERE status = 'sent' ORDER BY created_at;
VACUUM ANALYZE;
SQL
for q in \
 "SELECT channel, status, count(*) FROM notifications WHERE status IN ('scheduled', 'pending', 'retrying', 'processing') GROUP BY channel, status" \
 "SELECT c.channel, coalesce(extract(epoch FROM now() - min(o.ready_at)), 0)::float8 FROM unnest(ARRAY['sms','email','push']) AS c(channel) CROSS JOIN unnest(ARRAY[0,1,2]::smallint[]) AS p(priority) LEFT JOIN LATERAL (SELECT ready_at FROM notifications WHERE channel = c.channel AND priority = p.priority AND status IN ('scheduled', 'pending', 'retrying') AND ready_at <= now() ORDER BY ready_at LIMIT 1) o ON true GROUP BY c.channel" \
 "SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= now() - interval '5 minutes' GROUP BY n.channel, a.outcome" \
 "SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= now() - interval '1 hour' GROUP BY n.channel, a.outcome" \
 "SELECT n.channel, percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms), percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY extract(epoch FROM a.started_at - n.created_at) * 1000 + a.duration_ms) FILTER (WHERE a.outcome = 'sent') FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= now() - interval '1 hour' GROUP BY n.channel" \
 ; do
  echo "== $q" | cut -c1-120
  for run in 1 2 3; do echo "EXPLAIN (ANALYZE, BUFFERS) $q" | $P | grep -E 'Execution Time|brin|Seq Scan|Index' | head -4; done
done
docker rm -f s7pg >/dev/null
```

```sh
# measure2.sh: the same setup, then for each query of the table:
#   for run in 1 2 3; do echo "EXPLAIN (ANALYZE) <query>" | $P | grep 'Execution Time'; done
# The join by primary key, for the 1-hour window:
SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a
CROSS JOIN LATERAL (SELECT channel FROM notifications WHERE id = a.notification_id OFFSET 0) n
WHERE a.started_at >= now() - interval '1 hour' GROUP BY n.channel, a.outcome;
```
