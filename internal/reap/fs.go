package reap

import "path/filepath"

// canonical resolves symlinks (macOS /var is /private/var) so paths compare equal
// to what lsof reports. A path that cannot be resolved is returned cleaned.
func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
