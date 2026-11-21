// Command docrefs checks that what the documents point at exists (architecture §14).
//
//  1. Links. Every relative link of a Markdown file under docs/, of README.md and of CLAUDE.md
//     names a file or directory that exists, and, when it names a heading of a Markdown file
//     (`file.md#anchor`, or `#anchor` in the same file), a heading with that anchor, made as
//     GitHub makes it.
//  2. Paths and Go names, in README.md and docs/walkthrough.md. A path written in backticks
//     that starts with a top-level directory of the repository, or names one of its top-level
//     files, exists. A Go name written in backticks is declared in the code, outside the
//     tests: on a line that starts with "**Code:**", every name of the line is declared in a
//     Go file the line links; elsewhere, a method written as `(*Type).name` is declared on
//     that type in some package, and a name qualified by a package of the repository
//     (`store.Claim`, `store.(*Queue).Claim`) is declared in that package. A name qualified by
//     another package (`http.ErrUseLastResponse`) is not checked.
//
// Fenced code blocks are not read. A document that points at something that does not exist is
// a claim without evidence (CLAUDE.md, rule 12), so the command exits with status 1 and lists
// each reference with its file and line.
//
// Usage: go run ./tools/docrefs [repository root]
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// codeDocs are the documents whose paths and Go names are checked; every document's links are.
var codeDocs = map[string]bool{"README.md": true, "docs/walkthrough.md": true}

// rootFiles are the top-level files a document may name without a directory.
var rootFiles = map[string]bool{"Makefile": true, "Dockerfile": true, "docker-compose.yml": true,
	"go.mod": true, "README.md": true, "CLAUDE.md": true, ".golangci.yml": true}

var (
	link      = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	span      = regexp.MustCompile("`([^`]+)`")
	method    = regexp.MustCompile(`^\(\*?([A-Za-z_]\w*)\)\.([A-Za-z_]\w*)$`)
	qualified = regexp.MustCompile(`^([a-z]\w*)\.(?:\(\*?([A-Za-z_]\w*)\)\.)?([A-Za-z_]\w*)$`)
	ident     = regexp.MustCompile(`^[A-Za-z_]\w*$`)
	repoPath  = regexp.MustCompile(`^(?:cmd|internal|tools|docs|deploy|\.github|\.claude)(?:/[\w.-]+)*/?$`)
	heading   = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	inline    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// Problem is one reference that does not resolve.
type Problem struct {
	File string
	Line int
	Ref  string
	Why  string
}

func (p Problem) String() string { return fmt.Sprintf("%s:%d: %s: %s", p.File, p.Line, p.Ref, p.Why) }

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	problems, err := Check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docrefs:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	fmt.Println("docrefs: every link, path and Go name the documents point at exists")
}

// declarations are the names declared outside the tests. A method is recorded as "Type.name".
type declarations struct {
	byFile  map[string]map[string]bool // repository-relative file
	byPkg   map[string]map[string]bool // package name; package main is left out
	methods map[string]bool
}

// Check returns every reference of the documents under root that does not resolve.
func Check(root string) ([]Problem, error) {
	d := declarations{byFile: map[string]map[string]bool{}, byPkg: map[string]map[string]bool{}, methods: map[string]bool{}}
	var docs []string
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if rel != "." && strings.HasPrefix(e.Name(), ".") || rel == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(rel, ".md") && (strings.HasPrefix(rel, "docs/") || rel == "README.md" || rel == "CLAUDE.md"):
			docs = append(docs, rel)
		case strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") && !strings.HasPrefix(rel, "docs/"):
			return d.add(path, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(docs)
	anchors := map[string]map[string]bool{}
	var problems []Problem
	for _, doc := range docs {
		ps, err := checkDoc(root, doc, d, anchors)
		if err != nil {
			return nil, err
		}
		problems = append(problems, ps...)
	}
	return problems, nil
}

// add records the top-level declarations of one Go file.
func (d declarations) add(path, rel string) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, decl := range f.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			if x.Recv == nil || len(x.Recv.List) == 0 {
				names[x.Name.Name] = true
				continue
			}
			if t := receiver(x.Recv.List[0].Type); t != "" {
				names[t+"."+x.Name.Name] = true
				d.methods[t+"."+x.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, s := range x.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					names[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names[n.Name] = true
					}
				}
			}
		}
	}
	d.byFile[rel] = names
	if pkg := f.Name.Name; pkg != "main" {
		if d.byPkg[pkg] == nil {
			d.byPkg[pkg] = map[string]bool{}
		}
		for n := range names {
			d.byPkg[pkg][n] = true
		}
	}
	return nil
}

