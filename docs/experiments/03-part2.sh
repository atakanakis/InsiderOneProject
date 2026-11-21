#!/bin/bash
# Experiment 03, second part: the shipped window queries at 5 minutes, 15 minutes and 1 hour,
# and the same figures without the join, from attempt rows that carry the channel and the
# notification's creation time. A window of 1,080,000 attempts over the hour before T0 (300
# per second), then old rows added in two steps (to 2,000,000 and 16,000,000 notifications);
# at each step every query runs three times under EXPLAIN ANALYZE, measured back from T0.
# Run from the repository: ./docs/experiments/03-part2.sh (about 25 minutes; the notifications table alone is 3.5 GB at the end).
set -e
cd "$(git rev-parse --show-toplevel)"
docker rm -f s8pgb >/dev/null 2>&1 || true
docker run -d --name s8pgb -e POSTGRES_PASSWORD=p -p 55434:5432 postgres:17-alpine >/dev/null
until docker exec s8pgb pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2
DATABASE_URL='postgres://postgres:p@localhost:55434/postgres?sslmode=disable' go run ./cmd/migrate
P="docker exec -i s8pgb psql -U postgres -q -At"
# The window: 1,080,000 sent rows and their attempts over the hour before T0, the start of
# the experiment, 1,000 of them failed. Every query below measures its window back from the
# fixed T0 instead of now(), so that the window's rows do not age out while the old rows are
# added (the first part stamped them in the future, so every window held all of them).
T0=$($P -c "SELECT now()")
echo "T0 = $T0"
$P <<SQL
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1,
       CASE WHEN i % 1080 = 0 THEN 'failed' ELSE 'sent' END, 1, 6,
       '$T0'::timestamptz, '$T0'::timestamptz - interval '1 hour' + (i * interval '3333 microseconds'), '$T0'::timestamptz
FROM generate_series(1, 1080000) i;
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT id, 1, created_at + interval '2 seconds', 20 + (random() * 80)::int, 202, 'sent'
FROM notifications ORDER BY created_at;
SQL
step() { # $1 rows to add, older than the window, each with one attempt
  $P <<SQL
WITH n AS (
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1, 'sent', 1, 6,
       '$T0'::timestamptz - interval '30 days', '$T0'::timestamptz - interval '2 hours' - (i * interval '1 millisecond') - (SELECT count(*) FROM notifications) * interval '1 millisecond', '$T0'::timestamptz
FROM generate_series(1, $1) i
RETURNING id, created_at, channel),
a AS (
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT id, 1, created_at + interval '2 seconds', 50, 202, 'sent' FROM n
RETURNING *)
INSERT INTO attempts_wide SELECT a.*, n.channel, n.created_at FROM a JOIN n ON n.id = a.notification_id;
VACUUM ANALYZE;
SQL
}
mk() { echo "'$T0'::timestamptz - $1::bigint * interval '1 microsecond'"; }
att() { echo "SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= $(mk $1) GROUP BY n.channel, a.outcome"; }
pct() { echo "SELECT n.channel, percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms), percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY extract(epoch FROM a.started_at - n.created_at) * 1000 + a.duration_ms) FILTER (WHERE a.outcome = 'sent') FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= $(mk $1) GROUP BY n.channel"; }
att_nj() { echo "SELECT channel, outcome, count(*) FROM attempts_wide WHERE started_at >= $(mk $1) GROUP BY channel, outcome"; }
pct_nj() { echo "SELECT channel, percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY duration_ms), percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - created_at) * 1000 + duration_ms) FILTER (WHERE outcome = 'sent') FROM attempts_wide WHERE started_at >= $(mk $1) GROUP BY channel"; }
measure() {
  echo "### $($P -c 'SELECT count(*) FROM notifications') notifications, table $($P -c "SELECT pg_size_pretty(pg_total_relation_size('notifications'))")"
  for w in 300000000 900000000 3600000000; do
    echo "window ${w} us: $($P -c "SELECT count(*) FROM delivery_attempts WHERE started_at >= $(mk $w)") attempts"
    for q in att pct att_nj pct_nj; do
      t=""
      for run in 1 2 3; do t="$t $(echo "EXPLAIN (ANALYZE) $($q $w)" | $P | grep 'Execution Time' | awk '{print $3}')"; done
      echo "  $q:$t ms"
    done
  done
}
# The attempt rows as they would be if each carried its notification's channel and creation
# time: a copy with a BRIN index on started_at, as delivery_attempts has.
$P <<'SQL'
CREATE TABLE attempts_wide AS SELECT a.*, n.channel, n.created_at FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id ORDER BY a.started_at;
CREATE INDEX ON attempts_wide USING brin (started_at);
SQL
step 920000; measure
step 14000000; measure
docker rm -f s8pgb >/dev/null
