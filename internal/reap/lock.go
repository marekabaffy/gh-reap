package reap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockName is the lock file gh reap creates in the repository's common git dir,
// shared by the main checkout and every worktree.
const LockName = "gh-reap.lock"

// ErrLocked is returned when another run holds the lock.
var ErrLocked = errors.New("another gh reap run holds the lock")

// Lock takes the repository lock by creating the file exclusively (O_EXCL), so of
// two runs exactly one succeeds. It returns the function that releases it.
func Lock(commonDir string) (func(), error) {
	path := filepath.Join(commonDir, LockName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrLocked, describeLock(path))
	}
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	fmt.Fprintf(f, "pid=%d\nhost=%s\nstarted=%s\n", os.Getpid(), host, time.Now().Format(time.RFC3339))
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// describeLock says who holds a lock and, when it can tell, that the holder is gone.
func describeLock(path string) string {
	data, _ := os.ReadFile(path)
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	msg := fmt.Sprintf("%s (pid %s on %s, since %s)", path, orUnknown(fields["pid"]), orUnknown(fields["host"]), orUnknown(fields["started"]))
	host, _ := os.Hostname()
	if pid, err := strconv.Atoi(fields["pid"]); err == nil && fields["host"] == host && !alive(pid) {
		return msg + "; that process is no longer running, so the lock is stale: delete the file and run again"
	}
	return msg + "; wait for it to finish, or, if no gh reap is running, delete the file and run again"
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, fs.ErrPermission)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
