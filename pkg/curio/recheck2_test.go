package curio

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-check 2, finding 1: the socket folder and every folder above it must be
// safe from other users, or someone could swap the path to the socket.
func TestListenSocket_RefusesSharedAncestor(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "cs") // /tmp is writable by every user
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	require.NoError(t, os.Chmod(base, 0o700))
	_, err = ListenSocket(filepath.Join(base, "curio.sock"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "other users")
}

// Re-check 2, finding 4: curio never changes the mode of a folder it did not make.
func TestListenSocket_KeepsExistingFolderMode(t *testing.T) {
	shared := filepath.Join(privateTempDir(t), "project")
	require.NoError(t, os.Mkdir(shared, 0o755))
	require.NoError(t, os.Chmod(shared, 0o755))
	_, err := ListenSocket(filepath.Join(shared, "curio.sock"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0700")
	info, err := os.Stat(shared)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the folder mode is unchanged")
}

// Re-check 2, finding 5: a socket that another program still serves stays.
func TestListenSocket_KeepsLiveDatagramSocket(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "run", "curio.sock")
	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o700))
	other, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	require.NoError(t, err)
	defer other.Close()
	_, err = ListenSocket(path)
	require.Error(t, err)
	_, err = os.Lstat(path)
	require.NoError(t, err, "the other program's socket is still there")
}

// Re-check 2, finding 2: every empty form of api_token stays valid YAML.
func TestEnsureAPIToken_EmptyForms(t *testing.T) {
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	for name, extra := range map[string]string{
		"crlf quoted": "api_token: \"\"\r\n",
		"null":        "api_token: null\n",
		"tilde":       "api_token: ~\n",
		"bare":        "api_token:\n",
		"crlf file":   "",
	} {
		t.Run(name, func(t *testing.T) {
			body := string(data) + extra
			if name == "crlf file" {
				body = strings.ReplaceAll(string(data), "\n", "\r\n") + "api_token: ''\r\n"
			}
			path := filepath.Join(t.TempDir(), "curio.yml")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			cfg, err := LoadConfig(path)
			require.NoError(t, err)
			_, err = EnsureAPIToken(path, &cfg)
			require.NoError(t, err)
			again, err := LoadConfig(path)
			require.NoError(t, err)
			assert.Equal(t, cfg.APIToken, again.APIToken)
			assert.Len(t, again.Criteria, 9, "the rest of the config is kept")
		})
	}
}

// Re-check 2, finding 3: two first starts end with one token.
func TestEnsureAPIToken_TwoFirstStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curio.yml")
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	a, err := LoadConfig(path)
	require.NoError(t, err)
	b, err := LoadConfig(path)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for _, cfg := range []*Config{&a, &b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := EnsureAPIToken(path, cfg)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	again, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, again.APIToken, a.APIToken)
	assert.Equal(t, again.APIToken, b.APIToken, "the second start uses the saved token")
}

// Re-check 2, finding 6: only one curio process runs and scores at a time.
func TestRunLock(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireRunLock(dir)
	require.NoError(t, err)
	_, err = AcquireRunLock(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another curio")
	require.NoError(t, first.Close())
	second, err := AcquireRunLock(dir)
	require.NoError(t, err)
	_ = second.Close()
}

// Re-check 2, finding 7: unrelated CLAUDE_CODE_USE_* switches do not stop curio.
func TestSubscriptionEnv_UnrelatedSwitch(t *testing.T) {
	env, err := subscriptionEnv([]string{"PATH=/bin", "CLAUDE_CODE_USE_POWERSHELL_TOOL=1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"PATH=/bin"}, env)
}
