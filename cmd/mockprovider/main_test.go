package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMockProviderAnswersTheContractAndCountsPeaks(t *testing.T) {
	srv := httptest.NewServer(newHandler(settings{}, rand.Float64))
	defer srv.Close()
	for _, ch := range []string{"sms", "sms", "sms", "email"} {
		resp := post(t, srv, ch)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	stats := getStats(t, srv)
	if stats.Received != 4 || stats.Peaks["sms"] != 3 || stats.Peaks["email"] != 1 || stats.Peaks["push"] != 0 || stats.Peaks["all"] != 4 {
		t.Fatalf("stats %+v", stats)
	}
}

// The source is a script: each request takes one draw for the failure and, only when it did
// not fail, one more for the throttle. A draw equal to the rate does not inject (the share
// is u < rate). One block of ten requests gives two failures and one throttle, the rates.
func TestMockProviderInjectsFailuresAndThrottlesAtTheirRates(t *testing.T) {
	block := []struct {
		draws []float64
		want  int
	}{
		{[]float64{0.05}, http.StatusServiceUnavailable},
		{[]float64{0.6, 0.7}, http.StatusAccepted},
		{[]float64{0.2, 0.05}, http.StatusTooManyRequests},
		{[]float64{0.95, 0.1}, http.StatusAccepted},
		{[]float64{0.199}, http.StatusServiceUnavailable},
		{[]float64{0.3, 0.3}, http.StatusAccepted},
		{[]float64{0.4, 0.5}, http.StatusAccepted},
		{[]float64{0.7, 0.2}, http.StatusAccepted},
		{[]float64{0.8, 0.95}, http.StatusAccepted},
		{[]float64{0.99, 0.15}, http.StatusAccepted},
	}
	const blocks = 10
	var draws []float64
	var want []int
	for range blocks {
		for _, r := range block {
			draws = append(draws, r.draws...)
			want = append(want, r.want)
		}
	}
	src := &script{draws: draws}

	srv := httptest.NewServer(newHandler(settings{failRate: 0.2, throttleRate: 0.1}, src.next))
	defer srv.Close()
	counts := map[int]int{}
	for i, w := range want {
		resp := post(t, srv, "sms")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != w {
			t.Fatalf("request %d: status %d, want %d", i, resp.StatusCode, w)
		}
		counts[resp.StatusCode]++
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			if string(body) != `{"error":"injected failure"}` {
				t.Fatalf("request %d: 503 body %q", i, body)
			}
		case http.StatusTooManyRequests:
			if got := resp.Header.Get("Retry-After"); got != "1" {
				t.Fatalf("request %d: Retry-After %q, want 1", i, got)
			}
			if string(body) != `{"error":"injected throttle"}` {
				t.Fatalf("request %d: 429 body %q", i, body)
			}
		}
	}
	if counts[http.StatusServiceUnavailable] != 2*blocks || counts[http.StatusTooManyRequests] != blocks ||
		counts[http.StatusAccepted] != 7*blocks {
		t.Fatalf("counts %v", counts)
	}
	if src.used() != len(draws) {
		t.Fatalf("%d draws taken, the script dictates %d", src.used(), len(draws))
	}

	stats := getStats(t, srv)
	if stats.Received != len(want) || stats.Injected.Failures != 2*blocks || stats.Injected.Throttles != blocks {
		t.Fatalf("stats %+v", stats)
	}
}

func TestMockProviderDelaysInjectedAnswersToo(t *testing.T) {
	const latency = 50 * time.Millisecond
	srv := httptest.NewServer(newHandler(settings{latency: latency, failRate: 1}, rand.Float64))
	defer srv.Close()
	start := time.Now()
	resp := post(t, srv, "sms")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if took := time.Since(start); took < latency {
		t.Fatalf("answered after %v, latency is %v", took, latency)
	}
}

