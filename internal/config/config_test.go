package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func defaults(t *testing.T) Worker {
	t.Helper()
	c, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load with no environment: %v", err)
	}
	return c.Worker
}

// requireRelation checks one relation of architecture §13 at its bound: the value on the
// bound is accepted, one step past it is refused, and the error names the setting.
func requireRelation(t *testing.T, name string, onBound, pastBound func(*Worker)) {
	t.Helper()
	w := defaults(t)
	onBound(&w)
	if err := w.Check(); err != nil {
		t.Errorf("value on the bound refused: %v", err)
	}
	w = defaults(t)
	pastBound(&w)
	err := w.Check()
	if err == nil {
		t.Fatalf("value past the bound accepted: %+v", w)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error does not name %s: %v", name, err)
	}
}

func TestDefaultsSatisfyEveryRelation(t *testing.T) {
	w := defaults(t)
	if err := w.Check(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
}

func TestDefaultsAreTheTimingBudget(t *testing.T) {
	want := Worker{
		RateLimit: 100, WindowMargin: 50 * time.Millisecond,
		SendTimeout: 15 * time.Second, DialTimeout: 3 * time.Second, TLSHandshakeTimeout: 3 * time.Second,
		GateBudget: 5 * time.Second, Lease: 60 * time.Second, BreakerOpen: 30 * time.Second,
		ShutdownTimeout: 25 * time.Second, Slots: 50, IdleConns: 50, IdleConnTimeout: 10 * time.Second,
		ClaimBatch: 10, PauseBusy: 20 * time.Millisecond, PauseIdle: 200 * time.Millisecond,
		ReaperTick: 5 * time.Second, MaxAttempts: 6, BackoffBase: 2 * time.Second, BackoffCap: 2 * time.Minute,
	}
	if got := defaults(t); got != want {
		t.Fatalf("defaults\n got %+v\nwant %+v", got, want)
	}
}

func TestDialTimeoutMustBeShorterThanSendTimeout(t *testing.T) {
	requireRelation(t, "DIAL_TIMEOUT",
		func(w *Worker) { w.DialTimeout = w.SendTimeout - time.Millisecond },
		func(w *Worker) { w.DialTimeout = w.SendTimeout })
}

func TestTLSHandshakeTimeoutMustBeShorterThanSendTimeout(t *testing.T) {
	requireRelation(t, "TLS_HANDSHAKE_TIMEOUT",
		func(w *Worker) { w.TLSHandshakeTimeout = w.SendTimeout - time.Millisecond },
		func(w *Worker) { w.TLSHandshakeTimeout = w.SendTimeout })
}

func TestGateBudgetMustBeShorterThanSendTimeout(t *testing.T) {
	requireRelation(t, "GATE_BUDGET",
		func(w *Worker) { w.GateBudget = w.SendTimeout - time.Millisecond },
		func(w *Worker) { w.GateBudget = w.SendTimeout })
}

func TestLeaseMustCoverSendTimeoutPlusResultWrite(t *testing.T) {
	requireRelation(t, "LEASE",
		func(w *Worker) { w.Lease = w.SendTimeout + 30*time.Second },
		func(w *Worker) { w.Lease = w.SendTimeout + 30*time.Second - time.Millisecond })
}

func TestBreakerOpenTimeMustExceedSendTimeout(t *testing.T) {
	requireRelation(t, "BREAKER_OPEN",
		func(w *Worker) { w.BreakerOpen = w.SendTimeout + time.Millisecond },
		func(w *Worker) { w.BreakerOpen = w.SendTimeout })
}

func TestShutdownTimeoutMustCoverSendTimeoutPlusFiveSeconds(t *testing.T) {
	requireRelation(t, "SHUTDOWN_TIMEOUT",
		func(w *Worker) { w.ShutdownTimeout = w.SendTimeout + 5*time.Second },
		func(w *Worker) { w.ShutdownTimeout = w.SendTimeout + 5*time.Second - time.Millisecond })
}

func TestIdleConnectionsMustEqualSlots(t *testing.T) {
	requireRelation(t, "IDLE_CONNS",
		func(w *Worker) { w.Slots, w.IdleConns = 20, 20 },
		func(w *Worker) { w.IdleConns = w.Slots - 1 })
	requireRelation(t, "IDLE_CONNS",
		func(w *Worker) {},
		func(w *Worker) { w.IdleConns = w.Slots + 1 })
}

func TestClaimBatchMustNotExceedSlots(t *testing.T) {
	requireRelation(t, "CLAIM_BATCH",
		func(w *Worker) { w.ClaimBatch = w.Slots },
		func(w *Worker) { w.ClaimBatch = w.Slots + 1 })
}

// A relation between two settings is meaningless if one of them is zero or negative, so
// every number in the budget must be positive. Each field is broken alone.
func TestNonPositiveSettingsAreRefused(t *testing.T) {
	fields := map[string]func(*Worker){
		"RATE_LIMIT":            func(w *Worker) { w.RateLimit = 0 },
		"WINDOW_MARGIN":         func(w *Worker) { w.WindowMargin = 0 },
		"SEND_TIMEOUT":          func(w *Worker) { w.SendTimeout = -time.Second },
		"DIAL_TIMEOUT":          func(w *Worker) { w.DialTimeout = 0 },
		"TLS_HANDSHAKE_TIMEOUT": func(w *Worker) { w.TLSHandshakeTimeout = 0 },
		"GATE_BUDGET":           func(w *Worker) { w.GateBudget = 0 },
		"SLOTS":                 func(w *Worker) { w.Slots, w.IdleConns, w.ClaimBatch = 0, 0, 0 },
		"IDLE_CONN_TIMEOUT":     func(w *Worker) { w.IdleConnTimeout = 0 },
		"CLAIM_BATCH":           func(w *Worker) { w.ClaimBatch = 0 },
		"PAUSE_BUSY":            func(w *Worker) { w.PauseBusy = 0 },
		"PAUSE_IDLE":            func(w *Worker) { w.PauseIdle = 0 },
		"REAPER_TICK":           func(w *Worker) { w.ReaperTick = 0 },
		"MAX_ATTEMPTS":          func(w *Worker) { w.MaxAttempts = 0 },
		"BACKOFF_BASE":          func(w *Worker) { w.BackoffBase = 0 },
		"BACKOFF_CAP":           func(w *Worker) { w.BackoffCap = 0 },
	}
	for name, breakIt := range fields {
		t.Run(name, func(t *testing.T) {
			w := defaults(t)
			breakIt(&w)
			err := w.Check()
			if err == nil || !strings.Contains(err.Error(), name+" must be positive") {
				t.Fatalf("Check() = %v, want an error saying %s must be positive", err, name)
			}
		})
	}
}

func TestEveryBrokenRelationIsReported(t *testing.T) {
	w := defaults(t)
	w.Lease = time.Second
	w.ClaimBatch = w.Slots + 1
	err := w.Check()
	if err == nil || !strings.Contains(err.Error(), "LEASE") || !strings.Contains(err.Error(), "CLAIM_BATCH") {
		t.Fatalf("Check() = %v, want both LEASE and CLAIM_BATCH", err)
	}
}

func TestLoadReadsEverySettingFromTheEnvironment(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://db", "REDIS_URL": "redis://r", "PROVIDER_URL": "http://p/send",
		"API_ADDR": ":1", "LOG_LEVEL": "debug",
		"RATE_LIMIT": "7", "WINDOW_MARGIN": "1ms", "SEND_TIMEOUT": "2s", "DIAL_TIMEOUT": "3ms",
		"TLS_HANDSHAKE_TIMEOUT": "4ms", "GATE_BUDGET": "5ms", "LEASE": "6m", "BREAKER_OPEN": "7s",
		"SHUTDOWN_TIMEOUT": "8s", "SLOTS": "9", "IDLE_CONNS": "10", "IDLE_CONN_TIMEOUT": "11s",
		"CLAIM_BATCH": "12", "PAUSE_BUSY": "13ms", "PAUSE_IDLE": "14ms", "REAPER_TICK": "15s",
		"MAX_ATTEMPTS": "16", "BACKOFF_BASE": "17s", "BACKOFF_CAP": "18m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL: "postgres://db", RedisURL: "redis://r", ProviderURL: "http://p/send",
		APIAddr: ":1", LogLevel: "debug",
		Worker: Worker{
			RateLimit: 7, WindowMargin: time.Millisecond, SendTimeout: 2 * time.Second,
			DialTimeout: 3 * time.Millisecond, TLSHandshakeTimeout: 4 * time.Millisecond,
			GateBudget: 5 * time.Millisecond, Lease: 6 * time.Minute, BreakerOpen: 7 * time.Second,
			ShutdownTimeout: 8 * time.Second, Slots: 9, IdleConns: 10, IdleConnTimeout: 11 * time.Second,
			ClaimBatch: 12, PauseBusy: 13 * time.Millisecond, PauseIdle: 14 * time.Millisecond,
			ReaperTick: 15 * time.Second, MaxAttempts: 16, BackoffBase: 17 * time.Second,
			BackoffCap: 18 * time.Minute,
		},
	}
	if c != want {
		t.Fatalf("Load\n got %+v\nwant %+v", c, want)
	}
}

func TestLoadNamesEveryMalformedValue(t *testing.T) {
	_, err := Load(env(map[string]string{"LEASE": "sixty", "SLOTS": "1.5", "LOG_LEVEL": "loud"}))
	if err == nil {
		t.Fatal("malformed values accepted")
	}
	for _, name := range []string{"LEASE", "SLOTS", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
}

// Architecture §13: the container's stop grace period must be longer than the shutdown
// timeout, or Docker kills the worker while it is still waiting for sends in flight. A
// single worker cannot check this, so this test reads docker-compose.yml.
func TestComposeStopGracePeriodExceedsShutdownTimeout(t *testing.T) {
	compose, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+stop_grace_period:\s*(\S+)`).FindAllSubmatch(compose, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one stop_grace_period (the worker's), found %d", len(m))
	}
	grace, err := time.ParseDuration(string(m[0][1]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compose), "SHUTDOWN_TIMEOUT") {
		t.Fatal("docker-compose.yml overrides SHUTDOWN_TIMEOUT; compare against that value instead")
	}
	if shutdown := defaults(t).ShutdownTimeout; grace <= shutdown {
		t.Fatalf("stop_grace_period %s is not longer than SHUTDOWN_TIMEOUT %s", grace, shutdown)
	}
}
