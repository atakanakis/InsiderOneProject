package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// The specification's paths: path, then method, then the status codes of the responses.
type openAPISpec struct {
	Paths map[string]map[string]struct {
		Responses map[string]any `yaml:"responses"`
	} `yaml:"paths"`
}

var specMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

// specOperations returns every "METHOD path" of openapi.yaml with its status codes.
func specOperations(t *testing.T) map[string][]int {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec openAPISpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	ops := map[string][]int{}
	for path, item := range spec.Paths {
		for method, op := range item {
			if !slices.Contains(specMethods, method) {
				continue // path-level keys such as parameters
			}
			key := strings.ToUpper(method) + " " + path
			if len(op.Responses) == 0 {
				t.Errorf("%s has no responses", key)
			}
			for code := range op.Responses {
				n, err := strconv.Atoi(code)
				if err != nil || n < 100 || n > 599 {
					t.Errorf("%s: response %q is not a status code", key, code)
					continue
				}
				ops[key] = append(ops[key], n)
			}
		}
	}
	return ops
}

// openAPIServer is the real handler on a real HTTP server, and a second one whose Postgres
// check fails, for the 503 of /readyz.
type openAPIServer struct {
	t        *testing.T
	url      string
	downTo   string
	brokenTo string // the real handler on a database pool that is closed: every query fails
	exec     func(sql string, args ...any)
}

const testAPIKey = "test-key-0123456789"

// do sends one request with the API key and returns its status code. The body of a /v1
// error must have the error form of architecture §8.
func (s *openAPIServer) do(method, path, body string, header ...string) int {
	code, _ := s.send(s.url, method, path, body, append([]string{"X-API-Key", testAPIKey}, header...)...)
	return code
}

// anonymous sends one request without a key.
func (s *openAPIServer) anonymous(method, path, body string) int {
	code, _ := s.send(s.url, method, path, body)
	return code
}

func (s *openAPIServer) send(base, method, path, body string, header ...string) (int, []byte) {
	s.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, base+path, r)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	if strings.HasPrefix(path, "/v1/") && resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got, &e); err != nil || e.Error.Code == "" || e.Error.Message == "" {
			s.t.Errorf("%s %s: %d without the error form: %.200s", method, path, resp.StatusCode, got)
		}
	}
	return resp.StatusCode, got
}

// notification inserts a notification row in the given status and returns its id.
func (s *openAPIServer) notification(status string) string {
	id := uuid.NewString()
	s.exec(`INSERT INTO notifications (id, recipient, channel, content, priority, status,
		attempts, max_attempts, ready_at) VALUES ($1, '+905551234567', 'sms', 'hello', 1, $2, 6, 6, now())`,
		id, status)
	return id
}

// template inserts a template and returns its name.
func (s *openAPIServer) template() string {
	name := "t-" + uuid.NewString()[:8]
	s.exec(`INSERT INTO templates (name, channel, body) VALUES ($1, 'sms', 'Hi {{.name}}')`, name)
	return name
}

// Request bodies and bounds of architecture §8.
const (
	maxBodyBytes      = 1 << 20
	maxBatchBodyBytes = 8 << 20
)

const smsJSON = `{"recipient": "+905551234567", "channel": "sms", "content": "hello"}`

// overTheBound pads a valid JSON body with whitespace to one byte over the bound.
func overTheBound(body string, bound int) string {
	return body + strings.Repeat(" ", bound+1-len(body))
}

// keyed returns a request with a new idempotency key.
func keyed() string {
	return `{"recipient": "+905551234567", "channel": "sms", "content": "hello", "idempotencyKey": "key-` + uuid.NewString() + `"}`
}

func batchJSON(content string) string {
	return `{"notifications": [{"recipient": "user@example.com", "channel": "email", "content": "` + content + `"}]}`
}

