// Command docrefs checks that what the documents point at exists (architecture §14).
//
//  1. Links, in every Markdown file under docs/, in README.md and in CLAUDE.md. Every relative
//     link names a file or directory that exists, and, when it names a heading of a Markdown
//     file (`file.md#anchor`, or `#anchor` in the same file), a heading with that anchor, made
//     as GitHub makes it. A link is an inline link, with or without a title, the target of a
//     reference-style definition (`[name]: target`), or an HTML `href`.
//  2. Paths and Go names, in README.md and docs/walkthrough.md only. A path written in
//     backticks that starts with a top-level directory of the repository (with or without
//     "./", with or without a ":line" after it), or names one of its top-level files, exists.
//     A Go name written in backticks is declared in the code, outside the tests: on a line that
//     starts with "**Code:**", every name of the line is declared in a Go file the line links;
//     elsewhere, a method written as `(*Type).name` is declared on that type in some package,
//     and a name qualified by a package of the repository (`store.Claim`,
//     `store.(*Queue).Claim`) is declared in that package. `(Type).name` needs a method with a
//     value receiver; `(*Type).name` takes either, as in Go. A qualifier that is neither a
//     package of the repository nor a package its code imports (`http.ErrUseLastResponse`
//     is not checked further) is reported, so a typo or a renamed package does not pass
//     unchecked. A line with a code span that does not close on it is reported too, because
//     its spans cannot be read.
//
// Fenced code blocks (``` or ~~~) are not read. A document that points at something that does
// not exist is a claim without evidence (CLAUDE.md, rule 12), so the command exits with status
// 1 and lists each reference with its file and line. It checks that a name exists, not that
// what a document says about it is true.
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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// codeDocs are the documents whose paths and Go names are checked; every document's links are.
var codeDocs = map[string]bool{"README.md": true, "docs/walkthrough.md": true}

// rootFiles are the top-level files a document may name without a directory.
var rootFiles = map[string]bool{"Makefile": true, "Dockerfile": true, "docker-compose.yml": true,
	"go.mod": true, "README.md": true, "CLAUDE.md": true, ".golangci.yml": true}

// fileExts are the extensions of a file name written alone (`main.go`), which looks like a
// qualified name and is not one.
var fileExts = map[string]bool{"go": true, "md": true, "yml": true, "yaml": true, "sql": true,
	"sh": true, "txt": true, "json": true, "mod": true, "sum": true}

var (
	link      = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	refDef    = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*(\S+)`)
	href      = regexp.MustCompile(`href="([^"]+)"`)
	span      = regexp.MustCompile("`([^`]+)`")
	method    = regexp.MustCompile(`^\((\*?)([A-Za-z_]\w*)\)\.([A-Za-z_]\w*)$`)
	qualified = regexp.MustCompile(`^([a-z]\w*)\.(?:\((\*?)([A-Za-z_]\w*)\)\.)?([A-Za-z_]\w*)$`)
	ident     = regexp.MustCompile(`^[A-Za-z_]\w*$`)
	repoPath  = regexp.MustCompile(`^(?:\./)?(?:cmd|internal|tools|docs|deploy|\.github|\.claude)(?:/[\w.-]+)*/?(?::\d+(?::\d+)?)?$`)
	lineAfter = regexp.MustCompile(`(?::\d+)+$`)
	heading   = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	inline    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	version   = regexp.MustCompile(`^v\d+$`)
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
	fmt.Println("docrefs: every link of the documents, and every path and Go name of the README and the walkthrough, exists")
}

// declarations are the names declared outside the tests. A method is recorded as "*Type.name",
// and also as "Type.name" when its receiver is a value.
type declarations struct {
	byFile  map[string]map[string]bool // repository-relative file
	byPkg   map[string]map[string]bool // package name; package main is left out
	all     map[string]bool            // every name of every file
	imports map[string]bool            // the names of the packages the code imports
}

