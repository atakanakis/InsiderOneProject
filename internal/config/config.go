// Package config reads the settings of every command from environment variables, and checks
// the relations of the timing budget in architecture §13.
//
// Load reads the settings every command uses; LoadWorker reads the timing budget, which only
// the worker uses, so a malformed worker setting cannot stop the API or the migration. The
// worker calls Worker.Check before it starts and refuses to start when a relation does not
// hold.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ResultWriteReserve is the part of the lease kept for writing the result after the send
// timeout has passed (architecture §13: lease ≥ send timeout + 30 s).
const ResultWriteReserve = 30 * time.Second

// ShutdownReserve is how much longer than the send timeout the worker waits for sends in
// flight before it exits (architecture §13: shutdown timeout ≥ send timeout + 5 s).
const ShutdownReserve = 5 * time.Second

// Config holds the settings shared by the commands.
type Config struct {
	DatabaseURL string // DATABASE_URL
	RedisURL    string // REDIS_URL
	ProviderURL string // PROVIDER_URL
	APIAddr     string // API_ADDR
	LogLevel    string // LOG_LEVEL: debug, info, warn or error
	// APIKeys (API_KEYS, separated by commas): when set, every /v1 request needs one
	// (architecture §8).
	APIKeys []string
	// MaxAttempts (MAX_ATTEMPTS): a new row's max_attempts and the budget of /retry. The API
	// reads it, so it is a shared setting, not part of the worker's timing budget.
	MaxAttempts int
}

// Worker is the timing budget of architecture §13. The comment on each field is its
// environment variable.
type Worker struct {
	RateLimit           int           // RATE_LIMIT: permits per window per channel
	WindowMargin        time.Duration // WINDOW_MARGIN: the window is one second plus this
	SendTimeout         time.Duration // SEND_TIMEOUT: connect + gate + write + response
	DialTimeout         time.Duration // DIAL_TIMEOUT
	TLSHandshakeTimeout time.Duration // TLS_HANDSHAKE_TIMEOUT
	GateBudget          time.Duration // GATE_BUDGET: longest wait for a permit
	Lease               time.Duration // LEASE
	BreakerOpen         time.Duration // BREAKER_OPEN
	ShutdownTimeout     time.Duration // SHUTDOWN_TIMEOUT
	StopGracePeriod     time.Duration // STOP_GRACE_PERIOD: how long the platform waits after SIGTERM before a kill
	Slots               int           // SLOTS: concurrent sends per channel per worker
	IdleConns           int           // IDLE_CONNS: idle connections kept per provider host
	IdleConnTimeout     time.Duration // IDLE_CONN_TIMEOUT
	ClaimBatch          int           // CLAIM_BATCH
	PauseBusy           time.Duration // PAUSE_BUSY: pause when there is no room
	PauseIdle           time.Duration // PAUSE_IDLE: pause when there is nothing to claim
	ReaperTick          time.Duration // REAPER_TICK
	BackoffBase         time.Duration // BACKOFF_BASE: first retry delay, doubled each time
	BackoffCap          time.Duration // BACKOFF_CAP
}

// Load reads the shared settings with getenv (os.Getenv in the commands). A variable that
// is unset or empty takes its default. Every malformed value is reported, by name.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	c := Config{
		DatabaseURL: p.str("DATABASE_URL", "postgres://notifications:notifications@localhost:5432/notifications?sslmode=disable"),
		RedisURL:    p.str("REDIS_URL", "redis://localhost:6379/0"),
		ProviderURL: p.str("PROVIDER_URL", "http://localhost:9090/send"),
		APIAddr:     p.str("API_ADDR", ":8080"),
		LogLevel:    p.oneOf("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		APIKeys:     p.list("API_KEYS"),
		MaxAttempts: p.integer("MAX_ATTEMPTS", 6),
	}
	if c.MaxAttempts <= 0 {
		p.errs = append(p.errs, fmt.Errorf("MAX_ATTEMPTS must be positive, got %d", c.MaxAttempts))
	}
	return c, errors.Join(p.errs...)
}

// LoadWorker reads the timing budget of architecture §13 with getenv. It does not check the
// relations; Worker.Check does.
func LoadWorker(getenv func(string) string) (Worker, error) {
	p := parser{getenv: getenv}
	w := Worker{
		RateLimit:           p.integer("RATE_LIMIT", 100),
		WindowMargin:        p.duration("WINDOW_MARGIN", 50*time.Millisecond),
		SendTimeout:         p.duration("SEND_TIMEOUT", 15*time.Second),
		DialTimeout:         p.duration("DIAL_TIMEOUT", 3*time.Second),
		TLSHandshakeTimeout: p.duration("TLS_HANDSHAKE_TIMEOUT", 3*time.Second),
		GateBudget:          p.duration("GATE_BUDGET", 5*time.Second),
		Lease:               p.duration("LEASE", 60*time.Second),
		BreakerOpen:         p.duration("BREAKER_OPEN", 30*time.Second),
		ShutdownTimeout:     p.duration("SHUTDOWN_TIMEOUT", 25*time.Second),
		StopGracePeriod:     p.duration("STOP_GRACE_PERIOD", 30*time.Second),
		Slots:               p.integer("SLOTS", 50),
		IdleConns:           p.integer("IDLE_CONNS", 50),
		IdleConnTimeout:     p.duration("IDLE_CONN_TIMEOUT", 10*time.Second),
		ClaimBatch:          p.integer("CLAIM_BATCH", 10),
		PauseBusy:           p.duration("PAUSE_BUSY", 20*time.Millisecond),
		PauseIdle:           p.duration("PAUSE_IDLE", 200*time.Millisecond),
		ReaperTick:          p.duration("REAPER_TICK", 5*time.Second),
		BackoffBase:         p.duration("BACKOFF_BASE", 2*time.Second),
		BackoffCap:          p.duration("BACKOFF_CAP", 2*time.Minute),
	}
	return w, errors.Join(p.errs...)
}

