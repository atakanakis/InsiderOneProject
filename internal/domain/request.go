package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
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
func (e *Errors) Add(field, message string) {
	e.total++
	if len(e.list) < MaxFieldErrors {
		e.list = append(e.list, FieldError{Field: field, Message: message})
	}
}

// Len is the number of errors added, kept or not.
func (e *Errors) Len() int { return e.total }

// List returns the kept errors.
func (e *Errors) List() []FieldError { return e.list }

// More is the number of errors that were counted and not kept.
func (e *Errors) More() int { return e.total - len(e.list) }

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
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ErrBadVariables
	}
	out := Variables{}
	seen := map[string]bool{}
	for dec.More() {
		if len(out) == MaxVariables {
			return ErrTooManyVariables
		}
		name, err := dec.Token()
		if err != nil {
			return ErrBadVariables
		}
		value, err := dec.Token()
		n, ok1 := name.(string)
		val, ok2 := value.(string)
		if err != nil || !ok1 || !ok2 || seen[n] {
			return ErrBadVariables
		}
		seen[n] = true
		out = append(out, Variable{Name: n, Value: val})
	}
	*v = out
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

// part is literal text, or a placeholder when name is set.
type part struct {
	text, name string
}

// Compiled is a template body split into literal text and placeholders.
type Compiled struct {
	parts        []part
	vars         []string       // distinct, in the order of their first use
	uses         map[string]int // how often each variable is used
	literalChars int
	literalBytes int
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CompileTemplate splits a body into its parts. It refuses a body with any "{{" that does not
// start a placeholder, a variable name outside its bound, or more than MaxVariables distinct
// variables.
func CompileTemplate(body string) (*Compiled, error) {
	c := &Compiled{uses: map[string]int{}}
	rest := body
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			c.literal(rest)
			break
		}
		c.literal(rest[:i])
		rest = rest[i:]
		end := strings.Index(rest, "}}")
		if end < 0 || !strings.HasPrefix(rest, "{{.") {
			return nil, errors.New("every {{ must start a placeholder {{.name}}")
		}
		name := rest[3:end]
		if !variableName.MatchString(name) || len(name) > MaxVariableNameLen {
			return nil, fmt.Errorf("a placeholder name must be a letter or _ followed by letters, digits or _, up to %d characters", MaxVariableNameLen)
		}
		if c.uses[name] == 0 {
			if len(c.vars) == MaxVariables {
				return nil, fmt.Errorf("a template has at most %d variables", MaxVariables)
			}
			c.vars = append(c.vars, name)
		}
		c.uses[name]++
		c.parts = append(c.parts, part{name: name})
		rest = rest[end+2:]
	}
	return c, nil
}

func (c *Compiled) literal(s string) {
	if s == "" {
		return
	}
	c.parts = append(c.parts, part{text: s})
	c.literalChars += utf8.RuneCountInString(s)
	c.literalBytes += len(s)
}

// Variables lists the template's distinct variables in the order of their first use.
func (c *Compiled) Variables() []string { return slices.Clone(c.vars) }

// Measure returns the length the template would render to with vars, in characters and in
// bytes, without rendering. A variable the template does not use and a placeholder without a
// variable are errors.
func (c *Compiled) Measure(vars Variables) (chars, size int, err error) {
	chars, size = c.literalChars, c.literalBytes
	given := make(map[string]bool, len(vars))
	for _, v := range vars {
		uses := c.uses[v.Name]
		if uses == 0 {
			return 0, 0, &VariableError{Name: v.Name, Message: "the template does not use this variable"}
		}
		given[v.Name] = true
		chars += uses * utf8.RuneCountInString(v.Value)
		size += uses * len(v.Value)
	}
	for _, name := range c.vars {
		if !given[name] {
			return 0, 0, &VariableError{Name: name, Message: "the template needs this variable"}
		}
	}
	return chars, size, nil
}

// VariableError names a variable that is missing or not used.
type VariableError struct {
	Name, Message string
}

func (e *VariableError) Error() string { return e.Message }

