package store_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// Rule 14: migrations are append-only. Every migration that has been recorded keeps its
// content, and a new migration must be recorded, after the others, in migrations.sha256.
func TestMigrationsAreAppendOnly(t *testing.T) {
	f, err := os.Open("migrations.sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var recorded []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			t.Fatalf("malformed line %q", sc.Text())
		}
		body, err := fs.ReadFile(store.Migrations(), name)
		if err != nil {
			t.Fatalf("recorded migration %s is gone: %v", name, err)
		}
		got := sha256.Sum256(body)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("migration %s was changed after it was recorded; add a new migration instead", name)
		}
		recorded = append(recorded, name)
	}
	files, err := fs.Glob(store.Migrations(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, recorded) {
		t.Errorf("migration files %v, recorded %v: record new files at the end of migrations.sha256", files, recorded)
	}
}

func TestMigrateBuildsAnEmptyDatabaseAndIsRepeatable(t *testing.T) {
	pool, err := store.Connect(ctx(t), testkit.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, _ := fs.Glob(store.Migrations(), "*.sql")
	for range 2 {
		v, err := store.Migrate(ctx(t), pool)
		if err != nil {
			t.Fatal(err)
		}
		if v != int64(len(files)) {
			t.Fatalf("schema version %d, want %d", v, len(files))
		}
	}
}

type column struct{ name, typ, nullable string }

// The data model of architecture §4, column by column.
func TestMigrationsCreateTheDataModel(t *testing.T) {
	e := testkit.Setup(t)
	want := map[string][]column{
		"batches": {
			{"id", "uuid", "NO"}, {"idempotency_key", "text", "YES"}, {"request_hash", "bytea", "YES"},
			{"total", "int4", "NO"}, {"created_at", "timestamptz", "NO"},
		},
		"notifications": {
			{"id", "uuid", "NO"}, {"batch_id", "uuid", "YES"}, {"recipient", "text", "NO"},
			{"channel", "text", "NO"}, {"content", "text", "NO"}, {"priority", "int2", "NO"},
			{"status", "text", "NO"}, {"attempts", "int4", "NO"}, {"max_attempts", "int4", "NO"},
			{"ready_at", "timestamptz", "NO"}, {"locked_until", "timestamptz", "YES"},
			{"idempotency_key", "text", "YES"}, {"request_hash", "bytea", "YES"},
			{"correlation_id", "text", "NO"}, {"traceparent", "text", "NO"},
			{"provider_message_id", "text", "YES"}, {"last_error", "text", "YES"},
			{"created_at", "timestamptz", "NO"}, {"updated_at", "timestamptz", "NO"},
			{"sent_at", "timestamptz", "YES"},
		},
		"delivery_attempts": {
			{"notification_id", "uuid", "NO"}, {"attempt_number", "int4", "NO"},
			{"started_at", "timestamptz", "NO"}, {"duration_ms", "int4", "NO"},
			{"status_code", "int4", "YES"}, {"outcome", "text", "NO"}, {"error", "text", "YES"},
		},
		"templates": {
			{"name", "text", "NO"}, {"channel", "text", "NO"}, {"body", "text", "NO"},
			{"created_at", "timestamptz", "NO"},
		},
	}
	for table, cols := range want {
		rows, err := e.Pool.Query(ctx(t), `SELECT column_name, udt_name, is_nullable FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []column
		for rows.Next() {
			var c column
			if err := rows.Scan(&c.name, &c.typ, &c.nullable); err != nil {
				t.Fatal(err)
			}
			got = append(got, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, cols) {
			t.Errorf("%s:\n got %v\nwant %v", table, got, cols)
		}
	}
	var tables int
	if err := e.Pool.QueryRow(ctx(t), `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'goose_db_version'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != len(want) {
		t.Errorf("%d tables, want %d", tables, len(want))
	}
}

func indexDef(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	err := pool.QueryRow(ctx(t), `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&def)
	if err != nil {
		t.Fatalf("index %s: %v", name, err)
	}
	return def
}

var quoted = regexp.MustCompile(`'([a-z]+)'::text`)

// The claim index (architecture §4) is ordered by (channel, priority, ready_at) and holds
// exactly the rows the claim may take: the claimable statuses of package domain. If the two
// lists differ, the claim either reads rows it must skip or misses rows it must take.
func TestClaimIndexCoversExactlyTheClaimableStatuses(t *testing.T) {
	e := testkit.Setup(t)
	def := indexDef(t, e.Pool, "notifications_claim_idx")
	if !strings.Contains(def, "(channel, priority, ready_at) WHERE") {
		t.Fatalf("claim index columns: %s", def)
	}
	var statuses []domain.Status
	for _, m := range quoted.FindAllStringSubmatch(def, -1) {
		statuses = append(statuses, domain.Status(m[1]))
	}
	if !slices.Equal(statuses, domain.Claimable) {
		t.Fatalf("claim index holds %v, the claimable statuses are %v", statuses, domain.Claimable)
	}
}

func TestReapListAndBatchIndexesExist(t *testing.T) {
	e := testkit.Setup(t)
	want := map[string]string{
		"notifications_reap_idx":  "(locked_until) WHERE (status = 'processing'::text)",
		"notifications_list_idx":  "(created_at DESC, id)",
		"notifications_batch_idx": "(batch_id)",
	}
	for name, suffix := range want {
		if def := indexDef(t, e.Pool, name); !strings.HasSuffix(def, suffix) {
			t.Errorf("%s: %s, want it to end with %s", name, def, suffix)
		}
	}
}

func insertNotification(c context.Context, pool *pgxpool.Pool, channel, status string, priority int, key *string) error {
	_, err := pool.Exec(c, `INSERT INTO notifications
		(id, recipient, channel, content, priority, status, max_attempts, ready_at, idempotency_key)
		VALUES ($1, 'r', $2, 'c', $3, $4, 6, now(), $5)`, uuid.New(), channel, priority, status, key)
	return err
}

func isViolation(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

const (
	checkViolation  = "23514"
	uniqueViolation = "23505"
	fkViolation     = "23503"
)

// The CHECK constraints accept exactly the values of package domain, so a value the Go code
// can produce is never refused by the database, and the database refuses anything else.
func TestColumnChecksAcceptExactlyTheDomainValues(t *testing.T) {
	e := testkit.Setup(t)
	c := ctx(t)
	for _, s := range domain.Statuses {
		if err := insertNotification(c, e.Pool, "sms", string(s), 0, nil); err != nil {
			t.Errorf("status %s refused: %v", s, err)
		}
	}
	for _, ch := range domain.Channels {
		if err := insertNotification(c, e.Pool, string(ch), "pending", 0, nil); err != nil {
			t.Errorf("channel %s refused: %v", ch, err)
		}
		if _, err := e.Pool.Exec(c, `INSERT INTO templates (name, channel, body) VALUES ($1, $1, 'b')`, string(ch)); err != nil {
			t.Errorf("template channel %s refused: %v", ch, err)
		}
	}
	for _, p := range domain.Priorities {
		if err := insertNotification(c, e.Pool, "sms", "pending", int(p), nil); err != nil {
			t.Errorf("priority %d refused: %v", p, err)
		}
	}
	bad := map[string]error{
		"status 'done'":   insertNotification(c, e.Pool, "sms", "done", 0, nil),
		"channel 'fax'":   insertNotification(c, e.Pool, "fax", "pending", 0, nil),
		"priority 3":      insertNotification(c, e.Pool, "sms", "pending", 3, nil),
		"priority -1":     insertNotification(c, e.Pool, "sms", "pending", -1, nil),
		"template 'fax'":  func() error { _, err := e.Pool.Exec(c, `INSERT INTO templates (name, channel, body) VALUES ('x', 'fax', 'b')`); return err }(),
		"status 'SENT'":   insertNotification(c, e.Pool, "sms", "SENT", 0, nil),
		"channel 'Email'": insertNotification(c, e.Pool, "Email", "pending", 0, nil),
	}
	for what, err := range bad {
		if !isViolation(err, checkViolation) {
			t.Errorf("%s: got %v, want a check violation", what, err)
		}
	}
}

// The database part of I10: one idempotency key, one row. Rows without a key do not collide.
func TestIdempotencyKeysAreUniqueInTheDatabase(t *testing.T) {
	e := testkit.Setup(t)
	c := ctx(t)
	key := "k-1"
	if err := insertNotification(c, e.Pool, "sms", "pending", 0, &key); err != nil {
		t.Fatal(err)
	}
	if err := insertNotification(c, e.Pool, "sms", "pending", 0, &key); !isViolation(err, uniqueViolation) {
		t.Errorf("second notification with key %q: %v, want a unique violation", key, err)
	}
	for range 2 {
		if err := insertNotification(c, e.Pool, "sms", "pending", 0, nil); err != nil {
			t.Errorf("notification without a key: %v", err)
		}
	}
	batch := func(key *string) error {
		_, err := e.Pool.Exec(c, `INSERT INTO batches (id, idempotency_key, total) VALUES ($1, $2, 1)`, uuid.New(), key)
		return err
	}
	if err := batch(&key); err != nil {
		t.Errorf("a batch may use a key a notification uses (separate columns): %v", err)
	}
	if err := batch(&key); !isViolation(err, uniqueViolation) {
		t.Errorf("second batch with key %q: %v, want a unique violation", key, err)
	}
	if err := batch(nil); err != nil {
		t.Errorf("batch without a key: %v", err)
	}
}

// One row per attempt number: a repeated Finish cannot add a second attempt row (I3), and an
// attempt row always belongs to a notification.
func TestAnAttemptNumberIsRecordedOnce(t *testing.T) {
	e := testkit.Setup(t)
	c := ctx(t)
	id := uuid.New()
	if _, err := e.Pool.Exec(c, `INSERT INTO notifications (id, recipient, channel, content, priority, status, max_attempts, ready_at)
		VALUES ($1, 'r', 'sms', 'c', 0, 'processing', 6, now())`, id); err != nil {
		t.Fatal(err)
	}
	attempt := func(nid uuid.UUID, n int) error {
		_, err := e.Pool.Exec(c, `INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, outcome)
			VALUES ($1, $2, now(), 5, 'sent')`, nid, n)
		return err
	}
	if err := attempt(id, 1); err != nil {
		t.Fatal(err)
	}
	if err := attempt(id, 1); !isViolation(err, uniqueViolation) {
		t.Errorf("second row for attempt 1: %v, want a unique violation", err)
	}
	if err := attempt(id, 2); err != nil {
		t.Errorf("attempt 2: %v", err)
	}
	if err := attempt(uuid.New(), 1); !isViolation(err, fkViolation) {
		t.Errorf("attempt of an unknown notification: %v, want a foreign key violation", err)
	}
	if _, err := e.Pool.Exec(c, `INSERT INTO notifications (id, batch_id, recipient, channel, content, priority, status, max_attempts, ready_at)
		VALUES ($1, $2, 'r', 'sms', 'c', 0, 'pending', 6, now())`, uuid.New(), uuid.New()); !isViolation(err, fkViolation) {
		t.Errorf("notification of an unknown batch: %v, want a foreign key violation", err)
	}
}
