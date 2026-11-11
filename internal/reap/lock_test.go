package reap

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLock(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, LockName))
	if !strings.Contains(string(data), "pid="+strconv.Itoa(os.Getpid())) {
		t.Errorf("lock file: %q", data)
	}

	// A second run is refused, and told who holds the lock (this live process).
	_, err = Lock(dir)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "wait for it to finish") {
		t.Fatalf("second lock: %v", err)
	}

	release()
	if _, err := os.Stat(filepath.Join(dir, LockName)); !os.IsNotExist(err) {
		t.Error("release left the file")
	}
	release2, err := Lock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}

// A file left by a run that was killed holds no lock: the next run takes it.
func TestLock_LeftoverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LockName)
	if err := os.WriteFile(path, []byte("pid=999999999\nhost=elsewhere\nstarted=then\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("a leftover file blocked the lock: %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "pid="+strconv.Itoa(os.Getpid())) || strings.Contains(string(data), "elsewhere") {
		t.Errorf("lock file: %q", data)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("release left the file")
	}
}

// Of many runs at once, one holds the lock at a time, and none locks a file
// another has just removed.
func TestLock_Concurrent(t *testing.T) {
	dir := t.TempDir()
	var holders, taken atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				release, err := Lock(dir)
				if errors.Is(err, ErrLocked) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if holders.Add(1) != 1 {
					t.Error("two runs hold the lock")
				}
				taken.Add(1)
				holders.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
	if taken.Load() == 0 {
		t.Error("nobody took the lock")
	}
}

func TestLock_NoDir(t *testing.T) {
	if _, err := Lock(filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrLocked) {
		t.Errorf("got %v", err)
	}
}