// Check returns an error naming every relation of architecture §13 that does not hold.
func (w Worker) Check() error {
	var errs []error
	for _, s := range w.numbers() {
		if s.value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %d", s.name, s.value))
		}
	}
	if w.DialTimeout >= w.SendTimeout {
		errs = append(errs, fmt.Errorf("DIAL_TIMEOUT (%s) must be shorter than SEND_TIMEOUT (%s)", w.DialTimeout, w.SendTimeout))
	}
	if w.TLSHandshakeTimeout >= w.SendTimeout {
		errs = append(errs, fmt.Errorf("TLS_HANDSHAKE_TIMEOUT (%s) must be shorter than SEND_TIMEOUT (%s)", w.TLSHandshakeTimeout, w.SendTimeout))
	}
	if w.GateBudget >= w.SendTimeout {
		errs = append(errs, fmt.Errorf("GATE_BUDGET (%s) must be shorter than SEND_TIMEOUT (%s)", w.GateBudget, w.SendTimeout))
	}
	if w.Lease < w.SendTimeout+ResultWriteReserve {
		errs = append(errs, fmt.Errorf("LEASE (%s) must be at least SEND_TIMEOUT (%s) + %s for the result write", w.Lease, w.SendTimeout, ResultWriteReserve))
	}
	if w.BreakerOpen <= w.SendTimeout {
		errs = append(errs, fmt.Errorf("BREAKER_OPEN (%s) must be longer than SEND_TIMEOUT (%s)", w.BreakerOpen, w.SendTimeout))
	}
	if w.ShutdownTimeout < w.SendTimeout+ShutdownReserve {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT (%s) must be at least SEND_TIMEOUT (%s) + %s", w.ShutdownTimeout, w.SendTimeout, ShutdownReserve))
	}
	if w.StopGracePeriod <= w.ShutdownTimeout {
		errs = append(errs, fmt.Errorf("STOP_GRACE_PERIOD (%s) must be longer than SHUTDOWN_TIMEOUT (%s), or the platform kills the worker while sends are in flight", w.StopGracePeriod, w.ShutdownTimeout))
	}
	if w.IdleConns != w.Slots {
		errs = append(errs, fmt.Errorf("IDLE_CONNS (%d) must equal SLOTS (%d)", w.IdleConns, w.Slots))
	}
	if w.ClaimBatch > w.Slots {
		errs = append(errs, fmt.Errorf("CLAIM_BATCH (%d) must not exceed SLOTS (%d)", w.ClaimBatch, w.Slots))
	}
	return errors.Join(errs...)
}

type named struct {
	name  string
	value int64
}

// numbers lists every setting of the budget; each must be positive.
func (w Worker) numbers() []named {
	return []named{
		{"RATE_LIMIT", int64(w.RateLimit)},
		{"WINDOW_MARGIN", int64(w.WindowMargin)},
		{"SEND_TIMEOUT", int64(w.SendTimeout)},
		{"DIAL_TIMEOUT", int64(w.DialTimeout)},
		{"TLS_HANDSHAKE_TIMEOUT", int64(w.TLSHandshakeTimeout)},
		{"GATE_BUDGET", int64(w.GateBudget)},
		{"LEASE", int64(w.Lease)},
		{"BREAKER_OPEN", int64(w.BreakerOpen)},
		{"SHUTDOWN_TIMEOUT", int64(w.ShutdownTimeout)},
		{"STOP_GRACE_PERIOD", int64(w.StopGracePeriod)},
		{"SLOTS", int64(w.Slots)},
		{"IDLE_CONNS", int64(w.IdleConns)},
		{"IDLE_CONN_TIMEOUT", int64(w.IdleConnTimeout)},
		{"CLAIM_BATCH", int64(w.ClaimBatch)},
		{"PAUSE_BUSY", int64(w.PauseBusy)},
		{"PAUSE_IDLE", int64(w.PauseIdle)},
		{"REAPER_TICK", int64(w.ReaperTick)},
		{"BACKOFF_BASE", int64(w.BackoffBase)},
		{"BACKOFF_CAP", int64(w.BackoffCap)},
	}
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) str(name, def string) string {
	if v := p.getenv(name); v != "" {
		return v
	}
	return def
}

// list splits a comma-separated value; empty entries are dropped.
func (p *parser) list(name string) []string {
	var out []string
	for _, v := range strings.Split(p.getenv(name), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (p *parser) oneOf(name, def string, allowed ...string) string {
	v := p.str(name, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.errs = append(p.errs, fmt.Errorf("%s: %q is not one of %v", name, v, allowed))
	return def
}

func (p *parser) integer(name string, def int) int {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a whole number", name, v))
		return def
	}
	return n
}

func (p *parser) duration(name string, def time.Duration) time.Duration {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a duration such as 15s or 50ms", name, v))
		return def
	}
	return d
}
