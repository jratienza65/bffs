package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockAcquireAndRelease(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "state.json.lock")

	release, err := Lock(lockDir, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	info, err := os.Stat(lockDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("lock dir should exist as a directory: %v", err)
	}
	release()
	release() // idempotent
	if _, err := os.Stat(lockDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock dir should be removed on release: %v", err)
	}
}

func TestLockContendedTimesOut(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	release, err := Lock(lockDir, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer release()

	start := time.Now()
	_, err = Lock(lockDir, 10*time.Second, 60*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
	if _, serr := os.Stat(lockDir); serr != nil {
		t.Fatalf("a failed Lock must not remove the holder's dir: %v", serr)
	}
}

func TestLockWaitsForRelease(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	release, err := Lock(lockDir, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(80 * time.Millisecond)
		release()
	}()

	start := time.Now()
	release2, err := Lock(lockDir, 10*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("second Lock should succeed once released: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("acquired before the holder released: %v", elapsed)
	}
	release2()
	wg.Wait()
}

func TestLockStaleIsRemoved(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := Touch(lockDir, past); err != nil {
		t.Fatal(err)
	}

	release, err := Lock(lockDir, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("stale lock should be taken over: %v", err)
	}
	info, err := os.Stat(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("lock dir was not recreated: mtime %v", info.ModTime())
	}
	release()
}

func TestLockFreshHolderNotTreatedAsStale(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Lock(lockDir, time.Hour, 20*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked for a fresh holder, got %v", err)
	}
}

func TestLockStaleZeroDisablesCheck(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Touch(lockDir, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := Lock(lockDir, 0, 20*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("stale=0 must never remove a holder, got %v", err)
	}
}

func TestLockMissingParent(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "nope", "x.lock")
	_, err := Lock(lockDir, 10*time.Second, 0)
	if err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("want a non-ErrLocked error for a missing parent, got %v", err)
	}
}
