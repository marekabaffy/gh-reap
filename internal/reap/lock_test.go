package reap

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestLock_StaleAndForeign(t *testing.T) {
	host, _ := os.Hostname()
	tests := []struct {
		name, content, want string
	}{
		{"dead process here", "pid=999999999\nhost=" + host + "\nstarted=t\n", "no longer running, so the lock is stale"},
		{"other host", "pid=1\nhost=some-other-host\nstarted=t\n", "if no gh reap is running, delete the file"},
		{"unreadable", "garbage", "pid unknown on unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, LockName), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Lock(dir)
			if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(filepath.Join(dir, LockName)); err != nil {
				t.Error("a refused run must not touch the lock")
			}
		})
	}
}

func TestLock_NoDir(t *testing.T) {
	if _, err := Lock(filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrLocked) {
		t.Errorf("got %v", err)
	}
}
