package curio

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_ReadState(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_, err := st.UpsertFront(ctx, front(1, 2, 3), t0)
	require.NoError(t, err)
	_, err = st.UpsertFront(ctx, front(1, 2, 3, 4), t0.Add(2*time.Hour))
	require.NoError(t, err)

	require.NoError(t, st.SetRead(ctx, 1, true, t0.Add(time.Minute)))
	require.NoError(t, st.SetRead(ctx, 1, true, t0.Add(time.Hour)), "a second read keeps the first time")
	one, err := st.Get(ctx, 1)
	require.NoError(t, err)
	require.True(t, one.ReadAt.Valid)
	assert.True(t, one.ReadAt.Time.Equal(t0.Add(time.Minute)))
	require.ErrorIs(t, st.SetRead(ctx, 42, true, t0), ErrNotFound)

	require.NoError(t, st.SetRead(ctx, 1, false, t0))
	one, err = st.Get(ctx, 1)
	require.NoError(t, err)
	assert.False(t, one.ReadAt.Valid, "unread clears the time")

	// a Good or Bad mark reads the story; clearing the mark keeps it read
	require.NoError(t, st.SetMark(ctx, 2, MarkGood, t0.Add(time.Minute)))
	require.NoError(t, st.SetMark(ctx, 2, MarkNone, t0.Add(time.Hour)))
	two, err := st.Get(ctx, 2)
	require.NoError(t, err)
	assert.True(t, two.ReadAt.Valid)
	assert.True(t, two.ReadAt.Time.Equal(t0.Add(time.Minute)))

	// mark read a set of stories: story 4 is not in the set and stays unread
	n, err := st.MarkRead(ctx, []int64{1, 2, 3}, t0.Add(3*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "stories 1 and 3; story 2 was read already")
	n, err = st.MarkRead(ctx, nil, t0)
	require.NoError(t, err)
	assert.Zero(t, n)
	two, err = st.Get(ctx, 2)
	require.NoError(t, err)
	assert.True(t, two.ReadAt.Time.Equal(t0.Add(time.Minute)), "mark all read keeps older read times")
	four, err := st.Get(ctx, 4)
	require.NoError(t, err)
	assert.False(t, four.ReadAt.Valid)

	// cleanup keeps read stories like marked ones and deletes only unread, unmarked ones
	n, err = st.Cleanup(ctx, t0.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	_, err = st.Get(ctx, 4)
	require.ErrorIs(t, err, ErrNotFound)
	for _, id := range []int64{1, 2, 3} {
		_, err = st.Get(ctx, id)
		require.NoError(t, err, "read story %d stays", id)
	}
}

func TestStore_Visit(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	last, err := st.Visit(ctx, "bb", t0)
	require.NoError(t, err)
	assert.Nil(t, last, "the first visit has no last visit")

	// a visit within the gap is the same visit: the last visit does not move
	last, err = st.Visit(ctx, "bb", t0.Add(10*time.Minute))
	require.NoError(t, err)
	assert.Nil(t, last)

	last, err = st.Visit(ctx, "bb", t0.Add(5*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.True(t, last.Equal(t0.Add(10*time.Minute)), "the last visit is the end of the previous visit, got %v", last)

	got, err := st.LastVisit(ctx, "bb")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Equal(t0.Add(10*time.Minute)))

	got, err = st.LastVisit(ctx, "other")
	require.NoError(t, err)
	assert.Nil(t, got, "each client has its own visits")
}

// TestOpenStore_AddsReadState opens a database from an older build: the
// stories stay and get the new column, and the visits table is added.
func TestOpenStore_AddsReadState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "curio.db")
	old, err := sqlx.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = old.ExecContext(ctx, `
		CREATE TABLE stories (
			id INTEGER PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL, domain TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '', points INTEGER NOT NULL DEFAULT 0, comments INTEGER NOT NULL DEFAULT 0,
			posted_at DATETIME NOT NULL, first_seen DATETIME NOT NULL, last_seen DATETIME NOT NULL,
			rank INTEGER NOT NULL DEFAULT 0, hn_text TEXT NOT NULL DEFAULT '', article_text TEXT NOT NULL DEFAULT '',
			extracted_at DATETIME, extract_error TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
			answers TEXT NOT NULL DEFAULT '[]', scored_at DATETIME, score_error TEXT NOT NULL DEFAULT '',
			score_attempts INTEGER NOT NULL DEFAULT 0, mark TEXT NOT NULL DEFAULT '', marked_at DATETIME,
			task_key TEXT NOT NULL DEFAULT '');
		INSERT INTO stories (id, title, url, posted_at, first_seen, last_seen, mark, marked_at)
		VALUES (7, 'Old story', 'https://example.com/7', ?, ?, ?, 'good', ?);
		INSERT INTO stories (id, title, url, posted_at, first_seen, last_seen)
		VALUES (8, 'Unmarked story', 'https://example.com/8', ?, ?, ?);`, t0, t0, t0, t0, t0, t0, t0)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	for i := range 2 { // the second open finds the columns in place and changes nothing
		st, err := OpenStore(ctx, path)
		require.NoError(t, err)
		row, err := st.Get(ctx, 7)
		require.NoError(t, err)
		assert.Equal(t, "Old story", row.Title)
		assert.Equal(t, MarkGood, row.Mark)
		require.True(t, row.ReadAt.Valid, "a story marked before read state counts as read")
		assert.True(t, row.ReadAt.Time.Equal(t0), "its read time is the mark time")
		assert.False(t, row.ScoreFailedAt.Valid)
		unmarked, err := st.Get(ctx, 8)
		require.NoError(t, err)
		if i == 0 {
			assert.False(t, unmarked.ReadAt.Valid, "an unmarked old story starts unread")
			require.NoError(t, st.SetRead(ctx, 8, true, t0.Add(time.Hour)))
		} else {
			assert.True(t, unmarked.ReadAt.Valid, "a read time stays after the next open")
		}
		_, err = st.Visit(ctx, "bb", t0)
		require.NoError(t, err)
		require.NoError(t, st.Close())
	}
}

func TestAPI_ReadState(t *testing.T) {
	svc, h := newTestAPI(t)

	rec, out := do(t, h, http.MethodGet, "/api/v1/stories", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.InDelta(t, 3, out["unread"], 0)
	assert.Equal(t, "2026-11-19T13:00:00Z", out["as_of"], "as_of is the server time of the list")
	assert.Nil(t, out["stories"].([]any)[0].(map[string]any)["read_at"])

	rec, out = do(t, h, http.MethodPost, "/api/v1/stories/1/read", `{"read":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2026-11-19T13:00:00Z", out["read_at"])
	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/42/read", `{"read":true}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/1/read", `{"read":"yes"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec, _ = do(t, h, http.MethodPost, "/api/v1/stories/1/read", `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "read is required")

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?read=unread", "", nil)
	assert.Equal(t, []float64{2, 3}, ids(out))
	assert.InDelta(t, 2, out["unread"], 0)
	// with as_of, a story read after that time stays in the unread list (it was unread when the list began)
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?read=unread&as_of=2026-11-19T12:30:00Z", "", nil)
	assert.Equal(t, []float64{1, 2, 3}, ids(out))
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?read=read", "", nil)
	assert.Equal(t, []float64{1}, ids(out))
	rec, _ = do(t, h, http.MethodGet, "/api/v1/stories?read=maybe", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec, _ = do(t, h, http.MethodGet, "/api/v1/stories?as_of=yesterday", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	_, out = do(t, h, http.MethodPost, "/api/v1/stories/1/read", `{"read":false}`, nil)
	assert.Nil(t, out["read_at"])

	// a Good mark reads the story
	_, out = do(t, h, http.MethodPost, "/api/v1/stories/2/mark", `{"mark":"good"}`, nil)
	assert.Equal(t, "2026-11-19T13:00:00Z", out["read_at"])

	// story 4 comes after the list time; story 5 is older than the 7 days of the list
	ctx := context.Background()
	_, err := svc.Store.UpsertFront(ctx, front(5), t0.AddDate(0, 0, -8))
	require.NoError(t, err)
	_, err = svc.Store.UpsertFront(ctx, front(1, 2, 3, 4), t0.Add(2*time.Hour))
	require.NoError(t, err)

	// the unread count follows min_fit, like the new count: stories 1, 3 and 4 are unread, only 1 is at 0.5 or above
	_, out = do(t, h, http.MethodGet, "/api/v1/stories", "", nil)
	assert.InDelta(t, 3, out["unread"], 0)
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?min_fit=0.5", "", nil)
	assert.InDelta(t, 1, out["unread"], 0)

	// mark all read reads only what the list can show: up to the time, in the 7 days, at or above min_fit
	rec, out = do(t, h, http.MethodPost, "/api/v1/read", `{"before":"2026-11-19T13:00:00Z","min_fit":0.5}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.InDelta(t, 1, out["marked"], 0, "story 1; story 3 is not scored")
	rec, out = do(t, h, http.MethodPost, "/api/v1/read", `{"before":"2026-11-19T13:00:00Z"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.InDelta(t, 1, out["marked"], 0, "story 3; story 4 came later and story 5 is too old")
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?read=unread", "", nil)
	assert.Equal(t, []float64{4}, ids(out))
	five, err := svc.Store.Get(ctx, 5)
	require.NoError(t, err)
	assert.False(t, five.ReadAt.Valid, "a story older than the list stays unread, so the cleanup can remove it")
	for _, body := range []string{`{"before":"2026-11-19T13:00:00Z","min_fit":2}`, `{"before":"2026-11-19T13:00:00Z","min_fit":-1}`} {
		rec, _ = do(t, h, http.MethodPost, "/api/v1/read", body, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
	rec, _ = do(t, h, http.MethodPost, "/api/v1/read", `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "before is required")
	rec, _ = do(t, h, http.MethodPost, "/api/v1/read", `{"before":"soon"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// nextCursor returns the next field of a list answer as a query value.
func nextCursor(t *testing.T, out map[string]any) string {
	t.Helper()
	next, ok := out["next"].(string)
	require.True(t, ok, "next is a string cursor, got %v", out["next"])
	return url.QueryEscape(next)
}

func TestAPI_Paging(t *testing.T) {
	_, h := newTestAPI(t)

	_, out := do(t, h, http.MethodGet, "/api/v1/stories?limit=2", "", nil)
	assert.Equal(t, []float64{1, 2}, ids(out))
	assert.InDelta(t, 3, out["listed"], 0, "listed counts every match, not only this page")

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=2&after="+nextCursor(t, out), "", nil)
	assert.Equal(t, []float64{3}, ids(out))
	assert.Nil(t, out["next"], "the last page has no cursor")

	// the cursor holds the sort values, not a position: a story that leaves the filter does not shift the next page
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=1&min_fit=0.5", "", nil)
	assert.Equal(t, []float64{1}, ids(out))
	assert.Nil(t, out["next"])
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=2&sort=date", "", nil)
	assert.Equal(t, []float64{1, 2}, ids(out), "date order is 1, 2, 3")
	dateNext := nextCursor(t, out)
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=2&sort=date&after="+dateNext, "", nil)
	assert.Equal(t, []float64{3}, ids(out))

	for _, bad := range []string{"x", "999", "date:0:1", "fit:x:0:1", "fit:0.5:0:0", "fit:0.5:x:1", "fit:NaN:0:1"} {
		rec, _ := do(t, h, http.MethodGet, "/api/v1/stories?after="+url.QueryEscape(bad), "", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	rec, _ := do(t, h, http.MethodGet, "/api/v1/stories?sort=points&after="+dateNext, "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a cursor of another sort")
}

// TestAPI_PagingKeepsTheBoundary changes the points of the last story of a
// page before the next page: the next page still starts after the old boundary.
func TestAPI_PagingKeepsTheBoundary(t *testing.T) {
	svc, h := newTestAPI(t)
	ctx := context.Background()

	// points are 10, 20 and 30, so the order is 3, 2, 1
	_, out := do(t, h, http.MethodGet, "/api/v1/stories?sort=points&limit=1", "", nil)
	require.Equal(t, []float64{3}, ids(out))
	after := nextCursor(t, out)

	// a refresh drops story 3 to 5 points, below every other story
	next := front(1, 2, 3)
	next[2].Points = 5
	_, err := svc.Store.UpsertFront(ctx, next, t0.Add(30*time.Minute))
	require.NoError(t, err)

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?sort=points&limit=1&after="+after, "", nil)
	assert.Equal(t, []float64{2}, ids(out), "story 2 is not skipped")
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?sort=points&limit=5&after="+nextCursor(t, out), "", nil)
	assert.Equal(t, []float64{1, 3}, ids(out), "story 3 comes again at its new place")

	// the same for a new fit: story 4 has fit 0.5, so the order is 1, 4, 2, 3
	_, err = svc.Store.UpsertFront(ctx, front(1, 2, 3, 4), t0.Add(time.Hour))
	require.NoError(t, err)
	require.NoError(t, svc.Store.SaveScore(ctx, 4, "A release.", []Answer{{"release", 1, "a release"}}, t0))
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=1", "", nil)
	require.Equal(t, []float64{1}, ids(out))
	after = nextCursor(t, out)
	// story 1 is scored again with no answers: fit 0.27, below story 4
	require.NoError(t, svc.Store.SaveScore(ctx, 1, "Rust ships.", []Answer{}, t0))
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=1", "", nil)
	require.Equal(t, []float64{4}, ids(out), "story 4 is first now")
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?limit=1&after="+after, "", nil)
	assert.Equal(t, []float64{4}, ids(out), "story 4 is not skipped")
}

func TestAPI_Visit(t *testing.T) {
	svc, h := newTestAPI(t)
	now := t0.Add(time.Hour)
	svc.Now = func() time.Time { return now }

	rec, out := do(t, h, http.MethodPost, "/api/v1/visit", `{"client":"bb"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, out["last_visit"])
	assert.Equal(t, "2026-11-19T13:00:00Z", out["visit_at"])
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?client=bb", "", nil)
	assert.InDelta(t, 0, out["new"], 0, "nothing is new on the first visit")

	// story 4 comes after the visit; the next visit is a day later
	_, err := svc.Store.UpsertFront(context.Background(), front(1, 2, 3, 4), t0.Add(2*time.Hour))
	require.NoError(t, err)
	now = t0.Add(26 * time.Hour)
	_, out = do(t, h, http.MethodPost, "/api/v1/visit", `{"client":"bb"}`, nil)
	assert.Equal(t, "2026-11-19T13:00:00Z", out["last_visit"])

	_, out = do(t, h, http.MethodGet, "/api/v1/stories?client=bb", "", nil)
	assert.InDelta(t, 1, out["new"], 0)
	assert.Equal(t, "2026-11-19T13:00:00Z", out["last_visit"])
	for _, s := range out["stories"].([]any) {
		item := s.(map[string]any)
		assert.Equal(t, item["id"] == float64(4), item["new"], "story %v", item["id"])
	}
	// the new count follows the cutoff: story 4 is not scored
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?client=bb&min_fit=0.5", "", nil)
	assert.InDelta(t, 0, out["new"], 0)
	// a read story is not new
	do(t, h, http.MethodPost, "/api/v1/stories/4/read", `{"read":true}`, nil)
	_, out = do(t, h, http.MethodGet, "/api/v1/stories?client=bb", "", nil)
	assert.InDelta(t, 0, out["new"], 0)

	_, out = do(t, h, http.MethodGet, "/api/v1/stories", "", nil)
	assert.Nil(t, out["last_visit"], "no client, no last visit")

	for _, body := range []string{`{}`, `{"client":"BB"}`, `{"client":"a b"}`, `{"client":"` + string(make([]byte, 40)) + `"}`} {
		rec, _ = do(t, h, http.MethodPost, "/api/v1/visit", body, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
	rec, _ = do(t, h, http.MethodGet, "/api/v1/stories?client=BB", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
