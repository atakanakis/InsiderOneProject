package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
)

// The API's SQL (architecture §8): creating notifications and batches, reading them, and
// templates. The queue operations are in queue.go.

var (
	// ErrIdempotencyConflict: the key was used with another request.
	ErrIdempotencyConflict = errors.New("idempotency key was used with another request")
	// ErrTemplateExists: a template with the name exists with another channel or body.
	ErrTemplateExists = errors.New("template exists with another channel or body")
	// ErrBatchNotFound: no batch has the id.
	ErrBatchNotFound = errors.New("batch not found")
	// ErrTemplateNotFound: no template has the name.
	ErrTemplateNotFound = errors.New("template not found")
)

// MaxAttemptHistory bounds the attempts a status answer carries (architecture §8).
const MaxAttemptHistory = 100

// NewNotification is a validated notification to insert.
type NewNotification struct {
	ID             uuid.UUID
	Recipient      string
	Channel        domain.Channel
	Content        string
	Priority       domain.Priority
	ScheduledAt    *time.Time
	IdempotencyKey string // "" for none
	RequestHash    []byte
}

// Notification is a stored notification.
type Notification struct {
	ID                uuid.UUID
	BatchID           *uuid.UUID
	Recipient         string
	Channel           domain.Channel
	Content           string
	Priority          domain.Priority
	Status            domain.Status
	Attempts          int
	MaxAttempts       int
	ReadyAt           time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SentAt            *time.Time
	LastError         *string
	ProviderMessageID *string
	CorrelationID     string
	History           []Attempt // the last MaxAttemptHistory attempts, oldest first
	HistoryTotal      int
}

// Attempt is one finished delivery attempt.
type Attempt struct {
	Number     int
	StartedAt  time.Time
	DurationMS int
	StatusCode *int
	Outcome    string
	Error      *string
}

// NewBatch is a validated batch to insert.
type NewBatch struct {
	ID             uuid.UUID
	IdempotencyKey string
	RequestHash    []byte
	Items          []NewNotification // their ids are set by CreateBatch
}

// Batch is a stored batch with a count per status.
type Batch struct {
	ID        uuid.UUID
	Total     int
	CreatedAt time.Time
	Counts    map[domain.Status]int
}

// Filter selects a page of the list.
type Filter struct {
	Status      *domain.Status
	Channel     *domain.Channel
	BatchID     *uuid.UUID
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	After       *Cursor
	Limit       int
}

// Cursor is the position after the last item of a page: ORDER BY created_at DESC, id ASC.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// StoredTemplate is a stored template.
type StoredTemplate struct {
	domain.Template
	CreatedAt time.Time
}

// Notifications holds the API's SQL.
type Notifications struct {
	pool        *pgxpool.Pool
	maxAttempts int
}

// NewNotifications returns the API's store; new rows get maxAttempts.
func NewNotifications(pool *pgxpool.Pool, maxAttempts int) *Notifications {
	return &Notifications{pool: pool, maxAttempts: maxAttempts}
}

// BatchItemID is the id of item i of a batch: derived from the batch id, so a repeated batch
// answers the same ids in the same order (architecture §8).
func BatchItemID(batch uuid.UUID, i int) uuid.UUID {
	return uuid.NewSHA1(batch, []byte(strconv.Itoa(i)))
}

const notificationColumns = `id, batch_id, recipient, channel, content, priority, status, attempts,
	max_attempts, ready_at, created_at, updated_at, sent_at, last_error, provider_message_id,
	correlation_id, request_hash`

// The status and ready_at of a new row are decided by Postgres's clock (rule 7): a scheduledAt
// in the future waits for it, anything else is due now.
const createSQL = `INSERT INTO notifications (id, recipient, channel, content, priority, status,
	max_attempts, ready_at, idempotency_key, request_hash, correlation_id)
VALUES ($1, $2, $3, $4, $5,
	CASE WHEN $6::timestamptz > now() THEN 'scheduled' ELSE 'pending' END,
	$7, GREATEST(COALESCE($6::timestamptz, now()), now()), NULLIF($8, ''), $9, $10)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING ` + notificationColumns

func scanNotification(row pgx.Row) (Notification, []byte, error) {
	var n Notification
	var channel, status string
	var hash []byte
	err := row.Scan(&n.ID, &n.BatchID, &n.Recipient, &channel, &n.Content, &n.Priority, &status,
		&n.Attempts, &n.MaxAttempts, &n.ReadyAt, &n.CreatedAt, &n.UpdatedAt, &n.SentAt, &n.LastError,
		&n.ProviderMessageID, &n.CorrelationID, &hash)
	n.Channel, n.Status = domain.Channel(channel), domain.Status(status)
	return n, hash, err
}