// Render substitutes vars. Call it only after Measure accepted them.
func (c *Compiled) Render(vars Variables) string {
	values := make(map[string]string, len(vars))
	size := c.literalBytes
	for _, v := range vars {
		values[v.Name] = v.Value
		size += c.uses[v.Name] * len(v.Value)
	}
	var b strings.Builder
	b.Grow(size)
	for _, p := range c.parts {
		if p.name == "" {
			b.WriteString(p.text)
		} else {
			b.WriteString(values[p.name])
		}
	}
	return b.String()
}

// CheckTemplate validates a template a client wants to create and returns it compiled.
func CheckTemplate(name, channel, body string, errs *Errors) (Template, *Compiled) {
	before := errs.Len()
	if !ValidTemplateName(name) {
		errs.Add("name", fmt.Sprintf("must be 1 to %d letters, digits, _, - or .", MaxTemplateNameLen))
	}
	ch, err := ParseChannel(channel)
	if err != nil {
		errs.Add("channel", "must be sms, email or push")
	}
	var c *Compiled
	switch {
	case body == "":
		errs.Add("body", "is required")
	case strings.ContainsRune(body, 0):
		errs.Add("body", "must not contain NUL")
	case ch != "" && utf8.RuneCountInString(body) > ContentLimit(ch):
		errs.Add("body", fmt.Sprintf("is longer than %d characters, the bound of its channel", ContentLimit(ch)))
	default:
		if c, err = CompileTemplate(body); err != nil {
			errs.Add("body", err.Error())
		}
	}
	if errs.Len() > before {
		return Template{}, nil
	}
	return Template{Name: name, Channel: ch, Body: body}, c
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

var (
	e164        = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	deviceToken = regexp.MustCompile(`^[A-Za-z0-9_:.-]+$`)
)

// Prepare validates one item. field is the item's path in the request ("" for a single
// notification, "notifications[3]" in a batch). Errors are added to errs; the result is
// valid only when no error was added.
func Prepare(in Input, field string, lookup Lookup, errs *Errors) Prepared {
	at := func(name string) string {
		if field == "" {
			return name
		}
		return field + "." + name
	}
	var p Prepared
	var err error
	if p.Channel, err = ParseChannel(in.Channel); err != nil {
		errs.Add(at("channel"), "must be sms, email or push")
	} else if msg := checkRecipient(p.Channel, in.Recipient); msg != "" {
		errs.Add(at("recipient"), msg)
	}
	p.Recipient = in.Recipient
	p.Priority = Normal
	if in.Priority != "" {
		if p.Priority, err = ParsePriority(in.Priority); err != nil {
			errs.Add(at("priority"), "must be high, normal or low")
		}
	}
	if in.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339Nano, in.ScheduledAt)
		if err != nil {
			errs.Add(at("scheduledAt"), "must be an RFC 3339 time")
		} else {
			t = t.UTC()
			p.ScheduledAt = &t
		}
	}
	if in.IdempotencyKey != "" && !ValidIdempotencyKey(in.IdempotencyKey) {
		errs.Add(at("idempotencyKey"), fmt.Sprintf("must be 1 to %d printable ASCII characters", MaxIdempotencyKeyLen))
	}
	p.Key = in.IdempotencyKey

	chars := 0
	switch {
	case in.Content != nil && (in.Template != "" || in.Variables != nil):
		errs.Add(at("template"), "give either content or a template with variables, not both")
		return p
	case in.Content != nil:
		p.content = *in.Content
		chars, p.Bytes = utf8.RuneCountInString(p.content), len(p.content)
		if strings.ContainsRune(p.content, 0) {
			errs.Add(at("content"), "must not contain NUL")
		}
	case in.Template != "":
		if !ValidTemplateName(in.Template) {
			errs.Add(at("template"), "is not a template name")
			return p
		}
		c, ch, ok := lookup(in.Template)
		if !ok {
			errs.Add(at("template"), "no template has this name")
			return p
		}
		if p.Channel != "" && ch != p.Channel {
			errs.Add(at("template"), "the template is for another channel")
			return p
		}
		if chars, p.Bytes, err = c.Measure(in.Variables); err != nil {
			var ve *VariableError
			if errors.As(err, &ve) {
				errs.Add(at("variables."+Echo(ve.Name)), ve.Message)
			}
			return p
		}
		for _, v := range in.Variables {
			if strings.ContainsRune(v.Value, 0) {
				errs.Add(at("variables."+Echo(v.Name)), "must not contain NUL")
			}
		}
		p.template, p.vars = c, in.Variables
	default:
		errs.Add(at("content"), "content or a template is required")
		return p
	}
	if chars == 0 {
		errs.Add(at("content"), "must not be empty")
	} else if p.Channel != "" && chars > ContentLimit(p.Channel) {
		errs.Add(at("content"), fmt.Sprintf("is longer than %d characters, the bound of the channel", ContentLimit(p.Channel)))
	}
	return p
}

