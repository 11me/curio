package curio

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-check 3: one fixed socket per data folder, in a folder curio makes.
func TestRunLock_SocketPerDataFolder(t *testing.T) {
	base := privateTempDir(t)
	dirA := filepath.Join(base, "a", "b", "data") // two missing parents
	dirB := filepath.Join(base, "other")
	for _, d := range []string{dirA, dirB} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
	lockA, err := AcquireRunLock(dirA)
	require.NoError(t, err)
	defer lockA.Close()
	lockB, err := AcquireRunLock(dirB)
	require.NoError(t, err)
	defer lockB.Close()

	lnA, err := lockA.Listen()
	require.NoError(t, err)
	defer lnA.Close()
	lnB, err := lockB.Listen()
	require.NoError(t, err)
	defer lnB.Close()
	assert.Equal(t, filepath.Join(dirA, "sock", "curio.sock"), SocketPath(dirA))
	assert.NotEqual(t, SocketPath(dirA), SocketPath(dirB), "two data folders never share a socket")

	info, err := os.Stat(filepath.Dir(SocketPath(dirA)))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	sock, err := os.Lstat(SocketPath(dirA))
	require.NoError(t, err)
	assert.NotZero(t, sock.Mode()&os.ModeSocket)
	assert.Equal(t, os.FileMode(0o600), sock.Mode().Perm())
}

// Re-check 3: a stale socket is replaced while the data folder lock is held.
func TestRunLock_ReplacesStaleSocket(t *testing.T) {
	dir := filepath.Join(privateTempDir(t), "data")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sock"), 0o700))
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: SocketPath(dir), Net: "unix"})
	require.NoError(t, err)
	stale.SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())

	lock, err := AcquireRunLock(dir)
	require.NoError(t, err)
	defer lock.Close()
	ln, err := lock.Listen()
	require.NoError(t, err)
	_ = ln.Close()
}

// Re-check 3: curio refuses a socket folder it would have to trust blindly.
func TestRunLock_RefusesUnsafeFolders(t *testing.T) {
	t.Run("outside home", func(t *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "cd")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		lock, err := AcquireRunLock(dir)
		require.NoError(t, err)
		defer lock.Close()
		_, err = lock.Listen()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "home folder")
	})
	t.Run("existing open socket folder keeps its mode", func(t *testing.T) {
		dir := filepath.Join(privateTempDir(t), "data")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sock"), 0o700))
		require.NoError(t, os.Chmod(filepath.Join(dir, "sock"), 0o755))
		lock, err := AcquireRunLock(dir)
		require.NoError(t, err)
		defer lock.Close()
		_, err = lock.Listen()
		require.Error(t, err)
		info, err := os.Stat(filepath.Join(dir, "sock"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})
	t.Run("data folder others can write", func(t *testing.T) {
		dir := filepath.Join(privateTempDir(t), "data")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.Chmod(dir, 0o777))
		lock, err := AcquireRunLock(dir)
		require.NoError(t, err)
		defer lock.Close()
		_, err = lock.Listen()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "other users")
	})
	t.Run("socket folder with an allow ACL", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("macOS ACLs")
		}
		dir := filepath.Join(privateTempDir(t), "data")
		sock := filepath.Join(dir, "sock")
		require.NoError(t, os.MkdirAll(sock, 0o700))
		require.NoError(t, exec.Command("/bin/chmod", "+a", "everyone allow add_file,delete_child", sock).Run())
		lock, err := AcquireRunLock(dir)
		require.NoError(t, err)
		defer lock.Close()
		_, err = lock.Listen()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ACL")
	})
}

// Re-check 3: token setup refuses a config with several YAML documents.
func TestEnsureAPIToken_RefusesSeveralDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curio.yml")
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	body := string(data) + "---\nmarker: keep me\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	_, err = EnsureAPIToken(path, &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one YAML document")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body, string(after), "the file is unchanged")
}
