# Experiment 03: the API's reads as the tables grow without retention

Session 8, 2026-11-21. Nothing removes old notifications or attempts (architecture §16).
Experiment 02 measured the reads of `GET /v1/metrics` on 2,080,000 notifications; this one
asks at what size they reach the API's database deadline of 5 s, and what the list's filters
cost at the same sizes. Postgres 17 in Docker on the author's machine (Apple silicon, 16 GB,
8 CPUs for Docker), default settings; three runs of each query, `EXPLAIN (ANALYZE)`.

The tables: 1,080,000 notifications with one attempt each inside the window (one hour at the
full rate of three channels, 300 per second; 1,000 of them `failed`, the rest `sent`), then old
rows, `sent` with one attempt each, added in steps. The window's attempts were stamped over
the three hours after the start, so all of them stayed inside "the last hour" for the whole
run.

## The 1-hour window and the list (first part)

Times in milliseconds, three runs each.

| Notifications | Table | Window counts (`attemptsSQL`) | Window percentiles (`percentilesSQL`) | Counts, joined by primary key | Percentiles, joined by primary key |
|---|---|---|---|---|---|
| 2,000,000 | 467 MB | 339 to 391 | 1,005 to 1,767 | 1,721 to 2,423 | 2,287 to 2,634 |
| 4,000,000 | 903 MB | 659 to 822 | 1,420 to 1,723 | 2,435 to 2,628 | 2,975 to 3,075 |
| 8,000,000 | 1,774 MB | 845 to 1,098 | 1,628 to 2,374 | 3,442 to 3,652 | 3,991 to 4,954 |
| 16,000,000 | 3,517 MB | 1,665 to 2,636 | 2,315 to 2,692 | 4,150 to 4,525 | 4,881 to 5,448 |

`GET /v1/metrics` runs the two window statements one after the other in one read, which has
the 5 s deadline (`internal/api/metrics.go`). At 16,000,000 notifications the two take 4.0 to
5.3 s together: the 1-hour window reaches its deadline there, and answers 500. At the full
rate of all three channels that is about 15 hours of traffic; at a tenth of it, about six
days. The join by primary key, which is bounded by the window instead of the table, is slower
at every size measured.

| Notifications | `status=failed` (1,000 rows) | `status=failed&channel=sms` | `status=sent` | `status=cancelled` (no row) |
|---|---|---|---|---|
| 2,000,000 | 6.0 to 6.8 | 9.8 to 11.2 | 0.04 to 0.06 | 50 to 55 |
| 4,000,000 | 10.0 to 14.9 | 9.6 to 9.9 | 0.04 | 94 to 106 |
| 8,000,000 | 14.5 to 21.8 | 10.0 to 16.6 | 0.04 to 0.06 | 280 to 290 |
| 16,000,000 | 37.5 to 112.4 | 36.3 to 39.4 | 0.04 | 1,629 to 3,209 |

The list reads the list index in its order and filters. A filter that most rows match fills a
page at once; a filter that few rows match reads further, and one that no row matches reads
the whole index: 1.6 to 3.2 s at 16,000,000 rows.

## Every window, and the same figures without the join (second part)

The first part stamped the window's attempts after its start, so that they would not age out
while old rows were added; that put all 1,080,000 inside every window. The second part
stamped them over the hour before a fixed time `T0`, the start of the run, and measured each
window back from `T0` instead of `now()`, with 90,502, 270,520 and 1,080,000 attempts in the
5-minute, 15-minute and 1-hour windows. It also kept a copy of the attempts that carries
each notification's channel and creation time (`attempts_wide`, with the same BRIN index on
`started_at`), to measure the figures with no join.

Times in milliseconds, three runs each; "with the join" is the shipped `attemptsSQL` and
`percentilesSQL`, "without" the same figures from `attempts_wide`.

| Notifications | Window | Counts, with the join | Percentiles, with the join | Counts, without | Percentiles, without |
|---|---|---|---|---|---|
| 2,000,000 | 5 min | 90 to 184 | 138 to 208 | 8.4 to 11.1 | 50 to 52 |
| 2,000,000 | 15 min | 147 to 151 | 278 to 313 | 19 to 23 | 147 to 149 |
| 2,000,000 | 1 h | 277 to 286 | 831 to 872 | 70 to 86 | 705 to 725 |
| 16,000,000 | 5 min | 158 to 301 | 211 to 213 | 35 to 48 | 63 to 65 |
| 16,000,000 | 15 min | 1,294 to 2,054 | 1,143 to 1,542 | 41 to 54 | 148 to 151 |
| 16,000,000 | 1 h | 1,873 to 2,073 | 1,966 to 2,276 | 93 to 319 | 595 to 613 |

