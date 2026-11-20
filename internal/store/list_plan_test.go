package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// architecture §8, "List": a page after a cursor reads the list index from the cursor, not
// from its start. Written as a filter alone, the cursor made Postgres read and drop every
// row before it: a page 1.9 million rows deep took 300 ms (session 6's review, F3). On
// 200,000 rows, a page 150,000 deep must drop almost nothing.
func TestAListPageReadsTheIndexFromItsCursor(t *testing.T) {
	e := testkit.Setup(t)
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := e.Pool.Exec(c, `INSERT INTO notifications (id, recipient, channel, content, priority,
		status, max_attempts, ready_at, created_at)
		SELECT gen_random_uuid(), '+905551234567', 'sms', 'x', 1, 'sent', 6, now(),
			'2026-01-01'::timestamptz + make_interval(secs => i)
		FROM generate_series(1, 200000) AS i`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(c, `ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	var id uuid.UUID
	if err := e.Pool.QueryRow(c, `SELECT created_at, id FROM notifications
		ORDER BY created_at DESC, id ASC OFFSET 150000 LIMIT 1`).Scan(&at, &id); err != nil {
		t.Fatal(err)
	}
	sql, args := store.ListSQL(store.Filter{After: &store.Cursor{CreatedAt: at, ID: id}, Limit: 50})
	var raw []byte
	if err := e.Pool.QueryRow(c, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Plan          planNode `json:"Plan"`
		ExecutionTime float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	var usedIndex bool
	var removed float64
	plan[0].Plan.walk(func(n planNode) {
		usedIndex = usedIndex || n.IndexName == "notifications_list_idx"
		removed += n.RowsRemovedByFilter
	})
	t.Logf("a page 150,000 rows deep: %.0f rows read and dropped, %.2f ms", removed, plan[0].ExecutionTime)
	if !usedIndex || removed > 1000 {
		t.Errorf("the list index used: %v; %.0f rows read and dropped before the page; plan: %s", usedIndex, removed, raw)
	}
}
