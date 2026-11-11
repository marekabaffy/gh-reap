package reap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LockName is the lock file gh reap creates in the repository's common git dir,
// shared by the main checkout and every worktree.
const LockName = "gh-reap.lock"

// ErrLocked is returned when another run holds the lock.
var ErrLocked = errors.New("another gh reap run holds the lock")

// Lock takes the repository lock: flock(2) on the lock file, so of two runs
// exactly one succeeds, and the kernel releases the lock when its holder exits,
// however it exits. A file left behind by a killed run locks nothing. It returns
// the function that releases the lock and removes the file.
func Lock(commonDir string) (func(), error) {
	path := filepath.Join(commonDir, LockName)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("%w: %s", ErrLocked, describeLock(path))
			}
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		// The previous holder removes the file as it releases. If it did so between
		// the open and the flock, this lock is on a file no other run can see: retry.
		if !sameFile(f, path) {
			_ = f.Close()
			continue
		}
		host, _ := os.Hostname()
		_ = f.Truncate(0)
		_, _ = fmt.Fprintf(f, "pid=%d\nhost=%s\nstarted=%s\n", os.Getpid(), host, time.Now().Format(time.RFC3339))
		return func() {
			// Remove before unlocking, so no run can lock the file about to vanish.
			_ = os.Remove(path)
			_ = f.Close()
		}, nil
	}
}

// sameFile reports whether path still names the open file f.
func sameFile(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(held, named)
}

// describeLock says who holds the lock, from what the holder wrote in the file.
func describeLock(path string) string {
	data, _ := os.ReadFile(path)
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	return fmt.Sprintf("%s (pid %s on %s, since %s); wait for it to finish", path, orUnknown(fields["pid"]), orUnknown(fields["host"]), orUnknown(fields["started"]))
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
