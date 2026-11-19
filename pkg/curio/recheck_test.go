package curio

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/newscope/pkg/hn"
)

// Re-check 1, finding 1: the token never lands in a file that others can read.
func TestEnsureAPIToken_NewPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "curio.yml")
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	// a reader that opened the file before keeps its handle
	before, err := os.Open(path)
	require.NoError(t, err)
	defer before.Close()
	infoBefore, err := before.Stat()
	require.NoError(t, err)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	_, err = EnsureAPIToken(path, &cfg)
	require.NoError(t, err)

	seen, err := io.ReadAll(before)
	require.NoError(t, err)
	assert.NotContains(t, string(seen), cfg.APIToken, "the old file never gets the token")
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(infoBefore, infoAfter), "the config is a new file")
	assert.Equal(t, os.FileMode(0o600), infoAfter.Mode().Perm())
	left, err := filepath.Glob(filepath.Join(dir, ".curio-*"))
	require.NoError(t, err)
	assert.Empty(t, left, "no temp file stays")
}

// Re-check 1, finding 10: an empty api_token entry is replaced, not repeated.
func TestEnsureAPIToken_ReplacesEmptyEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curio.yml")
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte("api_token: \"\"\n")...), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	_, err = EnsureAPIToken(path, &cfg)
	require.NoError(t, err)
	again, err := LoadConfig(path)
	require.NoError(t, err, "the file has one api_token entry")
	assert.Equal(t, cfg.APIToken, again.APIToken)
}

// privateTempDir is a folder whose ancestors other users cannot change: a
// folder under the home folder (not /tmp, which every user can write).
func privateTempDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	dir, err := os.MkdirTemp(home, ".curio-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Re-check 1, finding 2: curio serves on a socket in a folder only the owner can open.
func TestListenSocket(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "run", "curio.sock")

	ln, err := ListenSocket(path)
	require.NoError(t, err)
	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "a new folder is private")
	sock, err := os.Lstat(path)
	require.NoError(t, err)
	assert.NotZero(t, sock.Mode()&os.ModeSocket)
	assert.Equal(t, os.FileMode(0o600), sock.Mode().Perm())

	_, err = ListenSocket(path)
	require.Error(t, err, "a second curio does not take over a live socket")
	assert.Contains(t, err.Error(), "already")

	require.NoError(t, ln.Close())
	ln2, err := ListenSocket(path) // a stale socket file is replaced
	require.NoError(t, err)
	_ = ln2.Close()

	_, err = ListenSocket("relative/curio.sock")
	require.Error(t, err)
}

// Re-check 1, finding 3: the fetcher never connects to an address of this Mac.
func TestPageFetcher_RefusesOwnAddress(t *testing.T) {
	f := NewPageFetcher(time.Second, 1024)
	own := netip.MustParseAddr("2606:4700::1111")
	f.LocalAddrs = func() ([]netip.Addr, error) { return []netip.Addr{own}, nil }
	assert.False(t, f.permitted(netip.AddrPortFrom(own, 443)))
	assert.True(t, f.permitted(netip.AddrPortFrom(netip.MustParseAddr("2606:4700::1001"), 443)))
}

// Re-check 1, finding 11: the fetcher tries the next public address.
func TestPageFetcher_TriesNextAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()
	port := portOf(t, srv)
	down := netip.MustParseAddr("192.0.2.10")
	f := NewPageFetcher(5*time.Second, 1<<20)
	f.Allow = func(netip.AddrPort) bool { return true }
	f.LocalAddrs = func() ([]netip.Addr, error) { return nil, nil }
	f.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{down, netip.MustParseAddr("127.0.0.1")}, nil
	}
	f.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, down.String()) {
			return nil, errors.New("connection refused")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	text, err := f.Text(context.Background(), "http://news.example:"+strconv.Itoa(int(port))+"/a")
	require.NoError(t, err)
	assert.Contains(t, text, "faster compiler")
}

