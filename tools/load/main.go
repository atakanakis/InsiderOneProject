// Command load sends a burst of notifications to the running stack and measures it from the
// outside (plan, session 8): how fast the API accepts the burst, how much memory the API uses
// meanwhile, how long a single create waits while batches hold the API's rendering slots, how
// long a high-priority notification created during the drain waits until it is sent, how fast
// the backlog drains, and what the mock provider received: its peak in any one second per
// channel, and what it accepted twice.
//
// The burst is -n notifications in batches of up to 1000, -concurrency batches in flight at
// once, the channels in turn (sms, email, push) and the priorities mixed in each batch: 5%
// high, 45% normal, 50% low. While the batches are posted, one single create is made every
// -probe-every; its answer time is the wait for a rendering slot plus the insert. While the
// backlog drains, one high-priority notification per channel is created every -high-every;
// its latency is sentAt minus createdAt, both on the database's clock.
//
// It resets the mock provider's counters first, so it assumes nothing else sends through the
// stack meanwhile. It exits 1 when a request fails, when not everything is sent within
// -timeout, when the provider saw more than -limit requests of one channel in one second, or
// when the provider received or accepted a different number than was created.
//
// Usage: go run ./tools/load [-n 30000] [-concurrency 4] [-content-bytes 0] [-key K]
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	channels   = []string{"sms", "email", "push"}
	recipients = map[string]string{
		"sms":   "+905551234567",
		"email": "user@example.com",
		"push":  "loadDeviceToken0123456789abcdefAB",
	}
	// The content bound of each channel in characters (architecture §8).
	contentBound = map[string]int{"sms": 1600, "email": 100_000, "push": 4096}
)

type config struct {
	api, provider         string
	key                   string
	n, concurrency        int
	contentBytes          int
	probeEvery, highEvery time.Duration
	timeout               time.Duration
	limit                 int
}

