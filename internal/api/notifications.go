package api

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// Bounds of architecture §8 that the API enforces itself.
const (
	MaxBodyBytes      = 1 << 20
	MaxBatchBodyBytes = 8 << 20
	MaxPage           = 100
	DefaultPage       = 50
	MaxErrorBodyBytes = 64 << 10
	maxEchoedName     = domain.MaxEchoedName
)

// Options configure the API's handler.
type Options struct {
	Health      Health
	Pool        *pgxpool.Pool    // nil: only the health endpoints and the specification
	APIKeys     []string         // empty: no authentication
	MaxAttempts int              // a new row's max_attempts and the budget of /retry; 0 means 6
	Events      events.Publisher // nil: events.Discard
	// DatabaseDeadline bounds the database work of a request; 0 means DatabaseDeadline.
	DatabaseDeadline time.Duration
}

//go:embed openapi.yaml
var openapi []byte

const docsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Notification API</title></head>
<body><redoc spec-url="/openapi.yaml"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.5.0/bundles/redoc.standalone.js"></script>
</body></html>
`

type server struct {
	log         *slog.Logger
	store       *store.Notifications
	queue       *store.Queue
	keys        [][32]byte
	maxAttempts int
	events      events.Publisher
}

type route struct {
	pattern string
	handle  http.HandlerFunc
}

func (s *server) routes() []route {
	return []route{
		{"POST /v1/notifications", s.create},
		{"POST /v1/notifications/batch", s.createBatch},
		{"GET /v1/notifications/{id}", s.get},
		{"GET /v1/notifications", s.list},
		{"POST /v1/notifications/{id}/cancel", s.cancel},
		{"POST /v1/notifications/{id}/retry", s.retry},
		{"GET /v1/batches/{id}", s.batch},
		{"POST /v1/templates", s.createTemplate},
		{"GET /v1/templates", s.templates},
		{"GET /v1/templates/{name}", s.template},
	}
}

// routePatterns lists every route of the API, as registered on the mux.
func routePatterns() []string {
	out := []string{"GET /healthz", "GET /readyz", "GET /openapi.yaml", "GET /docs"}
	for _, r := range (&server{}).routes() {
		out = append(out, r.pattern)
	}
	return out
}

// register adds the specification and, with a database, the /v1 routes.
func register(mux *http.ServeMux, log *slog.Logger, o Options) {
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapi)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, docsPage)
	})
	if o.Pool == nil {
		return
	}
	s := &server{
		log: log, store: store.NewNotifications(o.Pool, maxAttempts(o)), queue: store.NewQueue(o.Pool),
		maxAttempts: maxAttempts(o), events: o.Events,
	}
	if s.events == nil {
		s.events = events.Discard{}
	}
	for _, k := range o.APIKeys {
		s.keys = append(s.keys, sha256.Sum256([]byte(k)))
	}
	for _, r := range s.routes() {
		mux.Handle(r.pattern, s.authenticate(r.handle))
	}
}

func maxAttempts(o Options) int {
	if o.MaxAttempts <= 0 {
		return 6
	}
	return o.MaxAttempts
}

// authenticate lets a request through when no keys are configured, or when it carries one
// of them. Every configured key is compared, in constant time.
func (s *server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.keys) == 0 {
			next(w, r)
			return
		}
		given := r.Header.Get("X-API-Key")
		if auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			given = auth
		}
		sum := sha256.Sum256([]byte(given))
		match := 0
		for _, k := range s.keys {
			match |= subtle.ConstantTimeCompare(sum[:], k[:])
		}
		if given == "" || match != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="notifications"`)
			writeError(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "an API key is required"})
			return
		}
		next(w, r)
	}
}

// apiError is the one form of every error answer (architecture §8, "Errors").
type apiError struct {
	Code       string              `json:"code"`
	Message    string              `json:"message"`
	Fields     []domain.FieldError `json:"fields,omitempty"`
	MoreFields int                 `json:"moreFields,omitempty"`
	Status     string              `json:"status,omitempty"`
}

// writeError writes the one error form. Its size does not depend on the request: a message is
// a fixed text, and a name from the request has passed domain.Echo (64 bytes). At most 100
// fields are kept, so the body stays under MaxErrorBodyBytes.
func writeError(w http.ResponseWriter, code int, e apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]apiError{"error": e})
}

func fieldError(field, message string) apiError {
	return apiError{Code: "validation_failed", Message: "the request is not valid",
		Fields: []domain.FieldError{{Field: field, Message: message}}}
}

