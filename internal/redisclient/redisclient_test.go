package redisclient

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// SilentServer accepts TCP connections and never answers: a Redis that is paused or hung.
// A refused port would fail at once and prove nothing about deadlines.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// The go-redis default replaces the caller's context with a background one on the socket,
// so a deadline is ignored and the call waits for the client's own read timeout. Every
// client of this service must stop when its context ends.
func TestRedisClientHonoursContextDeadlines(t *testing.T) {
	c, err := New("redis://" + silentServer(t) + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const deadline = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	began := time.Now()
	err = c.Ping(ctx).Err()
	took := time.Since(began)
	if err == nil {
		t.Fatal("ping of a silent server succeeded")
	}
	if took > deadline+500*time.Millisecond {
		t.Fatalf("ping returned after %s with a deadline of %s (%v)", took, deadline, err)
	}
}

func TestNewRefusesAMalformedURL(t *testing.T) {
	if _, err := New("http://not-redis"); err == nil {
		t.Fatal("a non-redis URL was accepted")
	}
}

// The class of the defect, not only its instance: a Redis client built anywhere else would
// ignore deadlines again. No Go file outside this package may build one.
func TestRedisClientsAreBuiltOnlyHere(t *testing.T) {
	forbidden := regexp.MustCompile(`redis\.(NewClient|NewClusterClient|NewFailoverClient|NewUniversalClient|ParseURL)\(`)
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "redisclient") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := forbidden.FindIndex(src); loc != nil {
			t.Errorf("%s builds a Redis client (%s); use redisclient.New", path, src[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// §10: go-redis writes a plain-text line of its own on every failed dial; through LogTo they
// go to the process's JSON log, at most one per 10 s. A Redis outage of 4 minutes made 307
// lines on one worker before this (session 7, the Compose stack with Redis stopped).
func TestTheRedisClientsOwnLinesAreLoggedSparsely(t *testing.T) {
	var buf lockedBuffer
	LogTo(slog.New(slog.NewJSONHandler(&buf, nil)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens: every dial fails
	c, err := New("redis://" + addr + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_ = c.Ping(ctx).Err()
		cancel()
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"msg":"redis client"`) {
		t.Errorf("%d lines for 10 failed dials, want one JSON line of the Redis client:\n%s", len(lines), buf.String())
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
