package redisclient

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
