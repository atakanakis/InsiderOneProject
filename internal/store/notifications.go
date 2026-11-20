package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
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
func BatchItemID(batch uuid.UUID, i int) uuid.UUID { return uuid.Nil }

// Create inserts a notification. With an idempotency key that exists, it returns the stored
// row and created false when the hashes match, and ErrIdempotencyConflict when they do not.
func (s *Notifications) Create(ctx context.Context, n NewNotification) (Notification, bool, error) {
	return Notification{}, false, errors.New("not implemented")
}

// CreateBatch inserts a batch and its items in one transaction. With an idempotency key that
// exists, it returns the stored batch's id and total and created false when the hashes match,
// and ErrIdempotencyConflict when they do not.
func (s *Notifications) CreateBatch(ctx context.Context, b NewBatch) (uuid.UUID, int, bool, error) {
	return uuid.Nil, 0, false, errors.New("not implemented")
}

// Get returns a notification with its attempt history, or ErrNotFound.
func (s *Notifications) Get(ctx context.Context, id uuid.UUID) (Notification, error) {
	return Notification{}, errors.New("not implemented")
}

// Batch returns a batch with its counts, or ErrBatchNotFound.
func (s *Notifications) Batch(ctx context.Context, id uuid.UUID) (Batch, error) {
	return Batch{}, errors.New("not implemented")
}

// List returns one page of summaries (Content and History are empty).
func (s *Notifications) List(ctx context.Context, f Filter) ([]Notification, error) {
	return nil, errors.New("not implemented")
}

// CreateTemplate stores a template. A template with the same name, channel and body returns
// created false; another channel or body is ErrTemplateExists.
func (s *Notifications) CreateTemplate(ctx context.Context, t domain.Template) (StoredTemplate, bool, error) {
	return StoredTemplate{}, false, errors.New("not implemented")
}

// Template returns one template, or ErrTemplateNotFound.
func (s *Notifications) Template(ctx context.Context, name string) (StoredTemplate, error) {
	return StoredTemplate{}, errors.New("not implemented")
}

// Templates returns up to limit templates after the name, in name order, without bodies.
func (s *Notifications) Templates(ctx context.Context, after string, limit int) ([]StoredTemplate, error) {
	return nil, errors.New("not implemented")
}

// TemplatesNamed returns the templates with the given names that exist.
func (s *Notifications) TemplatesNamed(ctx context.Context, names []string) ([]domain.Template, error) {
	return nil, errors.New("not implemented")
}
