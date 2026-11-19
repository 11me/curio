package curio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-check 5: the data folder is resolved once; a symlink changed after the
// check cannot move the lock, database or socket somewhere else.
func TestRunLock_UsesResolvedDataFolder(t *testing.T) {
	base := privateTempDir(t)
	realA := filepath.Join(base, "a")
	realB := filepath.Join(base, "b")
	require.NoError(t, os.Mkdir(realA, 0o700))
	require.NoError(t, os.Mkdir(realB, 0o700))
	link := filepath.Join(base, "data")
	require.NoError(t, os.Symlink(realA, link))

	lock, err := AcquireRunLock(link)
	require.NoError(t, err)
	defer lock.Close()
	resolvedA, err := filepath.EvalSymlinks(realA)
	require.NoError(t, err)
	assert.Equal(t, resolvedA, lock.Dir(), "callers get the resolved folder")

	// the link now points elsewhere
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(realB, link))

	ln, err := lock.Listen()
	require.NoError(t, err)
	defer ln.Close()
	_, err = os.Lstat(filepath.Join(resolvedA, "sock", "curio.sock"))
	require.NoError(t, err, "the socket is in the checked folder")
	_, err = os.Lstat(filepath.Join(realB, "sock"))
	assert.True(t, os.IsNotExist(err), "nothing is made in the new link target")
	_, err = os.Lstat(filepath.Join(realB, "curio.lock"))
	assert.True(t, os.IsNotExist(err))
}
