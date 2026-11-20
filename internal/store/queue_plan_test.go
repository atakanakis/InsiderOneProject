package store_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"


	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// planNode is one node of EXPLAIN (FORMAT JSON).
type planNode struct {
	NodeType     string     `json:"Node Type"`
	IndexName    string     `json:"Index Name"`
	RelationName string     `json:"Relation Name"`
	ActualRows   float64    `json:"Actual Rows"`
	SharedHit    int        `json:"Shared Hit Blocks"`
	SharedRead   int        `json:"Shared Read Blocks"`
	Plans        []planNode `json:"Plans"`
}

func (n planNode) walk(f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		c.walk(f)
	}
}

// Assumption A3: the claim reads the claim index in order and stops after n rows: no sort,
// no full scan, no bitmap scan (which loses the order), under a custom plan and under a
// generic plan (the plan a cached prepared statement can switch to). The table holds many
// rows that share one ready_at and many rows that are not due yet, the two cases that could
// push the planner away from the index. Both claims, one that finds 10 rows among 50,000
// tied due rows and one that finds nothing above 100,000 future rows, must read only a few
// pages of the index.
func TestClaimPlanReadsTheIndexInOrder(t *testing.T) {
	e := testkit.Setup(t)
	c := ctx(t)
	if _, err := e.Pool.Exec(c, `
		INSERT INTO notifications (id, recipient, channel, content, priority, status, max_attempts, ready_at)
		SELECT gen_random_uuid(), 'r', 'sms', 'c', p, s, 6, r
		FROM (VALUES
		        -- 50,000 due high-priority rows, all with the same ready_at
		        (0, 'pending',   now() - interval '1 minute',  50000),
		        -- 100,000 high-priority rows that are not due yet, spread over a day
		        (0, 'scheduled', NULL,                          100000),
		        -- 100,000 low-priority rows that are not due yet, all at one time
		        (2, 'scheduled', now() + interval '1 hour',    100000),
		        -- 50,000 finished rows, which are not in the claim index
		        (0, 'sent',      now() - interval '1 hour',    50000)
		     ) AS k(p, s, at, n),
		     LATERAL (SELECT coalesce(at, now() + i * interval '1 second') AS r
		              FROM generate_series(1, n) AS i) AS g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.Pool.Exec(c, `ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}

	conn, err := e.Pool.Acquire(c)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(c, `PREPARE claim AS `+store.ClaimSQL); err != nil {
		t.Fatalf("prepare the claim: %v", err)
	}
	defer func() { _, _ = conn.Exec(c, `DEALLOCATE claim`) }()

	type claimCase struct {
		name      string
		priority  int
		wantRows  int
		maxBlocks int // pages the index scan may touch
	}
	cases := []claimCase{
		{"10 of 50,000 tied due rows", 0, 10, 20},
		{"nothing due above 100,000 future rows", 2, 0, 10},
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		if _, err := conn.Exec(c, `SET plan_cache_mode = `+mode); err != nil {
			t.Fatal(err)
		}
		for _, cc := range cases {
			name := mode + ": " + cc.name
			// EXPLAIN ANALYZE runs the update; the transaction is rolled back.
			tx, err := conn.Begin(c)
			if err != nil {
				t.Fatal(err)
			}
			var raw string
			err = tx.QueryRow(c, fmt.Sprintf(`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE claim('sms', %d, 10, 60000000)`, cc.priority)).Scan(&raw)
			_ = tx.Rollback(c)
			if err != nil {
				t.Fatalf("%s: explain: %v", name, err)
			}
			checkClaimPlan(t, name, raw, cc.wantRows, cc.maxBlocks)
		}
	}
	if _, err := conn.Exec(c, `RESET plan_cache_mode`); err != nil {
		t.Fatal(err)
	}
}

func checkClaimPlan(t *testing.T, name, raw string, wantRows, maxBlocks int) {
	t.Helper()
	var out []struct {
		Plan          planNode `json:"Plan"`
		ExecutionTime float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) != 1 {
		t.Fatalf("%s: read the plan: %v", name, err)
	}
	var (
		bad        []string
		claimScans []planNode
		updated    float64
	)
	out[0].Plan.walk(func(n planNode) {
		switch n.NodeType {
		case "Seq Scan", "Sort", "Incremental Sort", "Bitmap Heap Scan", "Bitmap Index Scan", "Hash Join", "Merge Join":
			bad = append(bad, n.NodeType)
		case "Index Scan", "Index Only Scan":
			if n.IndexName == "notifications_claim_idx" {
				claimScans = append(claimScans, n)
			}
		case "ModifyTable":
			updated = n.ActualRows
		}
	})
	t.Logf("%s: %.3f ms", name, out[0].ExecutionTime)
	if len(bad) > 0 {
		t.Errorf("%s: the plan has %s:\n%s", name, strings.Join(bad, ", "), indent(raw))
	}
	if len(claimScans) != 1 {
		t.Fatalf("%s: the plan reads notifications_claim_idx %d times, want once:\n%s", name, len(claimScans), indent(raw))
	}
	scan := claimScans[0]
	if blocks := scan.SharedHit + scan.SharedRead; blocks > maxBlocks {
		t.Errorf("%s: the index scan touched %d pages, want at most %d", name, blocks, maxBlocks)
	}
	if int(scan.ActualRows) != wantRows || int(updated) != wantRows {
		t.Errorf("%s: the index scan returned %v rows and %v were updated, want %d", name, scan.ActualRows, updated, wantRows)
	}
}

func indent(s string) string {
	if len(s) > 4000 {
		s = s[:4000] + "..."
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