At 16,000,000 notifications the 5-minute window, the default, takes 0.37 to 0.51 s for its
two statements; the 15-minute window 2.4 to 3.6 s; the 1-hour window 3.8 to 4.4 s here and
4.0 to 5.3 s in the first part. Without the join, the figures cost what the window holds,
whatever the size of the table: at most 0.93 s for the hour.

## What was decided

Kept as shipped, with the limit stated (architecture §10, §16). The cost grows with the table
because nothing removes old rows, and that is the class: the list's filters grow with it too
(above), and so does the disk. A retention policy bounds all of them; it is left out because
the requirements give no period for which a status must stay readable. Storing the channel
and the creation time on each attempt row would bound `GET /v1/metrics` alone, at the price
of a migration and of a change to Finish's insert, which every result goes through; the
second part measured what it would buy. It is the change to make if the service must run at
the full rate for more than about half a day without retention.

## The commands

The scripts start their own Postgres container on a spare port and remove it at the end.
`exp03.sh` is the first part; `exp03b.sh` the second. Both build the window, then add old rows
with one `INSERT ... RETURNING` into the attempts, `VACUUM ANALYZE`, and run each query three
times:

```sh
#!/bin/bash
# The core of both scripts (the full scripts add the timing loop around these statements).
set -e
cd "$(git rev-parse --show-toplevel)"
docker run -d --name s8pg -e POSTGRES_PASSWORD=p -p 55433:5432 postgres:17-alpine
until docker exec s8pg pg_isready -h 127.0.0.1 -U postgres; do sleep 1; done; sleep 2
DATABASE_URL='postgres://postgres:p@localhost:55433/postgres?sslmode=disable' go run ./cmd/migrate
P="docker exec -i s8pg psql -U postgres -q -At"
T0=$($P -c "SELECT now()")
# The window: one hour at 300 per second before T0, 1,000 of the rows failed.
$P <<SQL
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1,
       CASE WHEN i % 1080 = 0 THEN 'failed' ELSE 'sent' END, 1, 6,
       '$T0', '$T0'::timestamptz - interval '1 hour' + i * interval '3333 microseconds', '$T0'
FROM generate_series(1, 1080000) i;
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT id, 1, created_at + interval '2 seconds', 20 + (random() * 80)::int, 202, 'sent' FROM notifications ORDER BY created_at;
CREATE TABLE attempts_wide AS SELECT a.*, n.channel, n.created_at FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id ORDER BY a.started_at;
CREATE INDEX ON attempts_wide USING brin (started_at);
SQL
# Old rows, older than the window, each with its attempt (repeated with 920,000, then 14,000,000).
$P <<SQL
WITH n AS (
  INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
  SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1, 'sent', 1, 6,
         '$T0'::timestamptz - interval '30 days', '$T0'::timestamptz - interval '2 hours' - i * interval '1 millisecond', '$T0'
  FROM generate_series(1, 14000000) i RETURNING id, created_at, channel),
a AS (
  INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
  SELECT id, 1, created_at + interval '2 seconds', 50, 202, 'sent' FROM n RETURNING *)
INSERT INTO attempts_wide SELECT a.*, n.channel, n.created_at FROM a JOIN n ON n.id = a.notification_id;
VACUUM ANALYZE;
SQL
# Each query, three times; W is "'$T0'::timestamptz - <window in µs>::bigint * interval '1 microsecond'"
# (the first part used now() in its place, as the code does):
#   EXPLAIN (ANALYZE) SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a
#     JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= W GROUP BY n.channel, a.outcome;
#   the same for percentilesSQL (internal/store/stats.go); without the join:
#   EXPLAIN (ANALYZE) SELECT channel, outcome, count(*) FROM attempts_wide WHERE started_at >= W GROUP BY channel, outcome;
#   the list: SELECT id, batch_id, recipient, channel, priority, status, attempts, created_at, updated_at
#     FROM notifications WHERE status = 'cancelled' ORDER BY created_at DESC, id ASC LIMIT 50;
docker rm -f s8pg
```

The first part used `now()` for the window and the old rows' times, and stamped the window's
attempts over the three hours after its start; in the first part the old rows were added in
steps of 920,000, 2,000,000, 4,000,000, 8,000,000 and 8,000,000.
