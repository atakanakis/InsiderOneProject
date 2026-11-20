package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

const (
	rulesFile  = "../../deploy/prometheus/alerts.yml"
	testsFile  = "../../deploy/prometheus/alerts_test.yml"
	configFile = "../../deploy/prometheus/prometheus.yml"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §10: every alert rule has a promtool test case in which it fires; a rule without one is
// found.
func TestEveryAlertRuleHasAFiringTest(t *testing.T) {
	rules, tests := readFile(t, rulesFile), readFile(t, testsFile)
	missing, err := untested(rules, tests)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("rules without a firing test case: %v", missing)
	}
	if !bytes.Contains(tests, []byte("rule_files:\n  - alerts.yml")) {
		t.Error("alerts_test.yml does not test alerts.yml")
	}
	// The check finds a rule that has no case, and one whose only case expects nothing.
	extra := append(slices.Clone(rules), []byte(`
      - alert: NotificationsUntested
        expr: notifications_breaker_state > 5
`)...)
	quiet := append(slices.Clone(tests), []byte(`
  - name: a case that expects no alert
    interval: 1m
    input_series: []
    alert_rule_test:
      - eval_time: 1m
        alertname: NotificationsQuiet
        exp_alerts: []
`)...)
	extra = append(extra, []byte(`
      - alert: NotificationsQuiet
        expr: notifications_breaker_state > 5
`)...)
	missing, err = untested(extra, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"NotificationsQuiet", "NotificationsUntested"}) {
		t.Errorf("rules found without a firing case: %v, want NotificationsQuiet and NotificationsUntested", missing)
	}
}

const sampleScrape = `# TYPE notifications_attempts_total counter
notifications_attempts_total{channel="sms",outcome="sent"} 3
notifications_attempts_total{channel="sms",outcome="closed_idle"} 0
# TYPE notifications_gate_wait_seconds histogram
notifications_gate_wait_seconds_bucket{channel="sms",le="+Inf"} 3
notifications_gate_wait_seconds_sum{channel="sms"} 0.1
notifications_gate_wait_seconds_count{channel="sms"} 3
# TYPE notifications_events_dropped_total counter
notifications_events_dropped_total{reason="buffer_full"} 0
`

// §10: the check refuses a rule that names a metric, a label value or a grouping label that
// no scrape exports, and passes one that names only what is exported.
func TestTheCheckRefusesWhatNoBinaryExports(t *testing.T) {
	rule := func(expr string) []byte {
		return []byte("groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: " + expr + "\n")
	}
	for _, tc := range []struct {
		expr string
		ok   bool
	}{
		{`sum by (channel) (rate(notifications_attempts_total{outcome="sent"}[5m])) > 0`, true},
		{`sum by (channel) (rate(notifications_attempts_total{outcome=~"sent|closed_idle"}[5m])) > 0`, true},
		{`histogram_quantile(0.9, sum by (le, channel) (rate(notifications_gate_wait_seconds_bucket[5m]))) > 1`, true},
		{`sum(rate(notifications_events_dropped_total[5m])) > 0`, true},
		{`sum(rate(notifications_stale_permits_total[5m])) > 0`, false},                     // never exported
		{`sum(rate(notifications_attempts_total{outcome="delivered"}[5m])) > 0`, false},     // a value never used
		{`sum(rate(notifications_attempts_total{outcome=~"sent|refused"}[5m])) > 0`, false}, // one value of a regex
		{`sum(rate(notifications_attempts_total{status="sent"}[5m])) > 0`, false},           // a label it has not
		{`sum by (instance_id) (rate(notifications_attempts_total[5m])) > 0`, false},        // grouped by a label it has not
		{`max by (channel) (notifications_events_dropped_total) > 0`, false},                // the same, the other metric
		{`sum(rate(notifications_attempts_total{outcome=~"sent.*"}[5m])) > 0`, false},       // a regex the check cannot read
		{`up{job="api"} == 0`, true}, // Prometheus's own series, a job of the configuration
		{`absent(up{job="worker"} == 1)`, true},
		{`up{job="apiserver"} == 0`, false},              // a job the configuration does not have
		{`max by (channel) (up{job="api"}) == 0`, false}, // up has no channel
	} {
		missing, err := unexported(rule(tc.expr), []string{"api", "worker"}, []byte(sampleScrape))
		if err != nil {
			t.Fatal(err)
		}
		if tc.ok != (len(missing) == 0) {
			t.Errorf("%s: %v, want ok=%v", tc.expr, missing, tc.ok)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// syncBuffer is a process's output, read while it runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// scrapeWhenUp reads a /metrics until it answers.
func scrapeWhenUp(t *testing.T, url string, out *syncBuffer) []byte {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && bytes.Contains(b, []byte("notifications_")) {
			return b
		}
	}
	t.Fatalf("%s did not answer within 30 s:\n%s", url, out.String())
	return nil
}

// §10: every metric, label value and grouping label of every rule is exported by the real
// binaries: cmd/api and cmd/worker are built and run against Postgres, Redis and the
// recording provider, and their /metrics read.
func TestEveryAlertRuleUsesMetricsTheBinariesExport(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	dir := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", dir, "../../cmd/api", "../../cmd/worker").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	apiAddr, workerAddr := freeAddr(t), freeAddr(t)
	env := []string{"DATABASE_URL=" + e.DatabaseURL, "REDIS_URL=" + e.RedisURL, "PROVIDER_URL=" + srv.URL + "/send",
		"API_ADDR=" + apiAddr, "METRICS_ADDR=" + workerAddr}
	var scrapes [][]byte
	for _, run := range []struct{ bin, url string }{
		{"api", "http://" + apiAddr + "/metrics"},
		{"worker", "http://" + workerAddr + "/metrics"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, filepath.Join(dir, run.bin))
		cmd.Env = env
		out := &syncBuffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = cmd.Wait() })
		scrapes = append(scrapes, scrapeWhenUp(t, run.url, out))
	}
	jobs, err := scrapeJobs(readFile(t, configFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(jobs, []string{"api", "worker"}) {
		t.Errorf("the jobs of prometheus.yml: %v, want api and worker", jobs)
	}
	missing, err := unexported(readFile(t, rulesFile), jobs, scrapes...)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range missing {
		t.Error(m)
	}
	for _, name := range []string{"notifications_queue_oldest_due_age_seconds", "notifications_breaker_state",
		"notifications_attempts_total", "notifications_worker_errors_total", "notifications_events_dropped_total"} {
		if !strings.Contains(string(scrapes[0])+string(scrapes[1]), name) {
			t.Errorf("no binary exports %s", name)
		}
	}
}
