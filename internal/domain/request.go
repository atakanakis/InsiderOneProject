package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// Bounds of architecture §8 that belong to a request's content. The API's own bounds (body
// size, pages) are in package api.
const (
	MaxBatchItems          = 1000
	MaxVariables           = 50
	MaxVariableNameLen     = 64
	MaxTemplateNameLen     = 64
	MaxIdempotencyKeyLen   = 128
	MaxRenderedBytes       = 16 << 20 // the total of one request
	MaxEmailRecipientBytes = 254
	MinDeviceTokenLen      = 16
	MaxDeviceTokenLen      = 256
	MaxFieldErrors         = 100
)

// ContentLimit is the bound on a rendered content of the channel, in characters.
func ContentLimit(ch Channel) int {
	switch ch {
	case SMS:
		return 1600
	case Push:
		return 4096
	case Email:
		return 100_000
	}
	return 0
}

// FieldError names a field and what is wrong with it. The message never quotes a value of
// the request (architecture §8, "Errors").
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Errors collects field errors up to MaxFieldErrors and counts the rest.
type Errors struct {
	list  []FieldError
	total int
}

// Add records one field error.
func (e *Errors) Add(field, message string) {}

// Len is the number of errors added, kept or not.
func (e *Errors) Len() int { return e.total }

// List returns the kept errors.
func (e *Errors) List() []FieldError { return e.list }

// More is the number of errors that were counted and not kept.
func (e *Errors) More() int { return 0 }

// ErrTooManyVariables ends the decoding of an item's variables at the 51st.
var ErrTooManyVariables = errors.New("more than 50 variables")

// ErrBadVariables: variables is not an object of strings, or a name appears twice.
var ErrBadVariables = errors.New("variables must be an object of strings, each name once")

// Variable is one template variable.
type Variable struct {
	Name, Value string
}

// Variables are the variables of one item, in the order they were sent. They are counted
// while they are decoded, so an item with a million variables costs one more than the bound.
type Variables []Variable

// UnmarshalJSON decodes an object of strings and stops at the 51st entry.
func (v *Variables) UnmarshalJSON(b []byte) error {
	_ = json.Valid(b)
	return nil
}

// Input is one notification as the client sent it.
type Input struct {
	Recipient      string    `json:"recipient"`
	Channel        string    `json:"channel"`
	Content        *string   `json:"content"`
	Template       string    `json:"template"`
	Variables      Variables `json:"variables"`
	Priority       string    `json:"priority"`
	ScheduledAt    string    `json:"scheduledAt"`
	IdempotencyKey string    `json:"idempotencyKey"`
}

// Template is a stored template.
type Template struct {
	Name    string
	Channel Channel
	Body    string
}

// Compiled is a template body split into literal text and placeholders.
type Compiled struct{}

// CompileTemplate splits a body into its parts. It refuses a body with any "{{" that does not
// start a placeholder, a variable name outside its bound, or more than MaxVariables distinct
// variables.
func CompileTemplate(body string) (*Compiled, error) { return &Compiled{}, nil }

// Variables lists the template's distinct variables in the order of their first use.
func (c *Compiled) Variables() []string { return nil }

// Measure returns the length the template would render to with vars, in characters and in
// bytes, without rendering. A variable the template does not use and a placeholder without a
// variable are errors.
func (c *Compiled) Measure(vars Variables) (chars, bytes int, err error) { return 0, 0, nil }

// Render substitutes vars. Call it only after Measure accepted them.
func (c *Compiled) Render(vars Variables) string { return "" }

// CheckTemplate validates a template a client wants to create and returns it compiled.
func CheckTemplate(name, channel, body string, errs *Errors) (Template, *Compiled) {
	return Template{}, nil
}

// Lookup finds a compiled template of the request by name.
type Lookup func(name string) (*Compiled, Channel, bool)

// Prepared is a validated item, measured and not yet rendered.
type Prepared struct {
	Recipient   string
	Channel     Channel
	Priority    Priority
	ScheduledAt *time.Time
	Key         string
	Bytes       int // the rendered content's length in bytes

	content  string
	template *Compiled
	vars     Variables
}

// Prepare validates one item. field is the item's path in the request ("" for a single
// notification, "notifications[3]" in a batch). Errors are added to errs; the result is
// valid only when no error was added.
func Prepare(in Input, field string, lookup Lookup, errs *Errors) Prepared {
	return Prepared{}
}

// Content renders the item's content.
func (p Prepared) Content() string { return "" }

// Hash is the request hash of idempotency (architecture §8): SHA-256 of the canonical form of
// the validated items, without the idempotency key.
func Hash(items ...Input) []byte { return nil }

// ValidIdempotencyKey reports whether s is 1 to 128 printable ASCII characters.
func ValidIdempotencyKey(s string) bool { return false }

// ValidTemplateName reports whether s is 1 to 64 letters, digits, '_', '-' or '.'.
func ValidTemplateName(s string) bool { return false }
