// Command testnames checks that every test named in the documents exists in the code.
//
// A document names a test by writing its function name, such as TestStatusTransitionsFollowTheLifecycle
// (a subtest path after "/" is ignored). Every Markdown file under docs/, and README.md, is
// read. A name is found when some _test.go file outside docs/ declares `func <Name>(`.
// A document that mentions a test which does not exist is a claim without evidence, so the
// command exits with status 1 and lists each one with its file and line.
//
// Usage: go run ./tools/testnames [repository root]
package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	named    = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)
	declared = regexp.MustCompile(`(?m)^func (Test[A-Z][A-Za-z0-9_]*)\(`)
)

// Missing is a test named in a document that does not exist.
type Missing struct {
	Name string
	File string
	Line int
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	missing, err := Check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testnames:", err)
		os.Exit(2)
	}
	for _, m := range missing {
		fmt.Printf("%s:%d: %s is named here but no such test exists\n", m.File, m.Line, m.Name)
	}
	if len(missing) > 0 {
		os.Exit(1)
	}
	fmt.Println("testnames: every test named in docs/ and README.md exists")
}

// Check returns every test named in the documents under root that does not exist.
func Check(root string) ([]Missing, error) {
	tests := map[string]bool{}
	var docs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == ".git" || rel == "vendor" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		inDocs := strings.HasPrefix(rel, "docs"+string(filepath.Separator))
		switch {
		case inDocs && strings.HasSuffix(path, ".md"), rel == "README.md":
			docs = append(docs, path)
		case !inDocs && strings.HasSuffix(path, "_test.go"):
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range declared.FindAllSubmatch(src, -1) {
				tests[string(m[1])] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(docs)
	var missing []Missing
	for _, path := range docs {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for line := 1; sc.Scan(); line++ {
			for _, name := range named.FindAllString(sc.Text(), -1) {
				if !tests[name] {
					missing = append(missing, Missing{Name: name, File: path, Line: line})
				}
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return missing, nil
}
