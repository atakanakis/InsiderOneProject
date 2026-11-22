package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each kind of reference that does not resolve is reported with its file and line, and each
// kind that resolves is not.
func TestDocRefsReportsWhatDoesNotResolve(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/queue/queue.go", `package queue

import (
	"context"
	"net/http"
)

var _, _ = context.Background, http.ErrUseLastResponse

const claimSQL = "UPDATE"

type Policy struct{}

func (p Policy) Delay() {}

type Queue struct{}

func (q *Queue) Claim() {}

func NewQueue() *Queue { return nil }

var (
	ErrNotFound = error(nil)
)
`)
	write(t, root, "internal/queue/queue_test.go", "package queue\n\nfunc onlyInATest() {}\n")
	write(t, root, "cmd/worker/main.go", "package main\n\nfunc run() int { return 0 }\n")
	write(t, root, "docs/design.md", "# Design\n\n## 5. The queue: five operations\n\nText.\n")
	write(t, root, "docs/sessions/01.md", strings.Join([]string{
		"[ok](../design.md#5-the-queue-five-operations)", // 1
		"[no file](../missing.md)",                       // 2
		"[no heading](../design.md#6-the-dispatcher)",    // 3
		"[a site](https://example.com/x.md)",             // 4: not checked
		"```",                                            // 5
		"[in a code block](nothing.md)",                  // 6: not checked
		"```",                                            // 7
		"[here](#01)",                                    // 8: no such heading in this file
		"[titled](../missing-titled.md \"a title\")",     // 9: a link with a title
		"[ref style][r]",                                 // 10
		"[r]: ../missing-ref.md",                         // 11: a reference-style link's target
		"<a href=\"../missing-html.md\">x</a>",           // 12: an HTML link
		"~~~",                                            // 13
		"[in a tilde block](nothing.md)",                 // 14: not checked
		"~~~",                                            // 15
		"[ok titled](../design.md \"the design\")",       // 16
	}, "\n")+"\n")
	write(t, root, "docs/walkthrough.md", strings.Join([]string{
		"# Walkthrough", // 1
		"**Code:** [`internal/queue/queue.go`](../internal/queue/queue.go): `(*Queue).Claim`, `claimSQL`, `ErrNotFound`.",  // 2
		"**Code:** [`cmd/worker/main.go`](../cmd/worker/main.go): `run`, `start`.",                                         // 3: start is not there
		"Prose: `queue.NewQueue`, `queue.(*Queue).Claim`, `queue.Missing`, `(*Queue).Finish`.",                             // 4: two missing
		"Paths: `internal/queue/`, `cmd/api/main.go`, `docs/design.md`, `queue.onlyInATest`.",                              // 5: two missing
		"Not checked: `http.ErrUseLastResponse`, `GotConn`, `sms/email/push`, `/v1/notifications`.",                        // 6
		"**Code:** [`cmd/worker/main.go`](../cmd/worker/main.go): `claimSQL`.",                                             // 7: declared, in another file
		"Qualifiers: `queue.run`, `stroe.NewQueue`, `context.WithoutCancel`.",                                              // 8: run is main's; stroe is no package
		"Paths: `./internal/queue/queue.go`, `./cmd/missing.go`, `internal/queue/queue.go:7`, `internal/queue/gone.go:3`.", // 9: two missing
		"Receivers: `(Policy).Delay`, `(*Policy).Delay`, `(Queue).Claim`.",                                                 // 10: Claim has a pointer receiver
		"A span `attempts >=",                    // 11: does not close here
		"max_attempts` closes on the next line.", // 12
	}, "\n")+"\n")
	write(t, root, "README.md", "See [the design](docs/design.md) and [nothing](docs/none.md).\n")

	got, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, p := range got {
		lines = append(lines, p.String())
	}
	sort.Strings(lines)
	want := []string{
		"README.md:1: docs/none.md: no such file",
		"docs/sessions/01.md:2: ../missing.md: no such file",
		"docs/sessions/01.md:3: ../design.md#6-the-dispatcher: no such heading",
		"docs/sessions/01.md:8: #01: no such heading",
		"docs/walkthrough.md:3: start: not declared in cmd/worker/main.go",
		"docs/walkthrough.md:4: (*Queue).Finish: not declared",
		"docs/walkthrough.md:4: queue.Missing: not declared",
		"docs/walkthrough.md:5: cmd/api/main.go: no such file",
		"docs/walkthrough.md:5: queue.onlyInATest: not declared",
		"docs/sessions/01.md:9: ../missing-titled.md: no such file",
		"docs/sessions/01.md:11: ../missing-ref.md: no such file",
		"docs/sessions/01.md:12: ../missing-html.md: no such file",
		"docs/walkthrough.md:7: claimSQL: not declared in cmd/worker/main.go",
		"docs/walkthrough.md:8: queue.run: not declared",
		"docs/walkthrough.md:8: stroe.NewQueue: not a package of the repository or of its imports",
		"docs/walkthrough.md:9: ./cmd/missing.go: no such file",
		"docs/walkthrough.md:9: internal/queue/gone.go:3: no such file",
		"docs/walkthrough.md:10: (Queue).Claim: not declared",
		"docs/walkthrough.md:11: `: a code span does not close on its line",
		"docs/walkthrough.md:12: `: a code span does not close on its line",
	}
	sort.Strings(want)
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\n\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// The repository itself passes: this is the check that make check-docs and CI run.
func TestEveryLinkPathAndNameInTheDocsResolves(t *testing.T) {
	got, err := Check("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		t.Error(p.String())
	}
}