// Check returns every reference of the documents under root that does not resolve.
func Check(root string) ([]Problem, error) {
	d := declarations{byFile: map[string]map[string]bool{}, byPkg: map[string]map[string]bool{},
		all: map[string]bool{}, imports: map[string]bool{}}
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

// add records the top-level declarations and the imports of one Go file.
func (d declarations) add(file, rel string) error {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			return err
		}
		name := path.Base(p)
		if version.MatchString(name) {
			name = path.Base(path.Dir(p))
		}
		if im.Name != nil {
			name = im.Name.Name
		}
		d.imports[name] = true
	}
	names := map[string]bool{}
	for _, decl := range f.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			if x.Recv == nil || len(x.Recv.List) == 0 {
				names[x.Name.Name] = true
				continue
			}
			if t, star := receiver(x.Recv.List[0].Type); t != "" {
				names["*"+t+"."+x.Name.Name] = true
				if !star { // a value receiver's method is in the method sets of T and *T
					names[t+"."+x.Name.Name] = true
				}
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
	for n := range names {
		d.all[n] = true
	}
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

// receiver is the name of a method's receiver type, T for T, *T, T[P] and *T[P], and whether
// it is a pointer.
func receiver(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.StarExpr:
		t, _ := receiver(x.X)
		return t, true
	case *ast.IndexExpr:
		return receiver(x.X)
	case *ast.IndexListExpr:
		return receiver(x.X)
	case *ast.Ident:
		return x.Name, false
	}
	return "", false
}

// key is the name a reference looks for: name, or "*Type.name" or "Type.name" for a method.
func key(star, typ, name string) string {
	if typ == "" {
		return name
	}
	return star + typ + "." + name
}

// fence reports whether a line opens or closes a fenced code block.
func fence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
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
		if fence(line) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		// Links, with the code spans blanked, so that a link written as code is not read.
		stripped := span.ReplaceAllString(line, "`x`")
		var targets []string
		for _, m := range link.FindAllStringSubmatch(stripped, -1) {
			targets = append(targets, m[1])
		}
		if m := refDef.FindStringSubmatch(stripped); m != nil {
			targets = append(targets, m[1])
		}
		for _, m := range href.FindAllStringSubmatch(stripped, -1) {
			targets = append(targets, m[1])
		}
		var linked []string
		for _, target := range targets {
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
		if strings.Count(line, "`")%2 == 1 {
			report(n, "`", "a code span does not close on its line")
			continue
		}
		code := strings.HasPrefix(strings.TrimSpace(line), "**Code:**")
		for _, m := range span.FindAllStringSubmatch(line, -1) {
			ref := m[1]
			switch {
			case repoPath.MatchString(ref) || rootFiles[ref]:
				file := strings.TrimPrefix(lineAfter.ReplaceAllString(ref, ""), "./")
				if _, err := os.Stat(filepath.Join(root, file)); err != nil {
					report(n, ref, "no such file")
				}
			case code && (method.MatchString(ref) || ident.MatchString(ref)):
				name := ref
				if mm := method.FindStringSubmatch(ref); mm != nil {
					name = key(mm[1], mm[2], mm[3])
				}
				if !declaredIn(d, linked, name) {
					report(n, ref, "not declared in "+strings.Join(goFiles(linked), ", "))
				}
			case method.MatchString(ref):
				mm := method.FindStringSubmatch(ref)
				if !d.all[key(mm[1], mm[2], mm[3])] {
					report(n, ref, "not declared")
				}
			case qualified.MatchString(ref):
				mm := qualified.FindStringSubmatch(ref)
				pkg, name := mm[1], key(mm[2], mm[3], mm[4])
				if mm[3] == "" && fileExts[mm[4]] {
					continue // a file name, such as main.go
				}
				names, ours := d.byPkg[pkg]
				switch {
				case !ours && !d.imports[pkg]:
					report(n, ref, "not a package of the repository or of its imports")
				case ours && !names[name]:
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
func headings(file string) (map[string]bool, error) {
	lines, err := readLines(file)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	seen := map[string]int{}
	fenced := false
	for _, line := range lines {
		if fence(line) {
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

func readLines(file string) ([]string, error) {
	f, err := os.Open(file)
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
