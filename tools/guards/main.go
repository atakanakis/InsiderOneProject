// Command guards checks that every guard is real (rule 13 in CLAUDE.md): disabling it turns
// a named test red.
//
// For each guard in list.go it:
//
//  1. checks that the named test exists and passes (a test that does not run proves nothing);
//  2. applies the mutation (replaces Old by New, which must occur exactly once in File) and
//     checks that the package still compiles (a mutation that does not compile proves nothing);
//  3. runs the named test and expects it to fail, by name;
//  4. restores the file, whatever happened.
//
// Postgres and Redis are started once, before the first guard, and every test process uses
// them (testkit.PostgresURLVar, testkit.RedisURLVar). Each test process still creates its own
// database and runs the migrations as they are on disk, so a guard in a migration is checked
// against a fresh schema. Starting the containers per test process took about 10 s per
// guard of an integration package (session 1's record).
//
// Usage: go run ./tools/guards [-only substring] [-from substring]
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// Guard is one line of code that enforces something, and the test that must notice when it
// is gone.
type Guard struct {
	Name    string // what the guard protects
	File    string // repository-relative path
	Old     string // text that must occur exactly once in File
	New     string // what replaces it: the guard disabled
	Package string // package of the test, such as ./internal/config
	Test    string // top-level test function that must fail
}

var restore struct {
	sync.Mutex
	file string
	data []byte
}

func main() {
	only := flag.String("only", "", "run only the guards whose name contains this text")
	from := flag.String("from", "", "run the guards from the first whose name contains this text, in list order")
	flag.Parse()

	// On Ctrl-C, put back the file being mutated before exiting.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		restoreFile()
		os.Exit(130)
	}()

	// The servers are this run's own, and their URLs go only to the test processes it
	// starts, never to the environment of the shell: each test process empties that Redis.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	pgURL, redisURL, stop, err := testkit.StartServers(ctx)
	cancel()
	if err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "start Postgres and Redis: %v\n", err)
		os.Exit(1)
	}
	testEnv = append(os.Environ(), testkit.PostgresURLVar+"="+pgURL, testkit.RedisURLVar+"="+redisURL)
	code := run(*only, *from)
	stop()
	os.Exit(code)
}

// testEnv is the environment of the test processes: this run's servers.
var testEnv []string

func run(only, from string) int {
	failed := 0
	ran := 0
	started := from == ""
	for _, g := range guards {
		started = started || strings.Contains(g.Name, from)
		if !started || only != "" && !strings.Contains(g.Name, only) {
			continue
		}
		ran++
		began := time.Now()
		err := check(g)
		status := "ok  "
		if err != nil {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s %-58s %-55s %5.1fs\n", status, g.Name, g.Test, time.Since(began).Seconds())
		if err != nil {
			fmt.Printf("     %v\n", err)
		}
	}
	fmt.Printf("\n%d guards, %d real, %d failed\n", ran, ran-failed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func check(g Guard) error {
	original, err := os.ReadFile(g.File)
	if err != nil {
		return err
	}
	if n := bytes.Count(original, []byte(g.Old)); n != 1 {
		return fmt.Errorf("%s: the guard's text occurs %d times, want 1; update tools/guards/list.go", g.File, n)
	}
	run := "^" + g.Test + "$"

	out, err := goTest(g.Package, run)
	if err != nil || !strings.Contains(out, "--- PASS: "+g.Test+" ") {
		return fmt.Errorf("before the mutation, %s did not pass:\n%s", g.Test, tail(out))
	}

	restore.Lock()
	restore.file, restore.data = g.File, original
	restore.Unlock()
	defer restoreFile()
	mutated := bytes.Replace(original, []byte(g.Old), []byte(g.New), 1)
	if err := os.WriteFile(g.File, mutated, 0o600); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	bin := filepath.Join(os.TempDir(), "guards-compile-check.test")
	defer func() { _ = os.Remove(bin) }()
	if out, err := exec.CommandContext(ctx, "go", "test", "-c", "-o", bin, g.Package).CombinedOutput(); err != nil {
		return fmt.Errorf("the mutation does not compile, so it proves nothing:\n%s", tail(string(out)))
	}

	out, err = goTest(g.Package, run)
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: "+g.Test+" ") {
		return fmt.Errorf("with the guard disabled, %s did not fail:\n%s", g.Test, tail(out))
	}
	return nil
}

// commandTimeout bounds each go command, so a mutation that makes a test hang fails the
// guard instead of hanging the run.
const commandTimeout = 5 * time.Minute

func goTest(pkg, run string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-run", run, pkg)
	cmd.Env = testEnv
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func restoreFile() {
	restore.Lock()
	defer restore.Unlock()
	if restore.file != "" {
		_ = os.WriteFile(restore.file, restore.data, 0o600)
		restore.file, restore.data = "", nil
	}
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return "     | " + strings.Join(lines, "\n     | ")
}
