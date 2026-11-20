package main

import (
	"bytes"
	"os"
	"testing"
)

// Every guard's text occurs exactly once in its file. make guards checks this too, one guard
// at a time and after minutes; this test finds a guard that a change of its file left behind
// in the fast suite (session 7: two guards no longer matched after the event's form changed).
func TestEveryGuardsTextOccursOnceInItsFile(t *testing.T) {
	names := map[string]bool{}
	for _, g := range guards {
		if names[g.Name] {
			t.Errorf("two guards are named %q", g.Name)
		}
		names[g.Name] = true
		b, err := os.ReadFile("../../" + g.File)
		if err != nil {
			t.Errorf("%s: %v", g.Name, err)
			continue
		}
		if n := bytes.Count(b, []byte(g.Old)); n != 1 {
			t.Errorf("%s: its text occurs %d times in %s, want 1", g.Name, n, g.File)
		}
	}
}