// openAPIScenarios produce each (method, path, status code) of the specification on the
// real server, keyed "METHOD path code" as in the specification. Each returns the code it got.
var openAPIScenarios = map[string]func(s *openAPIServer) int{
	// Create.
	"POST /v1/notifications 201": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications", keyed())
	},
	"POST /v1/notifications 200": func(s *openAPIServer) int { // the same key and request again
		body := keyed()
		s.do("POST", "/v1/notifications", body)
		return s.do("POST", "/v1/notifications", body)
	},
	"POST /v1/notifications 400": func(s *openAPIServer) int { // an unknown channel
		return s.do("POST", "/v1/notifications", `{"recipient": "+905551234567", "channel": "fax", "content": "hello"}`)
	},
	"POST /v1/notifications 401": func(s *openAPIServer) int {
		return s.anonymous("POST", "/v1/notifications", smsJSON)
	},
	"POST /v1/notifications 409": func(s *openAPIServer) int { // the same key, another content
		body := keyed()
		s.do("POST", "/v1/notifications", body)
		return s.do("POST", "/v1/notifications", strings.Replace(body, `"hello"`, `"bye"`, 1))
	},
	"POST /v1/notifications 413": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications", overTheBound(smsJSON, maxBodyBytes))
	},

	// Batch.
	"POST /v1/notifications/batch 201": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/batch", batchJSON("hello"))
	},
	"POST /v1/notifications/batch 200": func(s *openAPIServer) int { // the same header and batch again
		key := "batch-" + uuid.NewString()
		s.do("POST", "/v1/notifications/batch", batchJSON("hello"), "Idempotency-Key", key)
		return s.do("POST", "/v1/notifications/batch", batchJSON("hello"), "Idempotency-Key", key)
	},
	"POST /v1/notifications/batch 400": func(s *openAPIServer) int { // an item with an unknown channel
		return s.do("POST", "/v1/notifications/batch", `{"notifications": [{"recipient": "x", "channel": "fax", "content": "hello"}]}`)
	},
	"POST /v1/notifications/batch 401": func(s *openAPIServer) int {
		return s.anonymous("POST", "/v1/notifications/batch", batchJSON("hello"))
	},
	"POST /v1/notifications/batch 409": func(s *openAPIServer) int { // the same header, another batch
		key := "batch-" + uuid.NewString()
		s.do("POST", "/v1/notifications/batch", batchJSON("hello"), "Idempotency-Key", key)
		return s.do("POST", "/v1/notifications/batch", batchJSON("bye"), "Idempotency-Key", key)
	},
	"POST /v1/notifications/batch 413": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/batch", overTheBound(batchJSON("hello"), maxBatchBodyBytes))
	},

	// Status.
	"GET /v1/notifications/{id} 200": func(s *openAPIServer) int {
		return s.do("GET", "/v1/notifications/"+s.notification("pending"), "")
	},
	"GET /v1/notifications/{id} 400": func(s *openAPIServer) int {
		return s.do("GET", "/v1/notifications/not-a-uuid", "")
	},
	"GET /v1/notifications/{id} 401": func(s *openAPIServer) int {
		return s.anonymous("GET", "/v1/notifications/"+s.notification("pending"), "")
	},
	"GET /v1/notifications/{id} 404": func(s *openAPIServer) int {
		return s.do("GET", "/v1/notifications/"+uuid.NewString(), "")
	},

	// Batch summary.
	"GET /v1/batches/{id} 200": func(s *openAPIServer) int {
		code, body := s.send(s.url, "POST", "/v1/notifications/batch", batchJSON("hello"), "X-API-Key", testAPIKey)
		var b struct {
			BatchID string `json:"batchId"`
		}
		if err := json.Unmarshal(body, &b); code != http.StatusCreated || err != nil {
			s.t.Fatalf("create a batch: %d %.200s", code, body)
		}
		return s.do("GET", "/v1/batches/"+b.BatchID, "")
	},
	"GET /v1/batches/{id} 400": func(s *openAPIServer) int { return s.do("GET", "/v1/batches/not-a-uuid", "") },
	"GET /v1/batches/{id} 401": func(s *openAPIServer) int { return s.anonymous("GET", "/v1/batches/"+uuid.NewString(), "") },
	"GET /v1/batches/{id} 404": func(s *openAPIServer) int { return s.do("GET", "/v1/batches/"+uuid.NewString(), "") },

	// List.
	"GET /v1/notifications 200": func(s *openAPIServer) int {
		s.notification("pending")
		return s.do("GET", "/v1/notifications?status=pending&channel=sms&limit=10", "")
	},
	"GET /v1/notifications 400": func(s *openAPIServer) int { return s.do("GET", "/v1/notifications?limit=101", "") },
	"GET /v1/notifications 401": func(s *openAPIServer) int { return s.anonymous("GET", "/v1/notifications", "") },

	// Cancel and retry.
	"POST /v1/notifications/{id}/cancel 200": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/"+s.notification("pending")+"/cancel", "")
	},
	"POST /v1/notifications/{id}/cancel 400": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/not-a-uuid/cancel", "")
	},
	"POST /v1/notifications/{id}/cancel 401": func(s *openAPIServer) int {
		return s.anonymous("POST", "/v1/notifications/"+s.notification("pending")+"/cancel", "")
	},
	"POST /v1/notifications/{id}/cancel 404": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/"+uuid.NewString()+"/cancel", "")
	},
	"POST /v1/notifications/{id}/cancel 409": func(s *openAPIServer) int { // already sent
		return s.do("POST", "/v1/notifications/"+s.notification("sent")+"/cancel", "")
	},
	"POST /v1/notifications/{id}/retry 200": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/"+s.notification("failed")+"/retry", "")
	},
	"POST /v1/notifications/{id}/retry 400": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/not-a-uuid/retry", "")
	},
	"POST /v1/notifications/{id}/retry 401": func(s *openAPIServer) int {
		return s.anonymous("POST", "/v1/notifications/"+s.notification("failed")+"/retry", "")
	},
	"POST /v1/notifications/{id}/retry 404": func(s *openAPIServer) int {
		return s.do("POST", "/v1/notifications/"+uuid.NewString()+"/retry", "")
	},
	"POST /v1/notifications/{id}/retry 409": func(s *openAPIServer) int { // sent, not failed
		return s.do("POST", "/v1/notifications/"+s.notification("sent")+"/retry", "")
	},

	// Templates.
	"POST /v1/templates 201": func(s *openAPIServer) int {
		return s.do("POST", "/v1/templates", `{"name": "t-`+uuid.NewString()[:8]+`", "channel": "sms", "body": "Hi {{.name}}"}`)
	},
	"POST /v1/templates 200": func(s *openAPIServer) int { // the same name, channel and body
		return s.do("POST", "/v1/templates", `{"name": "`+s.template()+`", "channel": "sms", "body": "Hi {{.name}}"}`)
	},
	"POST /v1/templates 400": func(s *openAPIServer) int { // no name
		return s.do("POST", "/v1/templates", `{"name": "", "channel": "sms", "body": "Hi"}`)
	},
	"POST /v1/templates 401": func(s *openAPIServer) int {
		return s.anonymous("POST", "/v1/templates", `{"name": "t", "channel": "sms", "body": "Hi"}`)
	},
	"POST /v1/templates 409": func(s *openAPIServer) int { // the same name, another body
		return s.do("POST", "/v1/templates", `{"name": "`+s.template()+`", "channel": "sms", "body": "Hello {{.name}}"}`)
	},
	"POST /v1/templates 413": func(s *openAPIServer) int {
		return s.do("POST", "/v1/templates", overTheBound(`{"name": "t", "channel": "sms", "body": "Hi"}`, maxBodyBytes))
	},
	"GET /v1/templates 200": func(s *openAPIServer) int {
		s.template()
		return s.do("GET", "/v1/templates", "")
	},
	"GET /v1/templates 400": func(s *openAPIServer) int { return s.do("GET", "/v1/templates?cursor=%21%21%21", "") },
	"GET /v1/templates 401": func(s *openAPIServer) int { return s.anonymous("GET", "/v1/templates", "") },
	"GET /v1/templates/{name} 200": func(s *openAPIServer) int {
		return s.do("GET", "/v1/templates/"+s.template(), "")
	},
	"GET /v1/templates/{name} 401": func(s *openAPIServer) int {
		return s.anonymous("GET", "/v1/templates/"+s.template(), "")
	},
	"GET /v1/templates/{name} 404": func(s *openAPIServer) int { return s.do("GET", "/v1/templates/no-such-template", "") },

	// Open endpoints: no key needed.
	"GET /healthz 200":      func(s *openAPIServer) int { return s.anonymous("GET", "/healthz", "") },
	"GET /readyz 200":       func(s *openAPIServer) int { return s.anonymous("GET", "/readyz", "") },
	"GET /readyz 503":       func(s *openAPIServer) int { code, _ := s.send(s.downTo, "GET", "/readyz", ""); return code },
	"GET /openapi.yaml 200": func(s *openAPIServer) int { return s.anonymous("GET", "/openapi.yaml", "") },
	"GET /docs 200":         func(s *openAPIServer) int { return s.anonymous("GET", "/docs", "") },
}

