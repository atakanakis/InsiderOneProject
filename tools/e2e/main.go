// Command e2e sends notifications through the real API and waits until every one is sent.
//
// It first follows one notification over the WebSocket (architecture §9): it creates an SMS
// scheduled 3 s ahead, opens GET /v1/ws for it, and expects the current state, scheduled, then
// the worker's processing and sent. It then creates one SMS notification with an idempotency
// key and repeats the request, which must answer 200 with the same id. It then posts one batch of n notifications per channel (sms,
// email, push), with mixed priorities and an Idempotency-Key header, and polls
// GET /v1/batches/{id} every 500 ms until every batch has sent all of its notifications, and
// GET /v1/notifications/{id} until the single one is sent with one attempt. It prints one line
// per batch and exits 1 with the reason on a timeout or on any unexpected answer.
//
// Usage: go run ./tools/e2e -url http://localhost:8080 -n 500 -timeout 120s [-key K]
package main

import (
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
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const pollEvery = 500 * time.Millisecond

var channels = []string{"sms", "email", "push"}

func main() {
	base := flag.String("url", "http://localhost:8080", "base URL of the API")
	n := flag.Int("n", 100, "notifications per channel, 1 to 1000 (one batch each)")
	timeout := flag.Duration("timeout", 120*time.Second, "how long to wait for everything to be sent")
	key := flag.String("key", "", "API key, when the API has API_KEYS set")
	flag.Parse()
	if *n < 1 || *n > 1000 {
		fail("-n must be 1 to 1000 (the batch bound), got %d", *n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	c := &client{base: strings.TrimRight(*base, "/"), key: *key, http: &http.Client{Timeout: 10 * time.Second}}
	err := run(ctx, c, *n)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fail("not finished within %s: %v", *timeout, err)
		}
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e: "+format+"\n", args...)
	os.Exit(1)
}

type batch struct {
	channel string
	id      string
	total   int
	created time.Time
	done    bool
}

type counts struct {
	Scheduled, Pending, Processing, Retrying, Sent, Failed, Cancelled int
}

func (c counts) String() string {
	return fmt.Sprintf("scheduled %d, pending %d, processing %d, retrying %d, sent %d, failed %d, cancelled %d",
		c.Scheduled, c.Pending, c.Processing, c.Retrying, c.Sent, c.Failed, c.Cancelled)
}

func run(ctx context.Context, c *client, n int) error {
	began := time.Now()
	if err := followOne(ctx, c); err != nil {
		return err
	}
	single, err := createSingle(ctx, c)
	if err != nil {
		return err
	}
	batches := make([]*batch, 0, len(channels))
	for _, ch := range channels {
		b, err := createBatch(ctx, c, ch, n)
		if err != nil {
			return err
		}
		batches = append(batches, b)
	}

	singleSent := false
	last := map[string]counts{}
	for {
		left := 0
		for _, b := range batches {
			if b.done {
				continue
			}
			var s struct {
				Total  int    `json:"total"`
				Counts counts `json:"counts"`
			}
			if err := c.call(ctx, "GET", "/v1/batches/"+b.id, nil, nil, http.StatusOK, &s); err != nil {
				return fmt.Errorf("%s batch %s: %w (last counts: %s)", b.channel, b.id, err, last[b.channel])
			}
			last[b.channel] = s.Counts
			if s.Total != b.total {
				return fmt.Errorf("%s batch %s: total %d, created with %d", b.channel, b.id, s.Total, b.total)
			}
			if s.Counts.Failed > 0 || s.Counts.Cancelled > 0 {
				return fmt.Errorf("%s batch %s will not be sent completely: %s", b.channel, b.id, s.Counts)
			}
			if s.Counts.Sent == b.total {
				b.done = true
				fmt.Printf("%-5s batch %s: %d/%d sent in %s\n", b.channel, b.id, s.Counts.Sent, b.total,
					time.Since(b.created).Round(10*time.Millisecond))
				continue
			}
			left++
		}
		if !singleSent {
			var s struct {
				Status         string            `json:"status"`
				AttemptHistory []json.RawMessage `json:"attemptHistory"`
			}
			if err := c.call(ctx, "GET", "/v1/notifications/"+single, nil, nil, http.StatusOK, &s); err != nil {
				return fmt.Errorf("single notification %s: %w", single, err)
			}
			switch s.Status {
			case "sent":
				if len(s.AttemptHistory) != 1 {
					return fmt.Errorf("single notification %s: sent with %d attempts in its history, want 1",
						single, len(s.AttemptHistory))
				}
				singleSent = true
				fmt.Printf("single notification %s: sent with 1 attempt\n", single)
			case "failed", "cancelled":
				return fmt.Errorf("single notification %s ended %s", single, s.Status)
			default:
				left++
			}
		}
		if left == 0 {
			fmt.Printf("all %d notifications sent in %s\n", 3*n+2, time.Since(began).Round(10*time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			var waiting []string
			for _, b := range batches {
				if !b.done {
					waiting = append(waiting, fmt.Sprintf("%s batch (%s)", b.channel, last[b.channel]))
				}
			}
			if !singleSent {
				waiting = append(waiting, "the single notification")
			}
			return fmt.Errorf("%w; still waiting for %s", ctx.Err(), strings.Join(waiting, ", "))
		case <-time.After(pollEvery):
		}
	}
}

// followOne creates a notification scheduled 3 s ahead and follows it over the WebSocket: the
// current state first, scheduled, then processing and sent, from the worker through Redis.
func followOne(ctx context.Context, c *client) error {
	at := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	var created struct {
		ID string `json:"id"`
	}
	req := map[string]any{"recipient": "+905551234567", "channel": "sms", "content": "e2e followed", "scheduledAt": at}
	if err := c.call(ctx, "POST", "/v1/notifications", req, nil, http.StatusCreated, &created); err != nil {
		return fmt.Errorf("create the followed notification: %w", err)
	}
	header := http.Header{}
	if c.key != "" {
		header.Set("X-API-Key", c.key)
	}
	wsURL := "ws" + strings.TrimPrefix(c.base, "http") + "/v1/ws?notificationId=" + created.ID
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose // the library closes it ("You never need to close resp.Body yourself")
	if err != nil {
		return fmt.Errorf("open the WebSocket: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	var seen []string
	for len(seen) == 0 || seen[len(seen)-1] != "sent" {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("WebSocket of %s after %v: %w", created.ID, seen, err)
		}
		var m struct{ Type, ID, Status string }
		if err := json.Unmarshal(b, &m); err != nil || m.Type != "status" || m.ID != created.ID {
			return fmt.Errorf("WebSocket of %s: an unexpected message %.200s", created.ID, b)
		}
		seen = append(seen, m.Status)
	}
	if seen[0] != "scheduled" || !strings.Contains(strings.Join(seen, ","), "processing,sent") {
		return fmt.Errorf("WebSocket of %s: %v, want scheduled first, then processing and sent", created.ID, seen)
	}
	fmt.Printf("websocket: notification %s: %s\n", created.ID, strings.Join(seen, ", "))
	return nil
}

// createSingle creates one notification with an idempotency key, repeats the request, and
// checks that the repeat answers 200 with the same id.
func createSingle(ctx context.Context, c *client) (string, error) {
	req := map[string]any{
		"recipient":      "+905551234567",
		"channel":        "sms",
		"content":        "e2e single",
		"priority":       "high",
		"idempotencyKey": "e2e-" + randomHex(),
	}
	var first, again struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "POST", "/v1/notifications", req, nil, http.StatusCreated, &first); err != nil {
		return "", fmt.Errorf("create the single notification: %w", err)
	}
	if err := c.call(ctx, "POST", "/v1/notifications", req, nil, http.StatusOK, &again); err != nil {
		return "", fmt.Errorf("repeat the single notification with its idempotency key: %w", err)
	}
	if first.ID == "" || again.ID != first.ID {
		return "", fmt.Errorf("the repeated request answered id %q, the first %q", again.ID, first.ID)
	}
	fmt.Printf("single notification %s: created; the repeat with its key answered 200 with the same id\n", first.ID)
	return first.ID, nil
}

// createBatch posts n notifications of one channel as one batch.
func createBatch(ctx context.Context, c *client, channel string, n int) (*batch, error) {
	recipients := map[string]string{
		"sms":   "+905551234567",
		"email": "user@example.com",
		"push":  "e2eDeviceToken0123456789abcdefAB", // 32 letters and digits
	}
	priorities := []string{"high", "normal", "low"}
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{
			"recipient": recipients[channel],
			"channel":   channel,
			"content":   fmt.Sprintf("e2e %s %d", channel, i),
			"priority":  priorities[i%len(priorities)],
		}
	}
	var created struct {
		BatchID string   `json:"batchId"`
		Total   int      `json:"total"`
		IDs     []string `json:"ids"`
	}
	header := http.Header{"Idempotency-Key": {"e2e-batch-" + randomHex()}}
	began := time.Now()
	if err := c.call(ctx, "POST", "/v1/notifications/batch", map[string]any{"notifications": items}, header,
		http.StatusCreated, &created); err != nil {
		return nil, fmt.Errorf("create the %s batch: %w", channel, err)
	}
	if created.BatchID == "" || created.Total != n || len(created.IDs) != n {
		return nil, fmt.Errorf("create the %s batch: answered batchId %q, total %d, %d ids; want %d",
			channel, created.BatchID, created.Total, len(created.IDs), n)
	}
	return &batch{channel: channel, id: created.BatchID, total: n, created: began}, nil
}

type client struct {
	base string
	key  string
	http *http.Client
}

// call sends a JSON request and decodes the answer into out. An answer with another status
// code than want is an error that shows the code and the start of the body.
func (c *client) call(ctx context.Context, method, path string, in any, header http.Header, want int, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s answered %d, want %d: %.300s", method, path, resp.StatusCode, want, got)
	}
	if err := json.Unmarshal(got, out); err != nil {
		return fmt.Errorf("%s %s: the answer is not the expected JSON: %w", method, path, err)
	}
	return nil
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