func checkRecipient(ch Channel, to string) string {
	switch ch {
	case SMS:
		if !e164.MatchString(to) {
			return "must be an E.164 phone number such as +905551234567"
		}
	case Email:
		a, err := mail.ParseAddress(to)
		if len(to) > MaxEmailRecipientBytes || err != nil || a.Address != to || a.Name != "" {
			return fmt.Sprintf("must be a bare email address of at most %d bytes", MaxEmailRecipientBytes)
		}
	case Push:
		if len(to) < MinDeviceTokenLen || len(to) > MaxDeviceTokenLen || !deviceToken.MatchString(to) {
			return fmt.Sprintf("must be a device token of %d to %d letters, digits, _, -, : or .", MinDeviceTokenLen, MaxDeviceTokenLen)
		}
	}
	return ""
}

// Content renders the item's content.
func (p Prepared) Content() string {
	if p.template != nil {
		return p.template.Render(p.vars)
	}
	return p.content
}

// Hash is the request hash of idempotency (architecture §8): SHA-256 of the canonical form of
// the validated items, without the idempotency key. Each field is written with its length,
// so text cannot move from one field to the next without changing the hash.
func Hash(items ...Input) []byte {
	h := sha256.New()
	field := func(s string) {
		_ = binary.Write(h, binary.BigEndian, uint64(len(s)))
		h.Write([]byte(s))
	}
	for _, in := range items {
		field(in.Recipient)
		field(in.Channel)
		if in.Content != nil {
			field("content")
			field(*in.Content)
		} else {
			field("template")
			field(in.Template)
		}
		vars := slices.Clone(in.Variables)
		slices.SortFunc(vars, func(a, b Variable) int { return strings.Compare(a.Name, b.Name) })
		field(fmt.Sprint(len(vars)))
		for _, v := range vars {
			field(v.Name)
			field(v.Value)
		}
		priority := in.Priority
		if priority == "" {
			priority = Normal.String()
		}
		field(priority)
		at := in.ScheduledAt
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			at = t.UTC().Format(time.RFC3339Nano)
		}
		field(at)
	}
	return h.Sum(nil)
}

// ValidIdempotencyKey reports whether s is 1 to 128 printable ASCII characters.
func ValidIdempotencyKey(s string) bool {
	if len(s) == 0 || len(s) > MaxIdempotencyKeyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

var templateName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidTemplateName reports whether s is 1 to 64 letters, digits, '_', '-' or '.'.
func ValidTemplateName(s string) bool {
	return len(s) > 0 && len(s) <= MaxTemplateNameLen && templateName.MatchString(s)
}

// MaxEchoedName bounds a name taken from the request into an error: an unknown field, a
// variable name (architecture §8, "Errors").
const MaxEchoedName = 64

// Echo returns a name from the request as it may appear in an error: at most 64 bytes, with
// every byte outside printable ASCII, and the quote and backslash that JSON would escape,
// replaced. So an error's size does not depend on the request.
func Echo(name string) string {
	if len(name) > MaxEchoedName {
		name = name[:MaxEchoedName]
	}
	b := []byte(name)
	for i, c := range b {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b[i] = '?'
		}
	}
	return string(b)
}
