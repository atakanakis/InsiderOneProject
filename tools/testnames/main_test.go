package main

import (
	"os"
	"path/filepath"
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

func TestTestNamesCheckReportsATestThatDoesNotExist(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/x/x_test.go", "package x\n\nfunc TestExists(t *testing.T) {}\n")
	write(t, root, "docs/a.md", "Proven by `TestExists` and TestExists/subcase.\n\nAlso `TestMissing`.\n")
	write(t, root, "README.md", "See TestAlsoMissing.\n")
	write(t, root, "docs/code_test.go.txt", "func TestOnlyInAnExperimentFile(t *testing.T) {}\n")
	write(t, root, "docs/b.md", "TestOnlyInAnExperimentFile is not a test of the code.\n")
	write(t, root, "CLAUDE.md", "An example name, TestNotChecked, outside docs/.\n")

	got, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"TestMissing": 3, "TestAlsoMissing": 1, "TestOnlyInAnExperimentFile": 1}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for _, m := range got {
		if line, ok := want[m.Name]; !ok || line != m.Line {
			t.Errorf("unexpected %+v", m)
		}
	}
}

// The repository itself passes: this is the check CI runs.
func TestEveryTestNamedInTheDocsExists(t *testing.T) {
	got, err := Check("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		t.Errorf("%s:%d: %s does not exist", m.File, m.Line, m.Name)
	}
}