func validationError(errs *domain.Errors) apiError {
	return apiError{Code: "validation_failed", Message: "the request is not valid",
		Fields: errs.List(), MoreFields: errs.More()}
}

// failure is an error answer, or nil.
type failure struct {
	code int
	err  apiError
}

// internal logs err and answers 500 without showing it.
func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "the request could not be completed"})
}

// decodeFailure maps a decoding error to an answer that never quotes the request. prefix is
// the item's path in a batch.
func decodeFailure(err error, prefix string, limit int64) *failure {
	var tooLarge *http.MaxBytesError
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return &failure{http.StatusRequestEntityTooLarge, apiError{Code: "body_too_large",
			Message: fmt.Sprintf("the body is larger than %d bytes", limit)}}
	case errors.Is(err, domain.ErrTooManyVariables):
		return &failure{http.StatusBadRequest, fieldError(prefix+"variables", fmt.Sprintf("at most %d variables", domain.MaxVariables))}
	case errors.Is(err, domain.ErrBadVariables):
		return &failure{http.StatusBadRequest, fieldError(prefix+"variables", "must be an object of strings, each name once")}
	case errors.As(err, &syntax):
		return &failure{http.StatusBadRequest, apiError{Code: "invalid_json",
			Message: fmt.Sprintf("the body is not valid JSON (at byte %d)", syntax.Offset)}}
	case errors.As(err, &typ):
		return &failure{http.StatusBadRequest, fieldError(prefix+domain.Echo(typ.Field), "has the wrong type")}
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return &failure{http.StatusBadRequest, fieldError(prefix+domain.Echo(name), "is not a known field")}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &failure{http.StatusBadRequest, apiError{Code: "invalid_json", Message: "the body ends before its JSON value does"}}
	}
	return &failure{http.StatusBadRequest, apiError{Code: "invalid_json", Message: "the body could not be decoded"}}
}

// body wraps the request body in its bound. When the handler stops reading early, the rest
// (at most the bound) is read and dropped, so the client gets the answer.
func body(w http.ResponseWriter, r *http.Request, limit int64) (*json.Decoder, func()) {
	rd := http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(rd)
	dec.DisallowUnknownFields()
	return dec, func() { _, _ = io.Copy(io.Discard, rd) }
}

// end checks that nothing but whitespace follows the value, within the bound.
func end(dec *json.Decoder, limit int64) *failure {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return &failure{http.StatusBadRequest, apiError{Code: "invalid_json", Message: "the body must hold one JSON value"}}
		}
		return decodeFailure(err, "", limit)
	}
	return nil
}

// readOne decodes a body that holds one object.
func readOne(w http.ResponseWriter, r *http.Request, limit int64, v any) *failure {
	dec, drain := body(w, r, limit)
	if err := dec.Decode(v); err != nil {
		drain()
		return decodeFailure(err, "", limit)
	}
	if f := end(dec, limit); f != nil {
		drain()
		return f
	}
	return nil
}

// readBatch decodes {"notifications": [...]} one item at a time, and stops at the item after
// the bound (architecture §8, "Decoding is bounded too").
func readBatch(w http.ResponseWriter, r *http.Request) ([]domain.Input, *failure) {
	dec, drain := body(w, r, MaxBatchBodyBytes)
	items, f := decodeBatch(dec)
	if f == nil {
		f = end(dec, MaxBatchBodyBytes)
	}
	if f != nil {
		drain()
	}
	return items, f
}

func decodeBatch(dec *json.Decoder) ([]domain.Input, *failure) {
	delim := func(want json.Delim) *failure {
		tok, err := dec.Token()
		if err != nil {
			return decodeFailure(err, "", MaxBatchBodyBytes)
		}
		if tok != want {
			return &failure{http.StatusBadRequest, fieldError("notifications", "the body must be {\"notifications\": [...]}")}
		}
		return nil
	}
	if f := delim('{'); f != nil {
		return nil, f
	}
	var items []domain.Input
	seen := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, decodeFailure(err, "", MaxBatchBodyBytes)
		}
		if key, _ := tok.(string); key != "notifications" || seen {
			return nil, &failure{http.StatusBadRequest, fieldError(domain.Echo(key), "is not a known field, or appears twice")}
		}
		seen = true
		if f := delim('['); f != nil {
			return nil, f
		}
		for dec.More() {
			if len(items) == domain.MaxBatchItems {
				return nil, &failure{http.StatusBadRequest, fieldError("notifications",
					fmt.Sprintf("a batch has at most %d notifications", domain.MaxBatchItems))}
			}
			var in domain.Input
			if err := dec.Decode(&in); err != nil {
				return nil, decodeFailure(err, fmt.Sprintf("notifications[%d].", len(items)), MaxBatchBodyBytes)
			}
			items = append(items, in)
		}
		if f := delim(']'); f != nil {
			return nil, f
		}
	}
	if f := delim('}'); f != nil {
		return nil, f
	}
	return items, nil
}