// Re-check 1, finding 4: two processes cannot both use the last allowance.
func TestStore_ReserveIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curio.db")
	a, err := OpenStore(context.Background(), path)
	require.NoError(t, err)
	defer a.Close()
	b, err := OpenStore(context.Background(), path)
	require.NoError(t, err)
	defer b.Close()

	var calls, stories atomic.Int64
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := a
			if i%2 == 1 {
				st = b
			}
			got, err := st.Reserve(context.Background(), "2026-11-19", 5, 12, 3)
			if err != nil {
				t.Error(err)
				return
			}
			if got > 0 {
				calls.Add(1)
				stories.Add(int64(got))
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, calls.Load(), int64(5))
	assert.Equal(t, int64(12), stories.Load(), "12 stories in 4 calls: 3+3+3+3")
	c, s, err := a.Usage(context.Background(), "2026-11-19")
	require.NoError(t, err)
	assert.Equal(t, int(calls.Load()), c)
	assert.Equal(t, 12, s)
}

type clockScorer struct {
	advance func()
	batches int
}

func (c *clockScorer) Score(_ context.Context, in ScoreInput) ([]Scored, error) {
	c.batches++
	c.advance()
	out := make([]Scored, 0, len(in.Stories))
	for _, st := range in.Stories {
		out = append(out, Scored{ID: st.ID, Summary: "s"})
	}
	return out, nil
}

// Re-check 1, finding 5: a batch after midnight counts on the new day.
func TestService_LimitsUseTheDayOfEachBatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 11, 19, 23, 59, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.Extract = false
	cfg.Claude.BatchSize = 2
	cfg.Limits.DailyCalls = 5
	sc := &clockScorer{advance: func() { now = now.Add(2 * time.Minute) }}
	svc := &Service{Cfg: cfg, Store: st, HN: fakeFront{stories: manyStories(4)}, Scorer: sc, Now: func() time.Time { return now }}
	require.NoError(t, svc.Refresh(ctx))
	require.Equal(t, 2, sc.batches)
	c19, _, err := st.Usage(ctx, "2026-11-19")
	require.NoError(t, err)
	c20, _, err := st.Usage(ctx, "2026-11-20")
	require.NoError(t, err)
	assert.Equal(t, 1, c19)
	assert.Equal(t, 1, c20)
}

type countingFront struct {
	calls   int
	stories []hn.Story
}

func (c *countingFront) TopStories(context.Context, int) ([]hn.Story, error) {
	c.calls++
	return c.stories, nil
}

// Re-check 1, finding 7: every refresh start keeps the minimum interval.
func TestService_RefreshKeepsMinInterval(t *testing.T) {
	now := t0
	cfg := testConfig()
	cfg.Extract = false
	front := &countingFront{}
	svc := &Service{Cfg: cfg, Store: newTestStore(t), HN: front, Scorer: &fakeScorer{}, Now: func() time.Time { return now }}
	require.NoError(t, svc.Refresh(context.Background()))
	require.NoError(t, svc.Refresh(context.Background()))
	assert.Equal(t, 1, front.calls, "a scheduled tick right after a refresh waits")
	now = now.Add(cfg.Limits.MinRefresh)
	require.NoError(t, svc.Refresh(context.Background()))
	assert.Equal(t, 2, front.calls)
}

// Re-check 1, finding 6: the CLI gets only the variables a subscription login needs.
func TestSubscriptionEnv_Allowlist(t *testing.T) {
	env, err := subscriptionEnv([]string{
		"PATH=/bin", "HOME=/h", "USER=me", "TMPDIR=/t", "LANG=en_US.UTF-8",
		"AWS_PROFILE=prod", "AWS_ACCESS_KEY_ID=x", "ANTHROPIC_MODEL=opus", "GOOGLE_APPLICATION_CREDENTIALS=/k",
		"CLAUDE_CODE_USE_MANTLE=0",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"PATH=/bin", "HOME=/h", "USER=me", "TMPDIR=/t", "LANG=en_US.UTF-8"}, env)

	for _, kv := range []string{"CLAUDE_CODE_USE_MANTLE=1", "CLAUDE_CODE_USE_ANTHROPIC_AWS=1", "CLAUDE_CODE_USE_VERTEX=yes"} {
		_, err := subscriptionEnv([]string{"PATH=/bin", kv})
		require.Error(t, err, kv)
	}
}
