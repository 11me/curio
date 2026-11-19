package curio

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket is the API socket path. Its folder is private to the owner,
// so only the owner's processes can serve or call the API there.
const DefaultSocket = "~/.curio/curio.sock"

// ExpandHome turns a leading "~/" into the home folder.
func ExpandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home folder: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

// ListenSocket listens on a Unix socket in a folder that only this user can
// open (mode 0700, owned by this user). It refuses a folder owned by another
// user and a socket that another curio still serves; it replaces a stale one.
func ListenSocket(path string) (net.Listener, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket %q: use an absolute path", path)
	}
	if len(path) > 100 {
		return nil, fmt.Errorf("socket %q: path is too long for a Unix socket", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket folder: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("socket folder: %w", err)
	}
	if !info.IsDir() || !ownedByMe(info) {
		return nil, fmt.Errorf("socket folder %s must be a folder owned by this user", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a folder needs the x bit; 0700 is owner only
			return nil, fmt.Errorf("make socket folder private: %w", err)
		}
	}
	if old, err := os.Lstat(path); err == nil {
		if old.Mode()&os.ModeSocket == 0 || !ownedByMe(old) {
			return nil, fmt.Errorf("%s exists and is not a curio socket", path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another curio is already serving on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("make socket private: %w", err)
	}
	return ln, nil
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