// lookup reads the templates the items name in one query, and compiles each on its first
// use, once per request (architecture §8).
func (s *server) lookup(r *http.Request, items []domain.Input) (domain.Lookup, error) {
	var names []string
	seen := map[string]bool{}
	for _, in := range items {
		if in.Template != "" && domain.ValidTemplateName(in.Template) && !seen[in.Template] {
			seen[in.Template] = true
			names = append(names, in.Template)
		}
	}
	type template struct {
		domain.Template
		compiled *domain.Compiled
	}
	byName := map[string]*template{}
	if len(names) > 0 {
		list, err := s.store.TemplatesNamed(r.Context(), names)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			byName[t.Name] = &template{Template: t}
		}
	}
	return func(name string) (*domain.Compiled, domain.Channel, bool) {
		t, ok := byName[name]
		if !ok {
			return nil, "", false
		}
		if t.compiled == nil {
			c, err := domain.CompileTemplate(t.Body)
			if err != nil { // a stored body always compiles: it was checked when created
				s.log.ErrorContext(r.Context(), "stored template does not compile", "template", t.Name, "error", err)
				return nil, "", false
			}
			t.compiled = c
		}
		return t.compiled, t.Channel, true
	}, nil
}

func newRow(p domain.Prepared, hash []byte) store.NewNotification {
	return store.NewNotification{
		ID: uuid.New(), Recipient: p.Recipient, Channel: p.Channel, Content: p.Content(),
		Priority: p.Priority, ScheduledAt: p.ScheduledAt, IdempotencyKey: p.Key, RequestHash: hash,
	}
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in domain.Input
	if f := readOne(w, r, MaxBodyBytes, &in); f != nil {
		writeError(w, f.code, f.err)
		return
	}
	lookup, err := s.lookup(r, []domain.Input{in})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var errs domain.Errors
	p := domain.Prepare(in, "", lookup, &errs)
	if errs.Len() > 0 {
		writeError(w, http.StatusBadRequest, validationError(&errs))
		return
	}
	n, created, err := s.store.Create(r.Context(), newRow(p, domain.Hash(in)))
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, apiError{Code: "idempotency_conflict",
			Message: "the idempotency key was used with another request"})
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
		s.events.Publish(r.Context(), n.ID, n.Status)
	}
	writeJSON(w, code, notificationJSON(n, true))
}

func (s *server) createBatch(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !domain.ValidIdempotencyKey(key) {
		_, drain := body(w, r, MaxBatchBodyBytes)
		drain()
		writeError(w, http.StatusBadRequest, fieldError("Idempotency-Key",
			fmt.Sprintf("must be 1 to %d printable ASCII characters", domain.MaxIdempotencyKeyLen)))
		return
	}
	items, f := readBatch(w, r)
	if f != nil {
		writeError(w, f.code, f.err)
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, fieldError("notifications", "a batch has at least one notification"))
		return
	}
	lookup, err := s.lookup(r, items)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var errs domain.Errors
	prepared := make([]domain.Prepared, len(items))
	total := 0
	for i, in := range items {
		field := fmt.Sprintf("notifications[%d]", i)
		if in.IdempotencyKey != "" {
			errs.Add(field+".idempotencyKey", "an item has no key of its own; the batch has the Idempotency-Key header")
		}
		prepared[i] = domain.Prepare(in, field, lookup, &errs)
		total += prepared[i].Bytes
	}
	if errs.Len() > 0 {
		writeError(w, http.StatusBadRequest, validationError(&errs))
		return
	}
	// The total is known before anything is rendered (architecture §8).
	if total > domain.MaxRenderedBytes {
		writeError(w, http.StatusBadRequest, fieldError("notifications",
			fmt.Sprintf("the rendered content of the request is over %d bytes", domain.MaxRenderedBytes)))
		return
	}
	b := store.NewBatch{ID: uuid.New(), IdempotencyKey: key, RequestHash: domain.Hash(items...)}
	b.Items = make([]store.NewNotification, len(prepared))
	for i, p := range prepared {
		b.Items[i] = newRow(p, nil)
	}
	id, n, created, err := s.store.CreateBatch(r.Context(), b)
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, apiError{Code: "idempotency_conflict",
			Message: "the idempotency key was used with another batch"})
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = store.BatchItemID(id, i)
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
		for i, id := range ids {
			s.events.Publish(r.Context(), id, initialStatus(prepared[i]))
		}
	}
	writeJSON(w, code, map[string]any{"batchId": id, "total": n, "ids": ids})
}

