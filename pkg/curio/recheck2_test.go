package curio

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	dir := privateTempDir(t)
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
