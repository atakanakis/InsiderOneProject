package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func str(s string) *string { return &s }

func noTemplates(string) (*Compiled, Channel, bool) { return nil, "", false }

// prepare validates one item and returns its errors.
func prepare(in Input, lookup Lookup) (Prepared, []FieldError) {
	var errs Errors
	p := Prepare(in, "", lookup, &errs)
	return p, errs.List()
}

func valid(ch Channel) Input {
	to := map[Channel]string{SMS: "+905551234567", Email: "ada@example.com", Push: "device-token-0123456789"}[ch]
	return Input{Recipient: to, Channel: string(ch), Content: str("hello")}
}

func hasField(errs []FieldError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

// architecture §8: content after rendering is bounded per channel, in characters. The test
// uses a two-byte character, so a bound counted in bytes would fail it.
func TestContentIsBoundedPerChannelInCharacters(t *testing.T) {
	for _, ch := range Channels {
		limit := ContentLimit(ch)
		if limit == 0 {
			t.Fatalf("%s has no content bound", ch)
		}
		for _, n := range []int{limit - 1, limit, limit + 1} {
			in := valid(ch)
			in.Content = str(strings.Repeat("ç", n))
			_, errs := prepare(in, noTemplates)
			if refused := hasField(errs, "content"); refused != (n > limit) {
				t.Errorf("%s with %d characters (bound %d): refused=%v %v", ch, n, limit, refused, errs)
			}
		}
	}
}

func TestEveryFieldIsValidatedAndNamed(t *testing.T) {
	cases := map[string]Input{
		"recipient":      {Recipient: "05551234567", Channel: "sms", Content: str("x")},
		"channel":        {Recipient: "+905551234567", Channel: "fax", Content: str("x")},
		"priority":       {Recipient: "+905551234567", Channel: "sms", Content: str("x"), Priority: "urgent"},
		"scheduledAt":    {Recipient: "+905551234567", Channel: "sms", Content: str("x"), ScheduledAt: "tomorrow"},
		"content":        {Recipient: "+905551234567", Channel: "sms"},
		"idempotencyKey": {Recipient: "+905551234567", Channel: "sms", Content: str("x"), IdempotencyKey: "has space"},
		"template":       {Recipient: "+905551234567", Channel: "sms", Content: str("x"), Template: "both"},
	}
	for field, in := range cases {
		if _, errs := prepare(in, noTemplates); !hasField(errs, field) {
			t.Errorf("%s: errors %v do not name it", field, errs)
		}
	}
	for _, ch := range Channels {
		if _, errs := prepare(valid(ch), noTemplates); len(errs) != 0 {
			t.Errorf("valid %s refused: %v", ch, errs)
		}
	}
}

// Postgres refuses NUL in text; a row it refuses would be a 500 for a request that passed
// validation.
func TestContentAndVariablesMustNotContainNUL(t *testing.T) {
	in := valid(SMS)
	in.Content = str("a\x00b")
	if _, errs := prepare(in, noTemplates); !hasField(errs, "content") {
		t.Errorf("NUL in content accepted: %v", errs)
	}
	c, err := CompileTemplate("hi {{.name}}")
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (*Compiled, Channel, bool) { return c, SMS, true }
	in = Input{Recipient: "+905551234567", Channel: "sms", Template: "t", Variables: Variables{{"name", "a\x00"}}}
	if _, errs := prepare(in, lookup); len(errs) == 0 {
		t.Errorf("NUL in a variable accepted")
	}
}

func TestRecipientFormatsPerChannel(t *testing.T) {
	cases := []struct {
		ch Channel
		to string
		ok bool
	}{
		{SMS, "+905551234567", true}, {SMS, "+1234567", false}, {SMS, "905551234567", false},
		{SMS, "+90555123456789012", false}, {Email, "ada@example.com", true},
		{Email, "Ada <ada@example.com>", false}, {Email, "not-an-email", false},
		{Email, strings.Repeat("a", 242) + "@example.com", true}, // 254 bytes
		{Email, strings.Repeat("a", 243) + "@example.com", false},
		{Push, strings.Repeat("t", MinDeviceTokenLen), true}, {Push, strings.Repeat("t", MinDeviceTokenLen-1), false},
		{Push, strings.Repeat("t", MaxDeviceTokenLen), true}, {Push, strings.Repeat("t", MaxDeviceTokenLen+1), false},
		{Push, "token with spaces 123", false},
	}
	for _, c := range cases {
		in := valid(c.ch)
		in.Recipient = c.to
		_, errs := prepare(in, noTemplates)
		if ok := !hasField(errs, "recipient"); ok != c.ok {
			t.Errorf("%s %q (%d bytes): accepted=%v, want %v", c.ch, c.to, len(c.to), ok, c.ok)
		}
	}
}

func TestIdempotencyKeyAndTemplateNameBounds(t *testing.T) {
	for n, want := range map[int]bool{0: false, 1: true, MaxIdempotencyKeyLen: true, MaxIdempotencyKeyLen + 1: false} {
		if got := ValidIdempotencyKey(strings.Repeat("k", n)); got != want {
			t.Errorf("key of %d characters: %v, want %v", n, got, want)
		}
	}
	if ValidIdempotencyKey("tab\there") || ValidIdempotencyKey("é") {
		t.Error("a key outside printable ASCII accepted")
	}
	for n, want := range map[int]bool{0: false, 1: true, MaxTemplateNameLen: true, MaxTemplateNameLen + 1: false} {
		if got := ValidTemplateName(strings.Repeat("n", n)); got != want {
			t.Errorf("template name of %d characters: %v, want %v", n, got, want)
		}
	}
	if ValidTemplateName("a/b") || ValidTemplateName("a b") {
		t.Error("a template name with a separator accepted")
	}
}

// architecture §8: at most 100 field errors are kept, with a count of the rest.
func TestFieldErrorsAreKeptUpToTheirBound(t *testing.T) {
	for _, n := range []int{MaxFieldErrors - 1, MaxFieldErrors, MaxFieldErrors + 1, 5000} {
		var e Errors
		for i := range n {
			e.Add(fmt.Sprintf("f%d", i), "bad")
		}
		kept := min(n, MaxFieldErrors)
		if len(e.List()) != kept || e.More() != n-kept || e.Len() != n {
			t.Errorf("%d errors: kept %d, more %d, len %d", n, len(e.List()), e.More(), e.Len())
		}
	}
}

func decodeVariables(t *testing.T, n int) error {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`{`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"v%d":"x"`, i)
	}
	b.WriteString(`}`)
	var v Variables
	err := json.Unmarshal(b.Bytes(), &v)
	if err == nil && len(v) != n {
		t.Fatalf("decoded %d variables of %d", len(v), n)
	}
	return err
}

// architecture §8: at most 50 variables per notification, counted while they are decoded.
func TestVariablesAreCountedWhileDecoded(t *testing.T) {
	for _, n := range []int{MaxVariables - 1, MaxVariables} {
		if err := decodeVariables(t, n); err != nil {
			t.Errorf("%d variables: %v", n, err)
		}
	}
	for _, n := range []int{MaxVariables + 1, 100_000} {
		if err := decodeVariables(t, n); !errors.Is(err, ErrTooManyVariables) {
			t.Errorf("%d variables: %v, want ErrTooManyVariables", n, err)
		}
	}
	var v Variables
	for _, bad := range []string{`{"a":1}`, `{"a":"x","a":"y"}`, `["a"]`, `"a"`} {
		if err := json.Unmarshal([]byte(bad), &v); !errors.Is(err, ErrBadVariables) {
			t.Errorf("%s: %v, want ErrBadVariables", bad, err)
		}
	}
	if err := json.Unmarshal([]byte(`{"b":"2","a":"1"}`), &v); err != nil || len(v) != 2 || v[0].Name != "b" {
		t.Errorf("order not kept: %v %v", v, err)
	}
}

func TestVariableNamesAreBounded(t *testing.T) {
	at := strings.Repeat("n", MaxVariableNameLen)
	if _, err := CompileTemplate("{{." + at + "}}"); err != nil {
		t.Errorf("a name of %d characters refused: %v", MaxVariableNameLen, err)
	}
	if _, err := CompileTemplate("{{." + at + "n}}"); err == nil {
		t.Errorf("a name of %d characters accepted", MaxVariableNameLen+1)
	}
	for _, bad := range []string{"{{ .a }}", "{{.1a}}", "{{if .a}}x{{end}}", "{{.a", "{{", "{{xa}}", "{{-a}}"} {
		if _, err := CompileTemplate(bad); err == nil {
			t.Errorf("body %q accepted", bad)
		}
	}
}

func TestTemplateBodyBounds(t *testing.T) {
	var body strings.Builder
	for i := range MaxVariables {
		fmt.Fprintf(&body, "{{.v%d}}", i)
	}
	if _, err := CompileTemplate(body.String()); err != nil {
		t.Errorf("%d distinct variables refused: %v", MaxVariables, err)
	}
	if _, err := CompileTemplate(body.String() + "{{.extra}}"); err == nil {
		t.Errorf("%d distinct variables accepted", MaxVariables+1)
	}
	for _, ch := range Channels {
		limit := ContentLimit(ch)
		var errs Errors
		CheckTemplate("t", string(ch), strings.Repeat("ç", limit), &errs)
		if errs.Len() != 0 {
			t.Errorf("%s body at the bound refused: %v", ch, errs.List())
		}
		errs = Errors{}
		CheckTemplate("t", string(ch), strings.Repeat("ç", limit+1), &errs)
		if errs.Len() == 0 {
			t.Errorf("%s body over the bound accepted", ch)
		}
	}
}

// architecture §8: the rendered length is calculated from the compiled parts before
// anything is rendered, and matches what Render then produces.
func TestTheRenderedLengthIsMeasuredBeforeRendering(t *testing.T) {
	c, err := CompileTemplate("Hi {{.name}}, {{.name}}! Code {{.code}}.")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Variables(); len(got) != 2 || got[0] != "name" || got[1] != "code" {
		t.Errorf("variables %v", got)
	}
	vars := Variables{{"code", "42"}, {"name", "Çağrı"}}
	chars, size, err := c.Measure(vars)
	if err != nil {
		t.Fatal(err)
	}
	out := c.Render(vars)
	if out != "Hi Çağrı, Çağrı! Code 42." {
		t.Errorf("rendered %q", out)
	}
	if size != len(out) || chars != len([]rune(out)) {
		t.Errorf("measured %d chars %d bytes; rendered %d chars %d bytes", chars, size, len([]rune(out)), len(out))
	}
	if _, _, err := c.Measure(Variables{{"name", "x"}}); err == nil {
		t.Error("a missing variable accepted")
	}
	if _, _, err := c.Measure(append(vars, Variable{"unused", "x"})); err == nil {
		t.Error("an unused variable accepted")
	}
	// Placeholders render their value literally: a value that looks like a placeholder is
	// not expanded again.
	c, _ = CompileTemplate("{{.a}}")
	if out := c.Render(Variables{{"a", "{{.a}}"}}); out != "{{.a}}" {
		t.Errorf("rendered %q", out)
	}
}

func TestATemplateItemIsMeasuredAgainstItsChannel(t *testing.T) {
	c, _ := CompileTemplate("{{.v}}{{.v}}")
	lookup := func(string) (*Compiled, Channel, bool) { return c, SMS, true }
	in := Input{Recipient: "+905551234567", Channel: "sms", Template: "t", Variables: Variables{{"v", strings.Repeat("ç", 800)}}}
	p, errs := prepare(in, lookup)
	if len(errs) != 0 || p.Content() != strings.Repeat("ç", 1600) || p.Bytes != 3200 {
		t.Fatalf("at the bound: %v, %d bytes", errs, p.Bytes)
	}
	in.Variables[0].Value += "ç"
	if _, errs := prepare(in, lookup); !hasField(errs, "content") {
		t.Errorf("over the bound: %v", errs)
	}
	in.Channel = "email"
	if _, errs := prepare(in, lookup); !hasField(errs, "template") {
		t.Errorf("a template of another channel accepted: %v", errs)
	}
}

// I10: the hash is of the validated request, so forms of the same request hash the same and
// any change of content changes it.
func TestTheRequestHashIgnoresFormAndDefaults(t *testing.T) {
	a := Input{Recipient: "+905551234567", Channel: "sms", Content: str("hi"), ScheduledAt: "2030-01-01T12:00:00Z",
		Template: "", Variables: nil, IdempotencyKey: "k1"}
	b := a
	b.Priority = "normal"
	b.ScheduledAt = "2030-01-01T15:00:00+03:00"
	b.IdempotencyKey = "k2"
	if !bytes.Equal(Hash(a), Hash(b)) || len(Hash(a)) != 32 {
		t.Errorf("the same request hashes differently: %x %x", Hash(a), Hash(b))
	}
	t1 := Input{Recipient: "+905551234567", Channel: "sms", Template: "t", Variables: Variables{{"a", "1"}, {"b", "2"}}}
	t2 := t1
	t2.Variables = Variables{{"b", "2"}, {"a", "1"}}
	if !bytes.Equal(Hash(t1), Hash(t2)) {
		t.Error("the order of variables changed the hash")
	}
	for name, change := range map[string]func(*Input){
		"content":   func(i *Input) { i.Content = str("hi!") },
		"recipient": func(i *Input) { i.Recipient = "+905551234568" },
		"priority":  func(i *Input) { i.Priority = "high" },
		"schedule":  func(i *Input) { i.ScheduledAt = "2030-01-01T12:00:01Z" },
	} {
		c := a
		change(&c)
		if bytes.Equal(Hash(a), Hash(c)) {
			t.Errorf("a change of %s kept the hash", name)
		}
	}
	if bytes.Equal(Hash(a, b), Hash(a)) || bytes.Equal(Hash(a, t1), Hash(t1, a)) {
		t.Error("a batch hash ignores the items or their order")
	}
	// Field boundaries are part of the hash: moving text from one field to the next changes
	// it. A variable's value is followed by the priority; written without lengths, "bc" and
	// "normal" read the same as "b" and "cnormal".
	x := Input{Recipient: "+905551234567", Channel: "sms", Template: "t", Variables: Variables{{"a", "bc"}}}
	y := Input{Recipient: "+905551234567", Channel: "sms", Template: "t", Variables: Variables{{"a", "b"}}, Priority: "cnormal"}
	if bytes.Equal(Hash(x), Hash(y)) {
		t.Error("the hash does not separate fields")
	}
}
