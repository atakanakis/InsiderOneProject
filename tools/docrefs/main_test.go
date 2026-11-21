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

const claimSQL = "UPDATE"

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
	}, "\n")+"\n")
	write(t, root, "docs/walkthrough.md", strings.Join([]string{
		"# Walkthrough",                                                                    // 1
		"**Code:** [`internal/queue/queue.go`](../internal/queue/queue.go): `(*Queue).Claim`, `claimSQL`, `ErrNotFound`.", // 2
		"**Code:** [`cmd/worker/main.go`](../cmd/worker/main.go): `run`, `start`.",          // 3: start is not there
		"Prose: `queue.NewQueue`, `queue.(*Queue).Claim`, `queue.Missing`, `(*Queue).Finish`.", // 4: two missing
		"Paths: `internal/queue/`, `cmd/api/main.go`, `docs/design.md`, `queue.onlyInATest`.", // 5: two missing
		"Not checked: `http.ErrUseLastResponse`, `GotConn`, `sms/email/push`, `/v1/notifications`.", // 6
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
