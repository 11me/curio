package curio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeScorer_Score(t *testing.T) {
	var gotArgs []string
	var gotPrompt map[string]any
	run := func(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
		assert.Equal(t, "claude", name)
		gotArgs = args
		require.NoError(t, json.Unmarshal(stdin, &gotPrompt))
		return []byte(`{"type":"result","is_error":false,"result":"","structured_output":{"stories":[
			{"id":1,"summary":"  Rust ships 1.100. ","answers":[
				{"criterion":"picks","answer":1.7,"note":"close to\nyour picks"},
				{"criterion":"unknown","answer":1,"note":"x"}]},
			{"id":99,"summary":"not in batch","answers":[]}]}}`), nil
	}
	sc := &ClaudeScorer{Cfg: DefaultConfig().Claude, Criteria: testCriteria, Run: run, Environ: func() []string { return nil }}
	sc.Cfg.TextChars = 5
	res, err := sc.Score(context.Background(), ScoreInput{
		Stories:   []StoryRow{{ID: 1, Title: "Rust 1.100", Domain: "blog.rust-lang.org", ArticleText: "long article"}, {ID: 2, Title: "Two", HNText: "hn text"}},
		PastPicks: []string{"Zig 0.15"},
	})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, int64(1), res[0].ID)
	assert.Equal(t, "Rust ships 1.100.", res[0].Summary)
	require.Len(t, res[0].Answers, 1)
	assert.InDelta(t, 1.0, res[0].Answers[0].Answer, 0.0001)
	assert.Equal(t, "close to your picks", res[0].Answers[0].Note)

	args := strings.Join(gotArgs, " ")
	assert.Contains(t, args, "--model haiku")
	assert.Contains(t, args, "--tools  --strict-mcp-config")
	assert.Contains(t, args, "--no-session-persistence")
	assert.Contains(t, args, "--max-turns 3")
	assert.Contains(t, args, "--max-budget-usd 0.25")
	assert.Contains(t, args, `"maxLength":800`, "the schema limits the summary")
	assert.Contains(t, args, `"maxItems":30`, "the schema limits the story count")
	stories := gotPrompt["stories"].([]any)
	assert.Equal(t, "long …", stories[0].(map[string]any)["text"], "text is cut to text_chars")
	assert.Equal(t, "hn te…", stories[1].(map[string]any)["text"], "HN text is used without article text")
	assert.Equal(t, []any{"Zig 0.15"}, gotPrompt["past_picks"])
	assert.Equal(t, []any{}, gotPrompt["past_rejects"])
}

func TestClaudeScorer_Errors(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{name: "run fails", err: errors.New("exit 1"), want: "claude call"},
		{name: "not json", out: "hello", want: "decode claude output"},
		{name: "cli error", out: `{"is_error":true,"subtype":"error_max_turns","result":"limit"}`, want: "error_max_turns"},
		{name: "no scores", out: `{"is_error":false,"structured_output":{"stories":[]}}`, want: "no scores"},
		{name: "result text fallback", out: `{"is_error":false,"result":"{\"stories\":[{\"id\":1,\"summary\":\"s\",\"answers\":[]}]}"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &ClaudeScorer{Cfg: DefaultConfig().Claude, Criteria: testCriteria, Environ: func() []string { return nil },
				Run: func(context.Context, string, []string, []byte) ([]byte, error) { return []byte(tt.out), tt.err }}
			res, err := sc.Score(context.Background(), ScoreInput{Stories: []StoryRow{{ID: 1, Title: "t"}}})
			if tt.want == "" {
				require.NoError(t, err)
				require.Len(t, res, 1)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestSubscriptionEnv(t *testing.T) {
	env, err := subscriptionEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=y", "ANTHROPIC_BASE_URL=http://proxy", "HOME=/h", "CLAUDE_CODE_USE_BEDROCK=0"})
	require.NoError(t, err)
	assert.Equal(t, []string{"PATH=/bin", "HOME=/h", "CLAUDE_CODE_USE_BEDROCK=0"}, env)

	for _, kv := range []string{"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=true", "CLAUDE_CODE_USE_FOUNDRY=1"} {
		_, err := subscriptionEnv([]string{"PATH=/bin", kv})
		require.Error(t, err, kv)
		assert.Contains(t, err.Error(), "cloud provider")
	}
}

func TestClaudeScorer_RefusesCloudProvider(t *testing.T) {
	called := false
	sc := &ClaudeScorer{Cfg: DefaultConfig().Claude, Criteria: testCriteria,
		Environ: func() []string { return []string{"CLAUDE_CODE_USE_BEDROCK=1"} },
		Run:     func(context.Context, string, []string, []byte) ([]byte, error) { called = true; return nil, nil }}
	_, err := sc.Score(context.Background(), ScoreInput{Stories: []StoryRow{{ID: 1, Title: "t"}}})
	require.Error(t, err)
	assert.False(t, called, "no call is made")
}