// Create inserts a notification. With an idempotency key that exists, it returns the stored
// row and created false when the hashes match, and ErrIdempotencyConflict when they do not.
// Two identical requests at once: the second insert waits on the unique index until the
// first commits, then inserts nothing and reads the first row.
func (s *Notifications) Create(ctx context.Context, n NewNotification) (Notification, bool, error) {
	row := s.pool.QueryRow(ctx, createSQL, n.ID, n.Recipient, string(n.Channel), n.Content, n.Priority,
		n.ScheduledAt, s.maxAttempts, n.IdempotencyKey, n.RequestHash, observability.CorrelationID(ctx))
	created, _, err := scanNotification(row)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, fmt.Errorf("create notification: %w", err)
	}
	existing, hash, err := scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE idempotency_key = $1`, n.IdempotencyKey))
	if err != nil {
		return Notification{}, false, fmt.Errorf("read notification by key: %w", err)
	}
	if !bytes.Equal(hash, n.RequestHash) {
		return existing, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

const createBatchItemsSQL = `INSERT INTO notifications (id, batch_id, recipient, channel, content,
	priority, status, max_attempts, ready_at, correlation_id)
SELECT i.id, $1, i.recipient, i.channel, i.content, i.priority,
	CASE WHEN i.scheduled_at > now() THEN 'scheduled' ELSE 'pending' END,
	$2, GREATEST(COALESCE(i.scheduled_at, now()), now()), $3
FROM unnest($4::uuid[], $5::text[], $6::text[], $7::text[], $8::smallint[], $9::timestamptz[])
	AS i(id, recipient, channel, content, priority, scheduled_at)`

// CreateBatch inserts a batch and its items in one transaction. With an idempotency key that
// exists, it returns the stored batch's id and total and created false when the hashes match,
// and ErrIdempotencyConflict when they do not.
func (s *Notifications) CreateBatch(ctx context.Context, b NewBatch) (uuid.UUID, int, bool, error) {
	id, total, created := b.ID, len(b.Items), true
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var inserted uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO batches (id, idempotency_key, request_hash, total)
			VALUES ($1, NULLIF($2, ''), $3, $4) ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
			b.ID, b.IdempotencyKey, b.RequestHash, len(b.Items)).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			created = false
			var hash []byte
			if err := tx.QueryRow(ctx, `SELECT id, total, request_hash FROM batches WHERE idempotency_key = $1`,
				b.IdempotencyKey).Scan(&id, &total, &hash); err != nil {
				return fmt.Errorf("read batch by key: %w", err)
			}
			if !bytes.Equal(hash, b.RequestHash) {
				return ErrIdempotencyConflict
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("create batch: %w", err)
		}
		n := len(b.Items)
		ids, recipients, channels, contents := make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
		priorities, scheduled := make([]int16, n), make([]*time.Time, n)
		for i, it := range b.Items {
			ids[i], recipients[i], channels[i], contents[i] = BatchItemID(b.ID, i), it.Recipient, string(it.Channel), it.Content
			priorities[i], scheduled[i] = int16(it.Priority), it.ScheduledAt
		}
		if _, err := tx.Exec(ctx, createBatchItemsSQL, b.ID, s.maxAttempts, observability.CorrelationID(ctx),
			ids, recipients, channels, contents, priorities, scheduled); err != nil {
			return fmt.Errorf("create batch items: %w", err)
		}
		return nil
	})
	if err != nil {
		return id, total, false, err
	}
	return id, total, created, nil
}

// Get returns a notification with its attempt history, or ErrNotFound.
func (s *Notifications) Get(ctx context.Context, id uuid.UUID) (Notification, error) {
	n, _, err := scanNotification(s.pool.QueryRow(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, fmt.Errorf("get %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Notification{}, fmt.Errorf("get notification: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE notification_id = $1`, id).Scan(&n.HistoryTotal); err != nil {
		return Notification{}, fmt.Errorf("count attempts: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT attempt_number, started_at, duration_ms, status_code, outcome, error
		FROM delivery_attempts WHERE notification_id = $1 ORDER BY attempt_number DESC LIMIT $2`, id, MaxAttemptHistory)
	if err != nil {
		return Notification{}, fmt.Errorf("read attempts: %w", err)
	}
	n.History, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Attempt, error) {
		var a Attempt
		err := r.Scan(&a.Number, &a.StartedAt, &a.DurationMS, &a.StatusCode, &a.Outcome, &a.Error)
		return a, err
	})
	if err != nil {
		return Notification{}, fmt.Errorf("read attempts: %w", err)
	}
	slices.Reverse(n.History)
	return n, nil
}

// Batch returns a batch with its counts, or ErrBatchNotFound.
func (s *Notifications) Batch(ctx context.Context, id uuid.UUID) (Batch, error) {
	b := Batch{ID: id, Counts: map[domain.Status]int{}}
	err := s.pool.QueryRow(ctx, `SELECT total, created_at FROM batches WHERE id = $1`, id).Scan(&b.Total, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, fmt.Errorf("batch %s: %w", id, ErrBatchNotFound)
	}
	if err != nil {
		return Batch{}, fmt.Errorf("get batch: %w", err)
	}
	for _, st := range domain.Statuses {
		b.Counts[st] = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM notifications WHERE batch_id = $1 GROUP BY status`, id)
	if err != nil {
		return Batch{}, fmt.Errorf("count batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return Batch{}, fmt.Errorf("count batch: %w", err)
		}
		b.Counts[domain.Status(st)] = n
	}
	return b, rows.Err()
}

