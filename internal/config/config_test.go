package config

import (
	"os"
	"reflect"
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
	w, err := LoadWorker(env(nil))
	if err != nil {
		t.Fatalf("LoadWorker with no environment: %v", err)
	}
	return w
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
		ShutdownTimeout: 25 * time.Second, StopGracePeriod: 30 * time.Second, Slots: 50, IdleConns: 50, IdleConnTimeout: 10 * time.Second,
		ClaimBatch: 10, PauseBusy: 20 * time.Millisecond, PauseIdle: 200 * time.Millisecond,
		ReaperTick: 5 * time.Second, BackoffBase: 2 * time.Second, BackoffCap: 2 * time.Minute,
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

func TestShutdownTimeoutMustEndBeforeTheStopGracePeriod(t *testing.T) {
	requireRelation(t, "STOP_GRACE_PERIOD",
		func(w *Worker) { w.ShutdownTimeout = w.StopGracePeriod - time.Millisecond },
		func(w *Worker) { w.StopGracePeriod = w.ShutdownTimeout })
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
		"LEASE":                 func(w *Worker) { w.Lease = 0 },
		"BREAKER_OPEN":          func(w *Worker) { w.BreakerOpen = 0 },
		"SHUTDOWN_TIMEOUT":      func(w *Worker) { w.ShutdownTimeout = 0 },
		"STOP_GRACE_PERIOD":     func(w *Worker) { w.StopGracePeriod = 0 },
		"SLOTS":                 func(w *Worker) { w.Slots, w.IdleConns, w.ClaimBatch = 0, 0, 0 },
		"IDLE_CONN_TIMEOUT":     func(w *Worker) { w.IdleConnTimeout = 0 },
		"CLAIM_BATCH":           func(w *Worker) { w.ClaimBatch = 0 },
		"PAUSE_BUSY":            func(w *Worker) { w.PauseBusy = 0 },
		"PAUSE_IDLE":            func(w *Worker) { w.PauseIdle = 0 },
		"REAPER_TICK":           func(w *Worker) { w.ReaperTick = 0 },
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
		"API_ADDR": ":1", "LOG_LEVEL": "debug", "API_KEYS": " k1, ,k2 ", "MAX_ATTEMPTS": "16",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Config{DatabaseURL: "postgres://db", RedisURL: "redis://r", ProviderURL: "http://p/send", APIAddr: ":1", LogLevel: "debug",
		APIKeys: []string{"k1", "k2"}, MaxAttempts: 16}); !reflect.DeepEqual(c, want) {
		t.Fatalf("Load\n got %+v\nwant %+v", c, want)
	}
	w, err := LoadWorker(env(map[string]string{
		"RATE_LIMIT": "7", "WINDOW_MARGIN": "1ms", "SEND_TIMEOUT": "2s", "DIAL_TIMEOUT": "3ms",
		"TLS_HANDSHAKE_TIMEOUT": "4ms", "GATE_BUDGET": "5ms", "LEASE": "6m", "BREAKER_OPEN": "7s",
		"SHUTDOWN_TIMEOUT": "8s", "STOP_GRACE_PERIOD": "19s", "SLOTS": "9", "IDLE_CONNS": "10",
		"IDLE_CONN_TIMEOUT": "11s", "CLAIM_BATCH": "12", "PAUSE_BUSY": "13ms", "PAUSE_IDLE": "14ms",
		"REAPER_TICK": "15s", "BACKOFF_BASE": "17s", "BACKOFF_CAP": "18m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Worker{
		RateLimit: 7, WindowMargin: time.Millisecond, SendTimeout: 2 * time.Second,
		DialTimeout: 3 * time.Millisecond, TLSHandshakeTimeout: 4 * time.Millisecond,
		GateBudget: 5 * time.Millisecond, Lease: 6 * time.Minute, BreakerOpen: 7 * time.Second,
		ShutdownTimeout: 8 * time.Second, StopGracePeriod: 19 * time.Second, Slots: 9, IdleConns: 10,
		IdleConnTimeout: 11 * time.Second, ClaimBatch: 12, PauseBusy: 13 * time.Millisecond,
		PauseIdle: 14 * time.Millisecond, ReaperTick: 15 * time.Second,
		BackoffBase: 17 * time.Second, BackoffCap: 18 * time.Minute,
	}
	if w != want {
		t.Fatalf("LoadWorker\n got %+v\nwant %+v", w, want)
	}
}

func TestLoadNamesEveryMalformedValue(t *testing.T) {
	_, err := LoadWorker(env(map[string]string{"LEASE": "sixty", "SLOTS": "1.5"}))
	if err == nil || !strings.Contains(err.Error(), "LEASE") || !strings.Contains(err.Error(), "SLOTS") {
		t.Errorf("LoadWorker: %v, want both LEASE and SLOTS named", err)
	}
	if _, err := Load(env(map[string]string{"LOG_LEVEL": "loud"})); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Errorf("Load: %v, want LOG_LEVEL named", err)
	}
}

// The API and the migration share the .env file with the worker. A malformed worker setting
// must not stop them: it is the worker's to refuse.
func TestSharedSettingsIgnoreMalformedWorkerSettings(t *testing.T) {
	if _, err := Load(env(map[string]string{"LEASE": "sixty", "SLOTS": "many"})); err != nil {
		t.Fatalf("Load refused a worker setting: %v", err)
	}
}

// Architecture §13: docker-compose.yml takes the container's stop_grace_period and the
// worker's STOP_GRACE_PERIOD from one variable with one default, so the worker checks the
// grace period the platform really gives it, whatever a .env file says.
func TestComposeGivesTheWorkerItsStopGracePeriod(t *testing.T) {
	compose, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	grace := regexp.MustCompile(`(?m)^\s+stop_grace_period:\s*(\S+)\s*$`).FindAllSubmatch(compose, -1)
	passed := regexp.MustCompile(`(?m)^\s+STOP_GRACE_PERIOD:\s*(\S+)\s*$`).FindAllSubmatch(compose, -1)
	if len(grace) != 1 || len(passed) != 1 {
		t.Fatalf("want one stop_grace_period (the worker's) and one STOP_GRACE_PERIOD; found %d and %d", len(grace), len(passed))
	}
	m := regexp.MustCompile(`^\$\{STOP_GRACE_PERIOD:-([0-9a-z]+)\}$`).FindSubmatch(grace[0][1])
	if m == nil || string(passed[0][1]) != string(grace[0][1]) {
		t.Fatalf("stop_grace_period %q and STOP_GRACE_PERIOD %q must both be ${STOP_GRACE_PERIOD:-<default>}", grace[0][1], passed[0][1])
	}
	def, err := time.ParseDuration(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if want := defaults(t).StopGracePeriod; def != want {
		t.Fatalf("Compose default %s, config default %s", def, want)
	}
}

// The API reads MAX_ATTEMPTS for a new row and the budget of /retry; a budget of 0 would
// make every requeued row fail without a send.
func TestMaxAttemptsMustBePositive(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		if _, err := Load(env(map[string]string{"MAX_ATTEMPTS": v})); err == nil || !strings.Contains(err.Error(), "MAX_ATTEMPTS must be positive") {
			t.Errorf("MAX_ATTEMPTS=%s: %v", v, err)
		}
	}
	if c, err := Load(env(map[string]string{})); err != nil || c.MaxAttempts != 6 {
		t.Errorf("default: %d %v", c.MaxAttempts, err)
	}
}