func main() {
	var c config
	flag.StringVar(&c.api, "url", "http://localhost:8080", "base URL of the API")
	flag.StringVar(&c.provider, "provider", "http://localhost:9090", "base URL of the mock provider")
	flag.StringVar(&c.key, "key", "", "API key, when the API has API_KEYS set")
	flag.IntVar(&c.n, "n", 30000, "notifications in the burst")
	flag.IntVar(&c.concurrency, "concurrency", 4, "batches posted at once")
	flag.IntVar(&c.contentBytes, "content-bytes", 0, "pad each item's content to this many bytes, up to its channel's bound; 0 keeps it short")
	flag.DurationVar(&c.probeEvery, "probe-every", 100*time.Millisecond, "a single create this often while the burst is posted")
	flag.DurationVar(&c.highEvery, "high-every", 2*time.Second, "a high-priority notification per channel this often while the backlog drains")
	flag.DurationVar(&c.timeout, "timeout", 15*time.Minute, "how long to wait for everything to be sent")
	flag.IntVar(&c.limit, "limit", 100, "the rate limit per channel; a higher peak at the provider fails the run")
	flag.Parse()
	if c.n < 1 || c.concurrency < 1 {
		fail("-n and -concurrency must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	err := run(ctx, c)
	cancel()
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "load: "+format+"\n", args...)
	os.Exit(1)
}

func run(ctx context.Context, c config) error {
	cl := &client{key: c.key, http: &http.Client{Timeout: 60 * time.Second}}
	if err := cl.call(ctx, "POST", c.provider+"/reset", nil, nil, http.StatusNoContent, nil); err != nil {
		return fmt.Errorf("reset the mock provider: %w", err)
	}

	// The API's memory, sampled from its own /metrics until the backlog is drained.
	mem := &memory{}
	memCtx, stopMem := context.WithCancel(ctx)
	memDone := make(chan struct{})
	go func() { defer close(memDone); mem.sample(memCtx, cl, c.api+"/metrics", 250*time.Millisecond) }()
	baseline := mem.now(ctx, cl, c.api+"/metrics")

	// Single creates while the burst is posted.
	probeCtx, stopProbes := context.WithCancel(ctx)
	var singles durations
	var singlesMade atomic.Int64
	probesDone := make(chan error, 1)
	go func() { probesDone <- probe(probeCtx, cl, c, &singles, &singlesMade) }()

	burst, err := postBurst(ctx, cl, c)
	stopProbes()
	if perr := <-probesDone; perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		stopMem()
		return err
	}
	burstMem := mem.peak()

	// High-priority notifications while the backlog drains, then wait for everything.
	highCtx, stopHigh := context.WithCancel(ctx)
	var high highs
	highDone := make(chan error, 1)
	go func() { highDone <- probeHigh(highCtx, cl, c, &high) }()
	drained, err := waitSent(ctx, cl, c, burst)
	stopHigh()
	if herr := <-highDone; herr != nil && err == nil {
		err = herr
	}
	stopMem()
	<-memDone
	if err != nil {
		return err
	}
	if err := high.waitSent(ctx, cl, c); err != nil {
		return err
	}

	var stats providerStats
	if err := cl.call(ctx, "GET", c.provider+"/stats", nil, nil, http.StatusOK, &stats); err != nil {
		return fmt.Errorf("the mock provider's stats: %w", err)
	}
	created := c.n + int(singlesMade.Load()) + high.count()
	report(c, burst, drained, &singles, &high, baseline, burstMem, mem.peak(), stats, created)

	var problems []string
	for _, ch := range channels {
		if stats.Peaks[ch] > c.limit {
			problems = append(problems, fmt.Sprintf("the provider received %d %s requests in one second; the limit is %d", stats.Peaks[ch], ch, c.limit))
		}
	}
	if stats.Received != created || stats.Accepted != created {
		problems = append(problems, fmt.Sprintf("created %d, the provider received %d and accepted %d", created, stats.Received, stats.Accepted))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// burst is what postBurst sent.
type burst struct {
	began, accepted time.Time // the first request; the last 201
	batches         []batchRef
	latency         durations // one per batch, request to answer
	upload          durations // one per batch, request to the end of writing its body
	afterUpload     durations // one per batch, the end of writing its body to the answer
	bodyBytes       int
}

type batchRef struct {
	id, channel string
	total       int
}

func postBurst(ctx context.Context, cl *client, c config) (*burst, error) {
	sizes := []int{}
	for left := c.n; left > 0; left -= 1000 {
		sizes = append(sizes, min(left, 1000))
	}
	b := &burst{batches: make([]batchRef, len(sizes))}
	bodies := make([][]byte, len(sizes))
	for i, size := range sizes {
		ch := channels[i%len(channels)]
		items := make([]map[string]any, size)
		for j := range items {
			items[j] = map[string]any{
				"recipient": recipients[ch], "channel": ch, "priority": priorityOf(j),
				"content": content(fmt.Sprintf("load %s %d/%d", ch, i, j), min(c.contentBytes, contentBound[ch])),
			}
		}
		body, err := json.Marshal(map[string]any{"notifications": items})
		if err != nil {
			return nil, err
		}
		bodies[i] = body
		b.bodyBytes += len(body)
		b.batches[i] = batchRef{channel: ch, total: size}
	}
	work := make(chan int)
	errs := make(chan error, c.concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	b.began = time.Now()
	for range c.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				var created struct {
					BatchID string `json:"batchId"`
					Total   int    `json:"total"`
				}
				t0 := time.Now()
				var wrote time.Time // when the client finished writing the body to its socket. On
				// Docker Desktop the host's port forwarder takes the bytes, so this is not when the
				// API has read them: it tells a slow client from the rest, not the upload into
				// Docker from the API (session 8's review, F4)
				tctx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
					WroteRequest: func(httptrace.WroteRequestInfo) { wrote = time.Now() },
				})
				header := http.Header{"Idempotency-Key": {"load-" + randomHex()}}
				if err := cl.callRaw(tctx, "POST", c.api+"/v1/notifications/batch", bodies[i], header, http.StatusCreated, &created); err != nil {
					errs <- fmt.Errorf("batch %d: %w", i, err)
					return
				}
				took := time.Since(t0)
				mu.Lock()
				b.batches[i].id = created.BatchID
				b.latency.add(took)
				b.upload.add(wrote.Sub(t0))
				b.afterUpload.add(t0.Add(took).Sub(wrote))
				b.accepted = time.Now()
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer close(work)
		for i := range bodies {
			select {
			case work <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return b, ctx.Err()
}

// priorityOf mixes the priorities of a batch: 5% high, 45% normal, 50% low.
func priorityOf(j int) string {
	switch {
	case j%20 == 0:
		return "high"
	case j%20 < 10:
		return "normal"
	}
	return "low"
}

func content(prefix string, size int) string {
	if size <= len(prefix) {
		return prefix
	}
	return prefix + " " + strings.Repeat("x", size-len(prefix)-1)
}

// probe makes one single create every c.probeEvery until ctx ends, and records its answer time.
func probe(ctx context.Context, cl *client, c config, d *durations, made *atomic.Int64) error {
	t := time.NewTicker(c.probeEvery)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		ch := channels[i%len(channels)]
		req := map[string]any{"recipient": recipients[ch], "channel": ch, "content": "load single", "priority": "low"}
		t0 := time.Now()
		var created struct{ ID string }
		err := cl.call(context.WithoutCancel(ctx), "POST", c.api+"/v1/notifications", req, nil, http.StatusCreated, &created)
		if err != nil {
			return fmt.Errorf("a single create during the burst: %w", err)
		}
		d.add(time.Since(t0))
		made.Add(1)
	}
}

// highs are the high-priority notifications created while the backlog drains.
type highs struct {
	mu      sync.Mutex
	ids     map[string]string          // id → channel
	latency map[string][]time.Duration // channel → sentAt minus createdAt
}

func (h *highs) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

func probeHigh(ctx context.Context, cl *client, c config, h *highs) error {
	h.ids = map[string]string{}
	t := time.NewTicker(c.highEvery)
	defer t.Stop()
	for {
		for _, ch := range channels {
			req := map[string]any{"recipient": recipients[ch], "channel": ch, "content": "load high", "priority": "high"}
			var created struct{ ID string }
			if err := cl.call(context.WithoutCancel(ctx), "POST", c.api+"/v1/notifications", req, nil, http.StatusCreated, &created); err != nil {
				return fmt.Errorf("a high-priority create during the drain: %w", err)
			}
			h.mu.Lock()
			h.ids[created.ID] = ch
			h.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// waitSent waits until every high-priority notification is sent and records, per channel,
// sentAt minus createdAt.
func (h *highs) waitSent(ctx context.Context, cl *client, c config) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latency = map[string][]time.Duration{}
	for id := range h.ids {
		for {
			var s struct {
				Status    string     `json:"status"`
				CreatedAt time.Time  `json:"createdAt"`
				SentAt    *time.Time `json:"sentAt"`
			}
			if err := cl.call(ctx, "GET", c.api+"/v1/notifications/"+id, nil, nil, http.StatusOK, &s); err != nil {
				return err
			}
			if s.Status == "sent" && s.SentAt != nil {
				h.latency[h.ids[id]] = append(h.latency[h.ids[id]], s.SentAt.Sub(s.CreatedAt))
				break
			}
			if s.Status == "failed" || s.Status == "cancelled" {
				return fmt.Errorf("high-priority notification %s ended %s", id, s.Status)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}

// drain is when each channel's batches were all sent.
type drain struct {
	done map[string]time.Time
}

func waitSent(ctx context.Context, cl *client, c config, b *burst) (*drain, error) {
	d := &drain{done: map[string]time.Time{}}
	left := map[string]int{}
	for _, ref := range b.batches {
		left[ref.channel]++
	}
	sent := make([]bool, len(b.batches))
	for {
		for i, ref := range b.batches {
			if sent[i] {
				continue
			}
			var s struct {
				Total  int            `json:"total"`
				Counts map[string]int `json:"counts"`
			}
			if err := cl.call(ctx, "GET", c.api+"/v1/batches/"+ref.id, nil, nil, http.StatusOK, &s); err != nil {
				return nil, fmt.Errorf("batch %s: %w", ref.id, err)
			}
			if s.Counts["failed"] > 0 || s.Counts["cancelled"] > 0 {
				return nil, fmt.Errorf("batch %s will not be sent completely: %v", ref.id, s.Counts)
			}
			if s.Counts["sent"] == ref.total {
				sent[i] = true
				if left[ref.channel]--; left[ref.channel] == 0 {
					d.done[ref.channel] = time.Now()
				}
			}
		}
		if len(d.done) == len(channels) {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("not drained: %w (channels done: %v)", ctx.Err(), d.done)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type providerStats struct {
	Received      int            `json:"received"`
	Accepted      int            `json:"accepted"`
	AcceptedTwice int            `json:"acceptedTwice"`
	Peaks         map[string]int `json:"maxInAnyOneSecondWindow"`
}

func report(c config, b *burst, d *drain, singles *durations, h *highs, baseline, burstPeak, drainPeak memSample, stats providerStats, created int) {
	acceptFor := b.accepted.Sub(b.began)
	fmt.Printf("burst: %d notifications in %d batches (%.1f MB of JSON), %d at once, content %d bytes\n",
		c.n, len(b.batches), float64(b.bodyBytes)/1e6, c.concurrency, c.contentBytes)
	fmt.Printf("accepted: in %v, %.0f notifications per second; a batch answered in %s\n",
		acceptFor.Round(time.Millisecond), float64(c.n)/acceptFor.Seconds(), b.latency.summary())
	fmt.Printf("  of which writing the body to the local socket: %s; from then to the answer: %s\n", b.upload.summary(), b.afterUpload.summary())
	fmt.Printf("single creates during the burst: %d, answered in %s\n", singles.len(), singles.summary())
	fmt.Printf("API memory (resident / Go heap in use): before %s, peak during the burst %s, peak until drained %s\n",
		baseline, burstPeak, drainPeak)
	perChannel := map[string]int{}
	for _, ref := range b.batches {
		perChannel[ref.channel] += ref.total
	}
	var total float64
	last := b.accepted
	for _, ch := range channels {
		took := d.done[ch].Sub(b.began)
		total += float64(perChannel[ch])
		if d.done[ch].After(last) {
			last = d.done[ch]
		}
		fmt.Printf("drained %-5s: %d in %v from the first request, %.1f per second; provider peak %d in one second\n",
			ch, perChannel[ch], took.Round(100*time.Millisecond), float64(perChannel[ch])/took.Seconds(), stats.Peaks[ch])
	}
	fmt.Printf("drained all: %d in %v, %.1f per second\n", int(total), last.Sub(b.began).Round(100*time.Millisecond), total/last.Sub(b.began).Seconds())
	for _, ch := range channels {
		var dd durations
		for _, l := range h.latency[ch] {
			dd.add(l)
		}
		fmt.Printf("high priority %-5s during the drain: %d, created to sent %s\n", ch, dd.len(), dd.summary())
	}
	fmt.Printf("provider: received %d, accepted %d, accepted twice %d; created %d\n",
		stats.Received, stats.Accepted, stats.AcceptedTwice, created)
}

// durations collects samples safely and summarises them.
type durations struct {
	mu sync.Mutex
	d  []time.Duration
}

func (d *durations) add(x time.Duration) {
	d.mu.Lock()
	d.d = append(d.d, x)
	d.mu.Unlock()
}

func (d *durations) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.d)
}

func (d *durations) summary() string {
	d.mu.Lock()
	s := slices.Clone(d.d)
	d.mu.Unlock()
	if len(s) == 0 {
		return "(none)"
	}
	slices.Sort(s)
	q := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	r := func(x time.Duration) time.Duration {
		if x < 10*time.Millisecond {
			return x.Round(100 * time.Microsecond)
		}
		return x.Round(time.Millisecond)
	}
	return fmt.Sprintf("median %v, p95 %v, p99 %v, max %v", r(q(0.5)), r(q(0.95)), r(q(0.99)), r(s[len(s)-1]))
}

// memory samples the API's resident memory and Go heap in use from its /metrics.
type memory struct {
	mu  sync.Mutex
	max memSample
}

type memSample struct{ resident, heap float64 }

func (m memSample) String() string {
	return fmt.Sprintf("%.0f / %.0f MiB", m.resident/(1<<20), m.heap/(1<<20))
}

func (m *memory) peak() memSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.max
}

func (m *memory) now(ctx context.Context, cl *client, url string) memSample {
	s, _ := scrape(ctx, cl, url)
	return s
}

func (m *memory) sample(ctx context.Context, cl *client, url string, every time.Duration) {
	for {
		if s, err := scrape(ctx, cl, url); err == nil {
			m.mu.Lock()
			m.max.resident, m.max.heap = max(m.max.resident, s.resident), max(m.max.heap, s.heap)
			m.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func scrape(ctx context.Context, cl *client, url string) (memSample, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return memSample{}, err
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		return memSample{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var s memSample
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		for name, dst := range map[string]*float64{"process_resident_memory_bytes ": &s.resident, "go_memstats_heap_inuse_bytes ": &s.heap} {
			if v, ok := strings.CutPrefix(line, name); ok {
				*dst, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
		}
	}
	return s, sc.Err()
}

type client struct {
	key  string
	http *http.Client
}

func (c *client) call(ctx context.Context, method, url string, in any, header http.Header, want int, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	return c.callRaw(ctx, method, url, body, header, want, out)
}

// callRaw sends body (JSON, or nothing when nil) and decodes the answer into out (when not
// nil). An answer with another status than want is an error with the start of the body.
func (c *client) callRaw(ctx context.Context, method, url string, body []byte, header http.Header, want int, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, url, err)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s answered %d, want %d: %.300s", method, url, resp.StatusCode, want, got)
	}
	if out != nil {
		if err := json.Unmarshal(got, out); err != nil {
			return fmt.Errorf("%s %s: the answer is not the expected JSON: %w", method, url, err)
		}
	}
	return nil
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