// initialStatus is the status the insert gave a batch item. The insert decided it on the
// database clock; the event is best-effort, and a scheduledAt within the clocks' drift of
// now may be reported as scheduled for a row stored as pending.
func initialStatus(p domain.Prepared) domain.Status {
	if p.ScheduledAt != nil && p.ScheduledAt.After(time.Now()) {
		return domain.Scheduled
	}
	return domain.Pending
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fieldError("id", "is not a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func notFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no " + what + " has this id"})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "notification")
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusOK, notificationJSON(n, true))
	}
}

func (s *server) batch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	b, err := s.store.Batch(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrBatchNotFound):
		notFound(w, "batch")
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"id": b.ID, "total": b.Total, "createdAt": b.CreatedAt, "counts": b.Counts})
	}
}

// The list cursor is the (created_at, id) of the last item: 8 bytes of microseconds and the
// 16 bytes of the id, base64url without padding.
func encodeCursor(c store.Cursor) string {
	b := binary.BigEndian.AppendUint64(nil, uint64(c.CreatedAt.UnixMicro()))
	return base64.RawURLEncoding.EncodeToString(append(b, c.ID[:]...))
}

// lastCursorMicros is the end of the year 9999: a cursor's time lies between 1970 and then.
var lastCursorMicros = uint64(time.Date(9999, 12, 31, 23, 59, 59, 999_999_000, time.UTC).UnixMicro())

func decodeCursor(s string) (store.Cursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if len(s) != 32 || err != nil || len(b) != 24 {
		return store.Cursor{}, false
	}
	micros := binary.BigEndian.Uint64(b[:8])
	if micros > lastCursorMicros {
		return store.Cursor{}, false
	}
	id, _ := uuid.FromBytes(b[8:])
	return store.Cursor{CreatedAt: time.UnixMicro(int64(micros)).UTC(), ID: id}, true //nolint:gosec // bounded above
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var errs domain.Errors
	f := store.Filter{Limit: DefaultPage}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPage {
			errs.Add("limit", fmt.Sprintf("must be 1 to %d", MaxPage))
		}
		f.Limit = n
	}
	if v := q.Get("status"); v != "" {
		st, err := domain.ParseStatus(v)
		if err != nil {
			errs.Add("status", "is not a status")
		}
		f.Status = &st
	}
	if v := q.Get("channel"); v != "" {
		ch, err := domain.ParseChannel(v)
		if err != nil {
			errs.Add("channel", "must be sms, email or push")
		}
		f.Channel = &ch
	}
	if v := q.Get("batchId"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			errs.Add("batchId", "is not a UUID")
		}
		f.BatchID = &id
	}
	for name, dst := range map[string]**time.Time{"createdFrom": &f.CreatedFrom, "createdTo": &f.CreatedTo} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				errs.Add(name, "must be an RFC 3339 time")
			}
			*dst = &t
		}
	}
	if v := q.Get("cursor"); v != "" {
		c, ok := decodeCursor(v)
		if !ok {
			errs.Add("cursor", "is not a cursor this API gave")
		}
		f.After = &c
	}
	if errs.Len() > 0 {
		writeError(w, http.StatusBadRequest, validationError(&errs))
		return
	}
	list, err := s.store.List(r.Context(), f)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := map[string]any{}
	items := make([]notificationOut, len(list))
	for i, n := range list {
		items[i] = notificationJSON(n, false)
	}
	out["items"] = items
	if len(list) == f.Limit {
		last := list[len(list)-1]
		out["nextCursor"] = encodeCursor(store.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, out)
}

// change answers a cancel or a retry.
func (s *server) change(w http.ResponseWriter, r *http.Request, op func(uuid.UUID) (domain.Status, error)) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := op(id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "notification")
	case errors.Is(err, store.ErrWrongStatus):
		writeError(w, http.StatusConflict, apiError{Code: "wrong_status",
			Message: "the notification is not in a status that allows this", Status: string(st)})
	case err != nil:
		s.internal(w, r, err)
	default:
		s.events.Publish(r.Context(), id, st)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": st})
	}
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	s.change(w, r, func(id uuid.UUID) (domain.Status, error) { return s.queue.Cancel(r.Context(), id) })
}

// retry requeues a failed row with MAX_ATTEMPTS more attempts (architecture §5).
func (s *server) retry(w http.ResponseWriter, r *http.Request) {
	s.change(w, r, func(id uuid.UUID) (domain.Status, error) { return s.queue.Requeue(r.Context(), id, s.maxAttempts) })
}

