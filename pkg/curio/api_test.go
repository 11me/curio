package curio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAPI(t *testing.T) (*Service, http.Handler) {
	t.Helper()
	ctx := context.Background()
	st := newTestStore(t)
	cfg := testConfig()
	cfg.Cutoff = 0.5
	svc := &Service{Cfg: cfg, Store: st, Now: func() time.Time { return t0.Add(time.Hour) }}
	_, err := st.UpsertFront(ctx, front(1, 2, 3), t0)
	require.NoError(t, err)
	// story 1: high fit, story 2: low fit, story 3: unscored
	require.NoError(t, st.SaveScore(ctx, 1, "Rust ships.", []Answer{{"picks", 1, "like your Rust picks"}, {"release", 1, "a release"}}, t0))
	require.NoError(t, st.SaveScore(ctx, 2, "Drama.", []Answer{{"drama", 1, "drama"}}, t0))
	return svc, (&API{Svc: svc}).Handler()
}

func do(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8090"+target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func ids(out map[string]any) []float64 {
	res := []float64{}
	for _, s := range out["stories"].([]any) {
		res = append(res, s.(map[string]any)["id"].(float64))
	}
	return res
}

func TestAPI_List(t *testing.T) {
	_, h := newTestAPI(t)

	rec, out := do(t, h, http.MethodGet, "/api/v1/stories", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []float64{1, 2, 3}, ids(out), "sorted by fit, unscored last")
	assert.InDelta(t, 3, out["total"], 0)
	assert.InDelta(t, 1, out["unscored"], 0)
	assert.InDelta(t, 0.5, out["cutoff"], 0)
	first := out["stories"].([]any)[0].(map[string]any)
	assert.InDelta(t, 0.881, first["fit"], 0.001)
	assert.Equal(t, "https://news.ycombinator.com/item?id=1", first["discussion_url"])
	assert.Nil(t, out["stories"].([]any)[2].(map[string]any)["fit"])

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?min_fit=0.5", "", nil)
	assert.Equal(t, []float64{1}, ids(out))
	assert.InDelta(t, 1, out["listed"], 0)

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?q=drama", "", nil)
	assert.Equal(t, []float64{2}, ids(out), "search looks in the summary")

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?sort=points", "", nil)
	assert.Equal(t, []float64{3, 2, 1}, ids(out))

	for _, bad := range []string{"min_fit=2", "sort=x", "marks=x", "limit=0"} {
		rec, _ := do(t, h, http.MethodGet, "/api/v1/stories?"+bad, "", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
}

func TestAPI_DetailAndMark(t *testing.T) {
	_, h := newTestAPI(t)

	rec, out := do(t, h, http.MethodGet, "/api/v1/stories/1", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Rust ships.", out["summary"])
	reasons := out["reasons"].([]any)
	require.Len(t, reasons, 2)
	assert.Equal(t, "Like your picks", reasons[0].(map[string]any)["label"])
	assert.Equal(t, "like your Rust picks", reasons[0].(map[string]any)["note"])
	assert.Len(t, out["criteria"].([]any), 3)

	rec, out = do(t, h, http.MethodPost, "/api/v1/stories/1/mark", `{"mark":"good"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "good", out["mark"])
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?marks=hide", "", nil)
	assert.Equal(t, []float64{2, 3}, ids(out))
	assert.InDelta(t, 1, out["good"], 0)
	_, out = do(t, h, http.MethodPost, "/api/v1/stories/1/mark", `{"mark":"none"}`, nil)
	assert.Empty(t, out["mark"])

	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/1/mark", `{"mark":"spam"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/77/mark", `{"mark":"bad"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec, _ = do(t, h, http.MethodGet, "/api/v1/stories/abc", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec, out = do(t, h, http.MethodPost, "/api/v1/stories/2/task", `{"key":"PER-80"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "PER-80", out["task_key"])
	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/2/task", `{"key":"<b>"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_Guard(t *testing.T) {
	_, h := newTestAPI(t)
	rec, _ := do(t, h, http.MethodGet, "/api/v1/stories", "", map[string]string{"Origin": "https://evil.example"})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	req := httptest.NewRequest(http.MethodGet, "http://evil.example:8090/api/v1/stories", http.NoBody)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "DNS rebinding host")

	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/1/mark", `{"mark":"good"}`, map[string]string{"Content-Type": "text/plain"})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)

	rec, out := do(t, h, http.MethodPost, "/api/v1/refresh", `{}`, nil)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, out, "refreshing")
}