// receiver is the name of a method's receiver type: T for T, *T, T[P] and *T[P].
func receiver(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return receiver(x.X)
	case *ast.IndexExpr:
		return receiver(x.X)
	case *ast.IndexListExpr:
		return receiver(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// checkDoc checks the links of one document and, for the code documents, its paths and names.
func checkDoc(root, doc string, d declarations, anchors map[string]map[string]bool) ([]Problem, error) {
	lines, err := readLines(filepath.Join(root, doc))
	if err != nil {
		return nil, err
	}
	var problems []Problem
	report := func(line int, ref, why string) {
		problems = append(problems, Problem{File: doc, Line: line, Ref: ref, Why: why})
	}
	dir := filepath.ToSlash(filepath.Dir(doc))
	fenced := false
	for i, line := range lines {
		n := i + 1
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		// Links, with the code spans blanked, so that a link written as code is not read.
		var linked []string
		for _, m := range link.FindAllStringSubmatch(span.ReplaceAllString(line, "`x`"), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			rel := doc
			if file != "" {
				rel = filepath.ToSlash(filepath.Clean(filepath.Join(dir, file)))
				if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
					report(n, target, "no such file")
					continue
				}
				linked = append(linked, rel)
			}
			if anchor == "" || !strings.HasSuffix(rel, ".md") {
				continue
			}
			if anchors[rel] == nil {
				if anchors[rel], err = headings(filepath.Join(root, rel)); err != nil {
					return nil, err
				}
			}
			if !anchors[rel][anchor] {
				report(n, target, "no such heading")
			}
		}
		if !codeDocs[doc] {
			continue
		}
		code := strings.HasPrefix(strings.TrimSpace(line), "**Code:**")
		for _, m := range span.FindAllStringSubmatch(line, -1) {
			ref := m[1]
			switch {
			case repoPath.MatchString(ref) || rootFiles[ref]:
				if _, err := os.Stat(filepath.Join(root, ref)); err != nil {
					report(n, ref, "no such file")
				}
			case code && (method.MatchString(ref) || ident.MatchString(ref)):
				name := ref
				if mm := method.FindStringSubmatch(ref); mm != nil {
					name = mm[1] + "." + mm[2]
				}
				if !declaredIn(d, linked, name) {
					report(n, ref, "not declared in "+strings.Join(goFiles(linked), ", "))
				}
			case method.MatchString(ref):
				mm := method.FindStringSubmatch(ref)
				if !d.methods[mm[1]+"."+mm[2]] {
					report(n, ref, "not declared")
				}
			case qualified.MatchString(ref):
				mm := qualified.FindStringSubmatch(ref)
				names, ours := d.byPkg[mm[1]]
				if !ours {
					continue // another package's name: not checked
				}
				name := mm[3]
				if mm[2] != "" {
					name = mm[2] + "." + mm[3]
				}
				if !names[name] {
					report(n, ref, "not declared")
				}
			}
		}
	}
	return problems, nil
}

func declaredIn(d declarations, files []string, name string) bool {
	for _, f := range files {
		if d.byFile[f][name] {
			return true
		}
	}
	return false
}

func goFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, ".go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{"(no Go file linked on the line)"}
	}
	return out
}

// headings returns the anchors GitHub makes for the headings of a Markdown file: the heading's
// text without its markup, in lower case, with every character that is not a letter, a digit,
// a space, '-' or '_' removed and each space made a '-'; a repeated anchor gets "-1", "-2", ....
func headings(path string) (map[string]bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	seen := map[string]int{}
	fenced := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		m := heading.FindStringSubmatch(line)
		if fenced || m == nil {
			continue
		}
		text := inline.ReplaceAllString(m[1], "$1")
		var b strings.Builder
		for _, r := range strings.ToLower(text) {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('-')
			}
		}
		a := b.String()
		if k := seen[a]; k > 0 {
			out[fmt.Sprintf("%s-%d", a, k)] = true
		} else {
			out[a] = true
		}
		seen[a]++
	}
	return out, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}