func templateJSON(t store.StoredTemplate, withBody bool) map[string]any {
	out := map[string]any{"name": t.Name, "channel": t.Channel, "createdAt": t.CreatedAt}
	if withBody {
		out["body"] = t.Body
		vars := []string{}
		if c, err := domain.CompileTemplate(t.Body); err == nil {
			vars = c.Variables()
		}
		out["variables"] = vars
	}
	return out
}

func (s *server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Channel string `json:"channel"`
		Body    string `json:"body"`
	}
	if f := readOne(w, r, MaxBodyBytes, &in); f != nil {
		writeError(w, f.code, f.err)
		return
	}
	var errs domain.Errors
	t, _ := domain.CheckTemplate(in.Name, in.Channel, in.Body, &errs)
	if errs.Len() > 0 {
		writeError(w, http.StatusBadRequest, validationError(&errs))
		return
	}
	st, created, err := s.store.CreateTemplate(r.Context(), t)
	switch {
	case errors.Is(err, store.ErrTemplateExists):
		writeError(w, http.StatusConflict, apiError{Code: "template_exists",
			Message: "a template with this name exists with another channel or body"})
	case err != nil:
		s.internal(w, r, err)
	case created:
		writeJSON(w, http.StatusCreated, templateJSON(st, true))
	default:
		writeJSON(w, http.StatusOK, templateJSON(st, true))
	}
}

func (s *server) templates(w http.ResponseWriter, r *http.Request) {
	after := ""
	if v := r.URL.Query().Get("cursor"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || !domain.ValidTemplateName(string(b)) {
			writeError(w, http.StatusBadRequest, fieldError("cursor", "is not a cursor this API gave"))
			return
		}
		after = string(b)
	}
	list, err := s.store.Templates(r.Context(), after, MaxPage)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	items := make([]map[string]any, len(list))
	for i, t := range list {
		items[i] = templateJSON(t, false)
	}
	out := map[string]any{"items": items}
	if len(list) == MaxPage {
		out["nextCursor"] = base64.RawURLEncoding.EncodeToString([]byte(list[len(list)-1].Name))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) template(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !domain.ValidTemplateName(name) {
		writeError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no template has this name"})
		return
	}
	t, err := s.store.Template(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrTemplateNotFound):
		writeError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no template has this name"})
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusOK, templateJSON(t, true))
	}
}

type attemptOut struct {
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"startedAt"`
	DurationMS int       `json:"durationMs"`
	StatusCode *int      `json:"statusCode,omitempty"`
	Outcome    string    `json:"outcome"`
	Error      *string   `json:"error,omitempty"`
}

type notificationOut struct {
	ID                uuid.UUID      `json:"id"`
	BatchID           *uuid.UUID     `json:"batchId,omitempty"`
	Recipient         string         `json:"recipient"`
	Channel           domain.Channel `json:"channel"`
	Content           *string        `json:"content,omitempty"`
	Priority          string         `json:"priority"`
	Status            domain.Status  `json:"status"`
	Attempts          int            `json:"attempts"`
	MaxAttempts       int            `json:"maxAttempts,omitempty"`
	ReadyAt           *time.Time     `json:"readyAt,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	SentAt            *time.Time     `json:"sentAt,omitempty"`
	LastError         *string        `json:"lastError,omitempty"`
	ProviderMessageID *string        `json:"providerMessageId,omitempty"`
	CorrelationID     *string        `json:"correlationId,omitempty"`
	History           *[]attemptOut  `json:"attemptHistory,omitempty"`
	HistoryTotal      *int           `json:"attemptHistoryTotal,omitempty"`
}

// notificationJSON is the full answer, or the list's summary without content.
func notificationJSON(n store.Notification, full bool) notificationOut {
	out := notificationOut{
		ID: n.ID, BatchID: n.BatchID, Recipient: n.Recipient, Channel: n.Channel, Priority: n.Priority.String(),
		Status: n.Status, Attempts: n.Attempts, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
	if !full {
		return out
	}
	out.Content, out.MaxAttempts, out.ReadyAt = &n.Content, n.MaxAttempts, &n.ReadyAt
	out.SentAt, out.LastError, out.ProviderMessageID, out.CorrelationID = n.SentAt, n.LastError, n.ProviderMessageID, &n.CorrelationID
	history := make([]attemptOut, len(n.History))
	for i, a := range n.History {
		history[i] = attemptOut(a)
	}
	out.History = &history
	out.HistoryTotal = &n.HistoryTotal
	return out
}