// broken sends one request with the API key to the server whose database fails.
func (s *openAPIServer) broken(method, path, body string) int {
	code, _ := s.send(s.brokenTo, method, path, body, "X-API-Key", testAPIKey)
	return code
}

// The 500 of every /v1 operation: a request that passes validation reaches a database that
// fails.
func init() {
	id := uuid.NewString()
	for name, run := range map[string]func(s *openAPIServer) int{
		"POST /v1/notifications 500":             func(s *openAPIServer) int { return s.broken("POST", "/v1/notifications", smsJSON) },
		"POST /v1/notifications/batch 500":       func(s *openAPIServer) int { return s.broken("POST", "/v1/notifications/batch", batchJSON("hello")) },
		"GET /v1/notifications/{id} 500":         func(s *openAPIServer) int { return s.broken("GET", "/v1/notifications/"+id, "") },
		"GET /v1/notifications 500":              func(s *openAPIServer) int { return s.broken("GET", "/v1/notifications", "") },
		"POST /v1/notifications/{id}/cancel 500": func(s *openAPIServer) int { return s.broken("POST", "/v1/notifications/"+id+"/cancel", "") },
		"POST /v1/notifications/{id}/retry 500":  func(s *openAPIServer) int { return s.broken("POST", "/v1/notifications/"+id+"/retry", "") },
		"GET /v1/batches/{id} 500":               func(s *openAPIServer) int { return s.broken("GET", "/v1/batches/"+id, "") },
		"POST /v1/templates 500": func(s *openAPIServer) int {
			return s.broken("POST", "/v1/templates", `{"name": "t", "channel": "sms", "body": "hi"}`)
		},
		"GET /v1/templates 500":        func(s *openAPIServer) int { return s.broken("GET", "/v1/templates", "") },
		"GET /v1/templates/{name} 500": func(s *openAPIServer) int { return s.broken("GET", "/v1/templates/t", "") },
	} {
		openAPIScenarios[name] = run
	}
}

