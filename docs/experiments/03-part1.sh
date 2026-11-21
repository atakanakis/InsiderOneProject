#!/bin/bash
# Experiment 03: what the API's reads cost as the notifications table grows without retention.
# A window of 1,080,000 attempts (one hour at the full rate of three channels, 300 per second),
# then old rows (sent, with one attempt each, older than the window) added in steps; at each
# step every query below runs three times under EXPLAIN ANALYZE. The window's attempts are
# stamped over the three hours after the start, so every window holds all of them; the 1-hour
# figures and the list's are this part's, the other windows the second part's.
# Run from the repository: ./docs/experiments/03-part1.sh (about 35 minutes; the notifications table alone is 3.5 GB at the end).
set -e
cd "$(git rev-parse --show-toplevel)"
docker rm -f s8pg >/dev/null 2>&1 || true
docker run -d --name s8pg -e POSTGRES_PASSWORD=p -p 55433:5432 postgres:17-alpine >/dev/null
until docker exec s8pg pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done; sleep 2
DATABASE_URL='postgres://postgres:p@localhost:55433/postgres?sslmode=disable' go run ./cmd/migrate
P="docker exec -i s8pg psql -U postgres -q -At"
# The window: 1,080,000 sent rows and their attempts, the attempts spread over the next three
# hours from now, so that all of them stay inside "the last hour" for the whole experiment.
# 1,000 failed rows among them (0.1%), the list's rare filter.
$P <<'SQL'
INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at, sent_at)
SELECT gen_random_uuid(), '+905551234567', (ARRAY['sms','email','push'])[1 + i % 3], 'x', 1,
       CASE WHEN i % 1080 = 0 THEN 'failed' ELSE 'sent' END, 1, 6,
       now(), now() + (i * interval '10 milliseconds'), now()
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
       now() - interval '30 days', now() - interval '2 hours' - (i * interval '1 millisecond') - (SELECT count(*) FROM notifications) * interval '1 millisecond', now()
FROM generate_series(1, $1) i
RETURNING id, created_at)
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT id, 1, created_at + interval '2 seconds', 50, 202, 'sent' FROM n;
VACUUM ANALYZE;
SQL
}
W="now() - 3600000000::bigint * interval '1 microsecond'"
Q_ATT="SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= $W GROUP BY n.channel, a.outcome"
Q_PCT="SELECT n.channel, percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms), percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY extract(epoch FROM a.started_at - n.created_at) * 1000 + a.duration_ms) FILTER (WHERE a.outcome = 'sent') FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id WHERE a.started_at >= $W GROUP BY n.channel"
Q_ATT_PK="SELECT n.channel, a.outcome, count(*) FROM delivery_attempts a CROSS JOIN LATERAL (SELECT channel FROM notifications WHERE id = a.notification_id OFFSET 0) n WHERE a.started_at >= $W GROUP BY n.channel, a.outcome"
Q_PCT_PK="SELECT n.channel, percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY a.duration_ms), percentile_disc(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY extract(epoch FROM a.started_at - n.created_at) * 1000 + a.duration_ms) FILTER (WHERE a.outcome = 'sent') FROM delivery_attempts a CROSS JOIN LATERAL (SELECT channel, created_at FROM notifications WHERE id = a.notification_id OFFSET 0) n WHERE a.started_at >= $W GROUP BY n.channel"
L="SELECT id, batch_id, recipient, channel, priority, status, attempts, created_at, updated_at FROM notifications"
Q_L_FAILED="$L WHERE status = 'failed' ORDER BY created_at DESC, id ASC LIMIT 50"
Q_L_FAILED_SMS="$L WHERE status = 'failed' AND channel = 'sms' ORDER BY created_at DESC, id ASC LIMIT 50"
Q_L_SENT="$L WHERE status = 'sent' ORDER BY created_at DESC, id ASC LIMIT 50"
Q_L_CANCELLED="$L WHERE status = 'cancelled' ORDER BY created_at DESC, id ASC LIMIT 50"
measure() {
  echo "### $($P -c 'SELECT count(*) FROM notifications') notifications, $($P -c 'SELECT count(*) FROM delivery_attempts') attempts, $($P -c "SELECT count(*) FROM delivery_attempts WHERE started_at >= $W") in the window, table $($P -c "SELECT pg_size_pretty(pg_total_relation_size('notifications'))")"
  for name in Q_ATT Q_PCT Q_ATT_PK Q_PCT_PK Q_L_FAILED Q_L_FAILED_SMS Q_L_SENT Q_L_CANCELLED; do
    t=""
    for run in 1 2 3; do
      t="$t $(echo "EXPLAIN (ANALYZE) ${!name}" | $P | grep 'Execution Time' | awk '{print $3}')"
    done
    echo "$name:$t ms"
  done
}
step 920000; measure
step 2000000; measure
step 4000000; measure
step 8000000; measure
step 8000000; measure
docker rm -f s8pg >/dev/null
