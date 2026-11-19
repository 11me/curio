package curio

import (
	"errors"
	"fmt"
	"io"
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

// ListenSocket listens on a Unix socket whose folder belongs to this user
// with mode 0700 and whose parent folders other users cannot change, so no
// other user can serve or swap a socket at that path. curio makes a missing
// folder (mode 0700) but never changes the mode of an existing one. It
// replaces a stale curio socket only when nothing listens on it.
func ListenSocket(path string) (net.Listener, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket %q: use an absolute path", path)
	}
	dir, err := privateSocketDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	if len(path) > 100 {
		return nil, fmt.Errorf("socket %q: path is too long for a Unix socket", path)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
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

// privateSocketDir checks (and if missing, makes) the socket folder and
// returns its real path.
func privateSocketDir(dir string) (string, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
		if err != nil {
			return "", fmt.Errorf("socket folder parent: %w", err)
		}
		if err := checkAncestors(parent); err != nil {
			return "", err
		}
		if err := os.Mkdir(filepath.Join(parent, filepath.Base(dir)), 0o700); err != nil {
			return "", fmt.Errorf("make socket folder: %w", err)
		}
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("socket folder: %w", err)
	}
	info, err := os.Lstat(realDir)
	if err != nil {
		return "", fmt.Errorf("socket folder: %w", err)
	}
	if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("socket folder %s must be a folder owned by you with mode 0700; curio does not change it", realDir)
	}
	if err := checkAncestors(filepath.Dir(realDir)); err != nil {
		return "", err
	}
	return realDir, nil
}

// checkAncestors refuses a path when any folder on it belongs to another
// user (not root) or lets group or other users write, because such a user
// could rename a folder and put a different socket at the same path.
func checkAncestors(dir string) error {
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("check %s: %w", d, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("check %s: no owner information", d)
		}
		if (int(st.Uid) != os.Getuid() && st.Uid != 0) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("socket path is not safe: other users can change %s; use a folder in your home", d)
		}
		if d == filepath.Dir(d) {
			return nil
		}
	}
}

// removeStaleSocket deletes path only when it is this user's stream socket
// with no listener (connection refused). A live socket, a socket of another
// type and any other file stay.
func removeStaleSocket(path string) error {
	old, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	if old.Mode()&os.ModeSocket == 0 || !ownedByMe(old) {
		return fmt.Errorf("%s exists and is not a curio socket", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("another curio is already serving on %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s is in use by another program: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// AcquireRunLock takes an exclusive lock on dataDir/curio.lock for the life
// of the process, so only one curio (service or --once) fetches and scores
// with a data folder at a time.
func AcquireRunLock(dataDir string) (io.Closer, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, "curio.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // data folder from the operator
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another curio is running with the data folder %s", dataDir)
		}
		return nil, fmt.Errorf("run lock: %w", err)
	}
	return f, nil
}