// architecture §8: every path, method and status code of the specification is produced by
// the real server, and every route of the server is in the specification.
func TestOpenAPIMatchesTheServer(t *testing.T) {
	ops := specOperations(t)

	// Both directions between the routes of the server and the operations of the spec.
	routes := map[string]bool{}
	for _, p := range routePatterns() {
		method, path, ok := strings.Cut(p, " ")
		if !ok || !strings.HasPrefix(path, "/") {
			t.Errorf("route %q has no method or is bound to a host", p)
			continue
		}
		routes[method+" "+strings.ReplaceAll(path, "...}", "}")] = true
	}
	for r := range routes {
		if _, ok := ops[r]; !ok {
			t.Errorf("route %s is not in openapi.yaml", r)
		}
	}
	for op := range ops {
		if !routes[op] {
			t.Errorf("openapi.yaml has %s, which the server does not route", op)
		}
	}

	// Every (method, path, code) of the spec against the real server, and no scenario for a
	// code the spec does not list.
	listed := map[string]bool{}
	for op, codes := range ops {
		for _, c := range codes {
			listed[fmt.Sprintf("%s %d", op, c)] = true
		}
	}
	for name := range openAPIScenarios {
		if !listed[name] {
			t.Errorf("scenario %q is for a response openapi.yaml does not list", name)
		}
	}

	env := testkit.Setup(t)
	log := observability.NewLogger(&bytes.Buffer{}, "api", "error")
	up := httptest.NewServer(NewHandler(log, Options{
		Health:      Health{Postgres: env.Pool.Ping, Redis: ok},
		Pool:        env.Pool,
		APIKeys:     []string{testAPIKey},
		MaxAttempts: 6,
	}))
	t.Cleanup(up.Close)
	down := httptest.NewServer(NewHandler(log, Options{Health: Health{Postgres: failing, Redis: ok}}))
	t.Cleanup(down.Close)
	closed, err := store.Connect(context.Background(), env.DatabaseURL, DatabaseOptions)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	broken := httptest.NewServer(NewHandler(log, Options{
		Health: Health{Postgres: closed.Ping, Redis: ok}, Pool: closed, APIKeys: []string{testAPIKey}, MaxAttempts: 6,
	}))
	t.Cleanup(broken.Close)

	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			scenario, ok := openAPIScenarios[name]
			if !ok {
				t.Fatalf("no scenario produces %s", name)
			}
			s := &openAPIServer{t: t, url: up.URL, downTo: down.URL, brokenTo: broken.URL, exec: func(sql string, args ...any) {
				t.Helper()
				if _, err := env.Pool.Exec(context.Background(), sql, args...); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}}
			want, _ := strconv.Atoi(name[strings.LastIndex(name, " ")+1:])
			if got := scenario(s); got != want {
				t.Errorf("got %d, want %d", got, want)
			}
		})
	}
}
