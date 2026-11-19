package curio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/newscope/pkg/hn"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Criteria = testCriteria
	cfg.Bias = -1
	cfg.Claude.BatchSize = 2
	return cfg
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "curio.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func front(ids ...int64) []hn.Story {
	out := make([]hn.Story, 0, len(ids))
	for i, id := range ids {
		out = append(out, hn.Story{ID: id, Title: "Story " + string(rune('A'+i)), URL: "https://www.example.com/" + string(rune('a'+i)),
			Points: 10 * int(id), Comments: int(id), Posted: t0.Add(-time.Duration(id) * time.Hour), Rank: i + 1})
	}
	return out
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "curio.yml")
	data, err := os.ReadFile("../../curio.example.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8090", cfg.Listen)
	assert.Equal(t, filepath.Join(dir, "var/curio"), cfg.DataDir)
	assert.Equal(t, "haiku", cfg.Claude.Model)
	assert.Len(t, cfg.Criteria, 9)
	assert.Equal(t, 30*time.Minute, cfg.Interval)
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{name: "ok", change: func(*Config) {}},
		{name: "public listen", change: func(c *Config) { c.Listen = "0.0.0.0:8090" }, want: "loopback"},
		{name: "empty host", change: func(c *Config) { c.Listen = ":8090" }, want: "loopback"},
		{name: "localhost", change: func(c *Config) { c.Listen = "localhost:8090" }},
		{name: "ipv6 loopback", change: func(c *Config) { c.Listen = "[::1]:8090" }},
		{name: "short interval", change: func(c *Config) { c.Interval = time.Second }, want: "interval"},
		{name: "bad cutoff", change: func(c *Config) { c.Cutoff = 2 }, want: "cutoff"},
		{name: "no criteria", change: func(c *Config) { c.Criteria = nil }, want: "criteria"},
		{name: "bad id", change: func(c *Config) { c.Criteria = []Criterion{{ID: "Bad Id", Label: "l", Question: "q"}} }, want: "id"},
		{name: "dup id", change: func(c *Config) { c.Criteria = append(c.Criteria, c.Criteria[0]) }, want: "twice"},
		{name: "batch size", change: func(c *Config) { c.Claude.BatchSize = 0 }, want: "batch_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.change(&cfg)
			err := cfg.Validate()
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestStore_UpsertAndMarks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	added, err := st.UpsertFront(ctx, front(1, 2), t0)
	require.NoError(t, err)
	assert.Equal(t, 2, added)

	next := front(2, 3)
	next[0].Points = 999
	added, err = st.UpsertFront(ctx, next, t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, added)

	one, err := st.Get(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, one.Rank, "a story that left the front page has rank 0")
	assert.Equal(t, "example.com", one.Domain)
	two, err := st.Get(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 999, two.Points)
	assert.Equal(t, 1, two.Rank)
	assert.True(t, two.FirstSeen.Equal(t0), "first_seen stays")

	require.NoError(t, st.SetMark(ctx, 2, MarkGood, t0))
	require.NoError(t, st.SetMark(ctx, 3, MarkBad, t0))
	require.Error(t, st.SetMark(ctx, 3, "spam", t0))
	require.ErrorIs(t, st.SetMark(ctx, 42, MarkGood, t0), ErrNotFound)
	good, err := st.RecentMarked(ctx, MarkGood, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"Story A"}, good)

	require.NoError(t, st.SetTask(ctx, 1, "PER-80"))
	_, err = st.Get(ctx, 42)
	require.ErrorIs(t, err, ErrNotFound)

	// story 1 has a task, 2 and 3 are marked: nothing is old and unmarked
	n, err := st.Cleanup(ctx, t0.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n)
	require.NoError(t, st.SetMark(ctx, 3, MarkNone, t0))
	n, err = st.Cleanup(ctx, t0.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

type fakeFront struct {
	stories []hn.Story
	err     error
}

func (f fakeFront) TopStories(context.Context, int) ([]hn.Story, error) { return f.stories, f.err }

type fakeExtractor struct{ calls []string }

func (f *fakeExtractor) Text(_ context.Context, url string) (string, error) {
	f.calls = append(f.calls, url)
	if url == "https://www.example.com/b" {
		return "", errors.New("blocked")
	}
	return "article text for " + url, nil
}

type fakeScorer struct {
	batches []ScoreInput
	fail    map[int64]bool
}

func (f *fakeScorer) Score(_ context.Context, in ScoreInput) ([]Scored, error) {
	f.batches = append(f.batches, in)
	out := []Scored{}
	for _, st := range in.Stories {
		if f.fail[st.ID] {
			return nil, errors.New("model down")
		}
		out = append(out, Scored{ID: st.ID, Summary: "sum " + st.Title,
			Answers: []Answer{{Criterion: "picks", Answer: 0.9, Note: "close to picks"}, {Criterion: "release", Answer: 0.2}}})
	}
	return out, nil
}

func TestService_Refresh(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	x := &fakeExtractor{}
	sc := &fakeScorer{fail: map[int64]bool{3: true}}
	stories := front(1, 2, 3)
	stories = append(stories, hn.Story{ID: 4, Title: "Ask HN: x", URL: hn.DiscussionURL(4), Text: "question", Posted: t0, Rank: 4})
	svc := &Service{Cfg: testConfig(), Store: st, HN: fakeFront{stories: stories}, Extractor: x, Scorer: sc,
		Now: func() time.Time { return t0 }}
	require.NoError(t, svc.Refresh(ctx))

	assert.Len(t, x.calls, 3, "text posts are not extracted")
	require.Len(t, sc.batches, 2, "batch size 2")
	assert.Equal(t, "article text for https://www.example.com/a", sc.batches[0].Stories[0].ArticleText)

	one, err := st.Get(ctx, 1)
	require.NoError(t, err)
	assert.True(t, one.ScoredAt.Valid)
	assert.Equal(t, "sum Story A", one.Summary)
	two, err := st.Get(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, "blocked", two.ExtractError)
	three, err := st.Get(ctx, 3)
	require.NoError(t, err)
	assert.False(t, three.ScoredAt.Valid)
	assert.Equal(t, 1, three.ScoreAttempts)
	assert.Contains(t, three.ScoreError, "model down")

	// a second refresh retries only the failed story and passes Good picks
	require.NoError(t, st.SetMark(ctx, 1, MarkGood, t0))
	sc.fail = nil
	require.NoError(t, svc.Refresh(ctx))
	require.Len(t, sc.batches, 3)
	assert.Equal(t, []string{"Story A"}, sc.batches[2].PastPicks)
	require.Len(t, sc.batches[2].Stories, 2)
	assert.Equal(t, "", svc.Status().LastError)
	assert.NotNil(t, svc.Status().LastRefresh)
}

func TestService_RefreshFrontError(t *testing.T) {
	svc := &Service{Cfg: testConfig(), Store: newTestStore(t), HN: fakeFront{err: errors.New("offline")}, Scorer: &fakeScorer{}}
	require.Error(t, svc.Refresh(context.Background()))
	assert.Contains(t, svc.Status().LastError, "offline")
}
