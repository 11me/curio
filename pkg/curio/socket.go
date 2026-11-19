package curio

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// SocketPath is the one API socket of a data folder. Two data folders never
// share a socket, and its folder is made by curio itself.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, "sock", "curio.sock")
}

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

// RunLock is an exclusive lock on dataDir/curio.lock for the life of the
// process: only one curio (service or --once) fetches, scores and serves with
// a data folder at a time.
type RunLock struct {
	dir string
	f   *os.File
}

// AcquireRunLock resolves and checks the data folder with ResolveDataDir and
// then takes its lock, in the resolved folder, without waiting. The lock file is the first file any curio run
// opens in the data folder, so no run (service or --once) uses an unsafe one.
func AcquireRunLock(dataDir string) (*RunLock, error) {
	resolved, err := ResolveDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(resolved, "curio.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // checked data folder
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
	return &RunLock{dir: resolved, f: f}, nil
}

// Dir is the checked, resolved data folder. Use only this path for files in
// the data folder: it has no symlinks that could be changed after the check.
func (l *RunLock) Dir() string { return l.dir }

// Close releases the lock.
func (l *RunLock) Close() error { return l.f.Close() }

// Listen serves on SocketPath of the locked data folder. It checks the data
// folder again (ResolveDataDir) and the socket folder with the same rules, and
// the socket folder must have mode 0700. curio makes the socket
// folder (mode 0700) when it is missing and never changes an existing one. An
// old socket is removed only here, while the data folder lock is held, so no
// other curio can be serving on it.
func (l *RunLock) Listen() (net.Listener, error) {
	// l.dir is resolved; a folder on it that became a symlink resolves elsewhere
	dataDir, err := ResolveDataDir(l.dir)
	if err != nil {
		return nil, err
	}
	if dataDir != l.dir {
		return nil, fmt.Errorf("data folder %s changed after the check", l.dir)
	}
	sockDir := filepath.Dir(SocketPath(dataDir))
	if _, err := os.Lstat(sockDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(sockDir, 0o700); err != nil {
			return nil, fmt.Errorf("make socket folder: %w", err)
		}
	}
	info, err := os.Lstat(sockDir)
	if err != nil {
		return nil, fmt.Errorf("socket folder: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("socket folder %s must be a folder with mode 0700; curio does not change it", sockDir)
	}
	if err := checkFolder(sockDir); err != nil {
		return nil, err
	}
	path := SocketPath(dataDir)
	if len(path) > 100 {
		return nil, fmt.Errorf("socket %s: path is too long for a Unix socket; use a shorter data folder", path)
	}
	if old, err := os.Lstat(path); err == nil {
		if old.Mode()&os.ModeSocket == 0 || !ownedByMe(old) {
			return nil, fmt.Errorf("%s exists and is not a curio socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove old socket: %w", err)
		}
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

// ResolveDataDir makes the data folder absolute, resolves its symlinks once,
// checks the resolved path and returns it. It refuses a folder that is not
// inside the home folder, or when a folder from the data folder up to the
// home folder belongs to another user, lets group or other users write, or
// has an ACL entry that allows access. Then no other user can replace the
// database, the lock or the socket.
func ResolveDataDir(dataDir string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("data folder: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("data folder: %w", err)
	}
	home, err := realHome()
	if err != nil {
		return "", err
	}
	if resolved == home || !strings.HasPrefix(resolved, home+string(filepath.Separator)) {
		return "", fmt.Errorf("data folder %s must be inside your home folder %s", resolved, home)
	}
	for d := resolved; ; d = filepath.Dir(d) {
		if err := checkFolder(d); err != nil {
			return "", err
		}
		if d == home {
			return resolved, nil
		}
	}
}

func realHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home folder: %w", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("home folder: %w", err)
	}
	return home, nil
}

// checkFolder refuses a folder that another user could change.
func checkFolder(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("folder %s is not safe: it is not your own folder closed to writes by other users", dir)
	}
	allow, err := hasAllowACL(dir)
	if err != nil {
		return err
	}
	if allow {
		return fmt.Errorf("folder %s is not safe: it has an ACL entry that allows access", dir)
	}
	return nil
}

// hasAllowACL reports an ACL entry that allows access. On macOS "ls -led"
// lists entries as "N: subject allow|deny rights"; deny entries (such as the
// home folder's "everyone deny delete") take nothing away from the owner and
// are fine. Elsewhere a "+" after the mode means a POSIX ACL.
func hasAllowACL(dir string) (bool, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/bin/ls", "-led", dir).Output() //nolint:gosec // fixed binary, path argument
		if err != nil {
			return false, fmt.Errorf("read ACL of %s: %w", dir, err)
		}
		for _, line := range strings.Split(string(out), "\n")[1:] {
			if strings.Contains(line, " allow ") {
				return true, nil
			}
		}
		return false, nil
	}
	out, err := exec.Command("ls", "-ld", dir).Output() //nolint:gosec // path argument
	if err != nil {
		return false, fmt.Errorf("read ACL of %s: %w", dir, err)
	}
	mode := strings.Fields(string(out))
	return len(mode) > 0 && len(mode[0]) > 10 && mode[0][10] == '+', nil
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
