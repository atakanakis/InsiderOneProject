package store_test

import (
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// Architecture §5: every session the service opens ends a transaction left idle, and the
// API's sessions cancel a long statement, so the service cannot hold vacuum back for long.
// Checked by behaviour, not only by SHOW.
func TestServiceSessionsCarryTheirTimeouts(t *testing.T) {
	e := testkit.Setup(t)

	def, err := store.Connect(ctx(t), e.DatabaseURL, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer def.Close()
	var idle, stmt string
	if err := def.QueryRow(ctx(t), `SELECT current_setting('idle_in_transaction_session_timeout'), current_setting('statement_timeout')`).Scan(&idle, &stmt); err != nil {
		t.Fatal(err)
	}
	if idle != "10s" || stmt != "0" {
		t.Errorf("defaults: idle_in_transaction_session_timeout %s, statement_timeout %s; want 10s and 0", idle, stmt)
	}

	short, err := store.Connect(ctx(t), e.DatabaseURL, store.Options{
		StatementTimeout: 200 * time.Millisecond, IdleInTransactionTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if _, err := short.Exec(ctx(t), `SELECT pg_sleep(1)`); !isViolation(err, "57014") {
		t.Errorf("a statement past the statement timeout: %v, want query_canceled (57014)", err)
	}
	tx, err := short.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := tx.Exec(ctx(t), `SELECT 1`); err == nil {
		t.Error("a transaction left idle past the timeout could still be used")
	}
	_ = tx.Rollback(ctx(t))
}