// List returns one page of summaries (Content and History are empty), ordered
// created_at DESC, id ASC: the order of the list index (architecture §4).
func (s *Notifications) List(ctx context.Context, f Filter) ([]Notification, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "$", "$"+strconv.Itoa(len(args))))
	}
	if f.Status != nil {
		add("status = $", string(*f.Status))
	}
	if f.Channel != nil {
		add("channel = $", string(*f.Channel))
	}
	if f.BatchID != nil {
		add("batch_id = $", *f.BatchID)
	}
	if f.CreatedFrom != nil {
		add("created_at >= $", *f.CreatedFrom)
	}
	if f.CreatedTo != nil {
		add("created_at < $", *f.CreatedTo)
	}
	if f.After != nil {
		args = append(args, f.After.CreatedAt, f.After.ID)
		c, i := len(args)-1, len(args)
		where = append(where, fmt.Sprintf("(created_at < $%d OR (created_at = $%d AND id > $%d))", c, c, i))
	}
	sql := `SELECT id, batch_id, recipient, channel, priority, status, attempts, created_at, updated_at FROM notifications`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, f.Limit)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Notification, error) {
		var n Notification
		var channel, status string
		err := r.Scan(&n.ID, &n.BatchID, &n.Recipient, &channel, &n.Priority, &status, &n.Attempts, &n.CreatedAt, &n.UpdatedAt)
		n.Channel, n.Status = domain.Channel(channel), domain.Status(status)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return list, nil
}

// CreateTemplate stores a template. A template with the same name, channel and body returns
// created false; another channel or body is ErrTemplateExists.
func (s *Notifications) CreateTemplate(ctx context.Context, t domain.Template) (StoredTemplate, bool, error) {
	st := StoredTemplate{Template: t}
	err := s.pool.QueryRow(ctx, `INSERT INTO templates (name, channel, body) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO NOTHING RETURNING created_at`, t.Name, string(t.Channel), t.Body).Scan(&st.CreatedAt)
	if err == nil {
		return st, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return StoredTemplate{}, false, fmt.Errorf("create template: %w", err)
	}
	existing, err := s.Template(ctx, t.Name)
	if err != nil {
		return StoredTemplate{}, false, err
	}
	if existing.Channel != t.Channel || existing.Body != t.Body {
		return existing, false, ErrTemplateExists
	}
	return existing, false, nil
}

// Template returns one template, or ErrTemplateNotFound.
func (s *Notifications) Template(ctx context.Context, name string) (StoredTemplate, error) {
	var st StoredTemplate
	var channel string
	err := s.pool.QueryRow(ctx, `SELECT name, channel, body, created_at FROM templates WHERE name = $1`, name).
		Scan(&st.Name, &channel, &st.Body, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredTemplate{}, ErrTemplateNotFound
	}
	if err != nil {
		return StoredTemplate{}, fmt.Errorf("get template: %w", err)
	}
	st.Channel = domain.Channel(channel)
	return st, nil
}

// Templates returns up to limit templates after the name, in name order, without bodies.
func (s *Notifications) Templates(ctx context.Context, after string, limit int) ([]StoredTemplate, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, channel, created_at FROM templates WHERE name > $1 ORDER BY name LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StoredTemplate, error) {
		var st StoredTemplate
		var channel string
		err := r.Scan(&st.Name, &channel, &st.CreatedAt)
		st.Channel = domain.Channel(channel)
		return st, err
	})
}

// TemplatesNamed returns the templates with the given names that exist.
func (s *Notifications) TemplatesNamed(ctx context.Context, names []string) ([]domain.Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, channel, body FROM templates WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("read templates: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Template, error) {
		var t domain.Template
		var channel string
		err := r.Scan(&t.Name, &channel, &t.Body)
		t.Channel = domain.Channel(channel)
		return t, err
	})
}
