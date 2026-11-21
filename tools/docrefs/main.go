// Command docrefs checks that what the documents point at exists. Not written yet.
package main

import "fmt"

// Problem is one reference that does not resolve.
type Problem struct {
	File string
	Line int
	Ref  string
	Why  string
}

func (p Problem) String() string { return fmt.Sprintf("%s:%d: %s: %s", p.File, p.Line, p.Ref, p.Why) }

// Check returns every reference of the documents under root that does not resolve.
func Check(root string) ([]Problem, error) { return nil, nil }

func main() {}