// The mock provider counts what it accepted by Idempotency-Key, so that a run can count
// duplicates where they happen: at the provider (CLAUDE.md, rule 9). A key accepted twice is
// counted once among acceptedTwice however often it comes back; a refused answer (503, 429) is
// not an acceptance; a request without a UUID key is accepted and not tracked. Reset forgets.
func TestMockProviderCountsWhatItAcceptedTwice(t *testing.T) {
	src := &script{draws: []float64{
		0.9, 0.9, // a: accepted
		0.9, 0.9, // a: accepted again
		0.9, 0.9, // a: a third time
		0.9, 0.9, // b: accepted
		0.0,      // b: 503, not an acceptance
		0.9, 0.9, // no key: accepted, not tracked
	}}
	srv := httptest.NewServer(newHandler(settings{failRate: 0.5, throttleRate: 0.5}, src.next))
	defer srv.Close()
	a, b := "0b0f8f5e-4a37-4c55-9a1a-7d6f1e2a3b4c", "6e1d2c3b-4a59-4e8f-8a7b-0c1d2e3f4a5b"
	for i, key := range []string{a, a, a, b, b, ""} {
		resp := postWithKey(t, srv, "sms", key)
		_ = resp.Body.Close()
		if want := map[bool]int{true: http.StatusServiceUnavailable, false: http.StatusAccepted}[i == 4]; resp.StatusCode != want {
			t.Fatalf("request %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
	st := getStats(t, srv)
	if st.Accepted != 5 || st.AcceptedTwice != 1 || st.KeysTracked != 2 || st.Received != 6 {
		t.Fatalf("stats %+v; want accepted 5, acceptedTwice 1, keysTracked 2, received 6", st)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/reset", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if st := getStats(t, srv); st.Accepted != 0 || st.AcceptedTwice != 0 || st.KeysTracked != 0 {
		t.Fatalf("after reset: %+v", st)
	}
}

// The keys the mock provider tracks are bounded: past MaxTrackedKeys a new key is accepted and
// counted in accepted, not tracked, so its memory does not grow with a long run.
func TestMockProviderTracksABoundedNumberOfKeys(t *testing.T) {
	h := newHandler(settings{}, func() float64 { return 0.9 })
	srv := httptest.NewServer(h)
	defer srv.Close()
	old := maxTrackedKeys
	maxTrackedKeys = 3
	defer func() { maxTrackedKeys = old }()
	for _, key := range []string{
		"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004",
		"00000000-0000-4000-8000-000000000004",
	} {
		resp := postWithKey(t, srv, "email", key)
		_ = resp.Body.Close()
	}
	if st := getStats(t, srv); st.Accepted != 5 || st.KeysTracked != 3 || st.AcceptedTwice != 0 || st.Untracked != 2 {
		t.Fatalf("stats %+v; want accepted 5, keysTracked 3, acceptedTwice 0, untracked 2", st)
	}
}

func TestMockProviderRefusesARateOutsideZeroToOne(t *testing.T) {
	cases := []struct {
		name, value string
	}{
		{"MOCK_FAIL_RATE", "1.5"},
		{"MOCK_FAIL_RATE", "-0.1"},
		{"MOCK_FAIL_RATE", "NaN"},
		{"MOCK_THROTTLE_RATE", "abc"},
		{"MOCK_THROTTLE_RATE", "1.0001"},
	}
	// The context is already cancelled and the address is a free port, so that a run that
	// wrongly accepts the value stops at once with 0 instead of serving.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range cases {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			env := map[string]string{"MOCK_ADDR": "127.0.0.1:0", c.name: c.value}
			var out bytes.Buffer
			if code := run(ctx, func(k string) string { return env[k] }, &out); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			if !strings.Contains(out.String(), c.name) {
				t.Fatalf("log does not name %s: %s", c.name, out.String())
			}
		})
	}
	t.Run("bounds accepted", func(t *testing.T) {
		env := map[string]string{"MOCK_ADDR": "127.0.0.1:0", "MOCK_FAIL_RATE": "1", "MOCK_THROTTLE_RATE": "0"}
		var out bytes.Buffer
		if code := run(ctx, func(k string) string { return env[k] }, &out); code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, out.String())
		}
	})
}

// script is a random source that returns its draws in order. It is safe for concurrent use.
type script struct {
	mu    sync.Mutex
	draws []float64
	i     int
}

func (s *script) next() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.i == len(s.draws) {
		return 0.999 // past the script: never inject; used() reports the overrun
	}
	s.i++
	return s.draws[s.i-1]
}

func (s *script) used() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.i
}

func post(t *testing.T, srv *httptest.Server, channel string) *http.Response {
	t.Helper()
	return postWithKey(t, srv, channel, "")
}

func postWithKey(t *testing.T, srv *httptest.Server, channel, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/some-uuid",
		strings.NewReader(`{"to":"x","channel":"`+channel+`","content":"c"}`))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type stats struct {
	Received      int            `json:"received"`
	Accepted      int            `json:"accepted"`
	AcceptedTwice int            `json:"acceptedTwice"`
	KeysTracked   int            `json:"keysTracked"`
	Untracked     int            `json:"acceptedUntracked"`
	Peaks         map[string]int `json:"maxInAnyOneSecondWindow"`
	Injected      struct {
		Failures  int `json:"failures"`
		Throttles int `json:"throttles"`
	} `json:"injected"`
}

func getStats(t *testing.T, srv *httptest.Server) stats {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/stats", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var s stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
