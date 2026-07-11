//go:build !windows

package dns

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestWithHostsFileLock_Success(t *testing.T) {
	err := withHostsFileLock(func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWithHostsFileLock_FnError(t *testing.T) {
	sentinel := errors.New("callback failed")
	err := withHostsFileLock(func() error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestWithHostsFileLock_Concurrent(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "locktest-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	path := tmp.Name()
	tmp.Close()

	const goroutines = 2
	const iterations = 50

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range iterations {
				if err := withHostsFileLock(func() error {
					line := fmt.Sprintf("g%d-i%d\n", id, i)
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
					if err != nil {
						return err
					}
					defer f.Close()
					_, err = f.WriteString(line)
					return err
				}); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for e := range errs {
		t.Fatalf("goroutine error: %v", e)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}

	// Each iteration writes exactly one line. Count newlines.
	want := goroutines * iterations
	got := 0
	for _, b := range data {
		if b == '\n' {
			got++
		}
	}
	if got != want {
		t.Fatalf("expected %d lines, got %d (possible corruption)", want, got)
	}
}
