package curio

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-check 4, finding 1: a relative data_dir becomes an absolute path.
func TestLoadConfig_RelativeDataDirIsAbsolute(t *testing.T) {
	dir := privateTempDir(t)
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	body := string(data) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "curio.yml"), []byte(body), 0o600))
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("curio.yml", []byte(replaceDataDir(body, "./var/curio")), 0o600))

	cfg, err := LoadConfig("curio.yml")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(cfg.DataDir), cfg.DataDir)
	assert.Equal(t, filepath.Join(dir, "var", "curio"), cfg.DataDir)

	require.NoError(t, os.MkdirAll(cfg.DataDir, 0o700))
	lock, err := AcquireRunLock(cfg.DataDir)
	require.NoError(t, err, "a relative data folder inside home starts")
	defer lock.Close()
	ln, err := lock.Listen()
	require.NoError(t, err)
	_ = ln.Close()
}

func replaceDataDir(body, value string) string {
	return regexp.MustCompile(`(?m)^data_dir:.*$`).ReplaceAllString(body, "data_dir: "+value)
}

// Re-check 4, finding 2: the data folder is checked before curio opens any
// file in it, in every mode; the run lock is the first file every run opens.
func TestAcquireRunLock_ChecksDataFolderFirst(t *testing.T) {
	t.Run("others can write", func(t *testing.T) {
		dir := filepath.Join(privateTempDir(t), "data")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.Chmod(dir, 0o777))
		_, err := AcquireRunLock(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "other users")
		_, statErr := os.Stat(filepath.Join(dir, "curio.lock"))
		assert.True(t, os.IsNotExist(statErr), "no file was made in the unsafe folder")
	})
	t.Run("outside home", func(t *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "cd")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		_, err = AcquireRunLock(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "home folder")
		_, statErr := os.Stat(filepath.Join(dir, "curio.lock"))
		assert.True(t, os.IsNotExist(statErr))
	})
}
