package curio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // pure Go SQLite driver

	"github.com/umputun/newscope/pkg/hn"
)

// Store keeps stories, scores and marks in SQLite.
type Store struct {
	db *sqlx.DB
}

// Mark values.
const (
	MarkNone = ""
	MarkGood = "good"
	MarkBad  = "bad"
)

// ErrNotFound is returned for an unknown story.
var ErrNotFound = errors.New("story not found")

const schema = `
CREATE TABLE IF NOT EXISTS stories (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	url TEXT NOT NULL,
	domain TEXT NOT NULL DEFAULT '',
	author TEXT NOT NULL DEFAULT '',
	points INTEGER NOT NULL DEFAULT 0,
	comments INTEGER NOT NULL DEFAULT 0,
	posted_at DATETIME NOT NULL,
	first_seen DATETIME NOT NULL,
	last_seen DATETIME NOT NULL,
	rank INTEGER NOT NULL DEFAULT 0,
	hn_text TEXT NOT NULL DEFAULT '',
	article_text TEXT NOT NULL DEFAULT '',
	extracted_at DATETIME,
	extract_error TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	answers TEXT NOT NULL DEFAULT '[]',
	scored_at DATETIME,
	score_error TEXT NOT NULL DEFAULT '',
	score_attempts INTEGER NOT NULL DEFAULT 0,
	mark TEXT NOT NULL DEFAULT '',
	marked_at DATETIME,
	task_key TEXT NOT NULL DEFAULT '',
	score_failed_at DATETIME,
	read_at DATETIME
);
CREATE TABLE IF NOT EXISTS visits (
	client TEXT PRIMARY KEY,
	visit_at DATETIME NOT NULL,
	last_visit DATETIME
);
CREATE TABLE IF NOT EXISTS usage (
	day TEXT PRIMARY KEY,
	calls INTEGER NOT NULL DEFAULT 0,
	stories INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_stories_first_seen ON stories(first_seen DESC);
CREATE INDEX IF NOT EXISTS idx_stories_mark ON stories(mark, marked_at DESC);
`

// OpenStore opens or creates the database file.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate adds the newer columns to a database from an older build; adding a
// column keeps every row. When read_at is new, a story with a Good or Bad mark
// counts as read from its mark time, as a mark reads a story.
func migrate(ctx context.Context, db *sqlx.DB) error {
	for _, col := range []string{"score_failed_at", "read_at"} {
		if err := addColumn(ctx, db, col); err != nil {
			return err
		}
	}
	return nil
}

// addColumn adds one DATETIME column and, for read_at, reads the marked
// stories in the same transaction. A column that exists already is left as is.
func addColumn(ctx context.Context, db *sqlx.DB, col string) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `ALTER TABLE stories ADD COLUMN `+col+` DATETIME`); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil
		}
		return fmt.Errorf("add column %s: %w", col, err)
	}
	if col == "read_at" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE stories SET read_at = marked_at WHERE mark != '' AND marked_at IS NOT NULL`); err != nil {
			return fmt.Errorf("read marked stories: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit column %s: %w", col, err)
	}
	return nil
}

// visitGap is the pause after which the next visit of a client is a new visit.
const visitGap = 30 * time.Minute

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// StoryRow is a stored story.
type StoryRow struct {
	ID            int64        `db:"id"`
	Title         string       `db:"title"`
	URL           string       `db:"url"`
	Domain        string       `db:"domain"`
	Author        string       `db:"author"`
	Points        int          `db:"points"`
	Comments      int          `db:"comments"`
	PostedAt      time.Time    `db:"posted_at"`
	FirstSeen     time.Time    `db:"first_seen"`
	LastSeen      time.Time    `db:"last_seen"`
	Rank          int          `db:"rank"`
	HNText        string       `db:"hn_text"`
	ArticleText   string       `db:"article_text"`
	ExtractedAt   sql.NullTime `db:"extracted_at"`
	ExtractError  string       `db:"extract_error"`
	Summary       string       `db:"summary"`
	AnswersJSON   string       `db:"answers"`
	ScoredAt      sql.NullTime `db:"scored_at"`
	ScoreError    string       `db:"score_error"`
	ScoreAttempts int          `db:"score_attempts"`
	Mark          string       `db:"mark"`
	MarkedAt      sql.NullTime `db:"marked_at"`
	TaskKey       string       `db:"task_key"`
	ScoreFailedAt sql.NullTime `db:"score_failed_at"`
	ReadAt        sql.NullTime `db:"read_at"`
}

// Answers decodes the stored answers.
func (r StoryRow) Answers() []Answer {
	var out []Answer
	if err := json.Unmarshal([]byte(r.AnswersJSON), &out); err != nil {
		return nil
	}
	return out
}

// UpsertFront saves the current front page. New stories are added; known
// stories get fresh points, comments and rank. Stories that left the front
// page keep their last numbers and get rank 0.
func (s *Store) UpsertFront(ctx context.Context, stories []hn.Story, now time.Time) (added int, err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE stories SET rank = 0 WHERE rank != 0`); err != nil {
		return 0, fmt.Errorf("reset rank: %w", err)
	}
	for _, st := range stories {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO stories (id, title, url, domain, author, points, comments, posted_at, first_seen, last_seen, rank, hn_text)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			st.ID, st.Title, st.URL, Domain(st.URL), st.By, st.Points, st.Comments, st.Posted, now, now, st.Rank, st.Text)
		if err != nil {
			return 0, fmt.Errorf("insert story %d: %w", st.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stories SET title = ?, points = ?, comments = ?, rank = ?, last_seen = ? WHERE id = ?`,
			st.Title, st.Points, st.Comments, st.Rank, now, st.ID); err != nil {
			return 0, fmt.Errorf("update story %d: %w", st.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return added, nil
}

// NeedText returns stories that have no article text attempt yet.
func (s *Store) NeedText(ctx context.Context, limit int) ([]StoryRow, error) {
	var rows []StoryRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM stories WHERE extracted_at IS NULL ORDER BY first_seen DESC, rank LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("select stories without text: %w", err)
	}
	return rows, nil
}

// SaveText stores the article text or the extraction error.
func (s *Store) SaveText(ctx context.Context, id int64, text, extractErr string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE stories SET article_text = ?, extract_error = ?, extracted_at = ? WHERE id = ?`, text, extractErr, now, id)
	if err != nil {
		return fmt.Errorf("save text %d: %w", id, err)
	}
	return nil
}

// NeedScore returns unscored stories with fewer than maxAttempts failed tries
// and no failed try after retryBefore, the newest first.
func (s *Store) NeedScore(ctx context.Context, maxAttempts int, retryBefore time.Time, limit int) ([]StoryRow, error) {
	var rows []StoryRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT * FROM stories WHERE scored_at IS NULL AND score_attempts < ?
		AND (score_failed_at IS NULL OR score_failed_at <= ?)
		ORDER BY first_seen DESC, rank LIMIT ?`, maxAttempts, retryBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("select unscored stories: %w", err)
	}
	return rows, nil
}

// SaveScore stores a summary and answers.
func (s *Store) SaveScore(ctx context.Context, id int64, summary string, answers []Answer, now time.Time) error {
	data, err := json.Marshal(answers)
	if err != nil {
		return fmt.Errorf("encode answers: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE stories SET summary = ?, answers = ?, scored_at = ?, score_error = '', score_attempts = score_attempts + 1
		WHERE id = ?`, summary, string(data), now, id)
	if err != nil {
		return fmt.Errorf("save score %d: %w", id, err)
	}
	return nil
}

// SaveScoreError records a failed scoring attempt.
func (s *Store) SaveScoreError(ctx context.Context, id int64, msg string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE stories SET score_error = ?, score_attempts = score_attempts + 1, score_failed_at = ? WHERE id = ?`, msg, now, id)
	if err != nil {
		return fmt.Errorf("save score error %d: %w", id, err)
	}
	return nil
}

// Since returns stories first seen at or after t, plus all marked stories in that window.
func (s *Store) Since(ctx context.Context, t time.Time) ([]StoryRow, error) {
	var rows []StoryRow
	if err := s.db.SelectContext(ctx, &rows, `SELECT * FROM stories WHERE first_seen >= ?`, t); err != nil {
		return nil, fmt.Errorf("select stories: %w", err)
	}
	return rows, nil
}

// Get returns one story.
func (s *Store) Get(ctx context.Context, id int64) (StoryRow, error) {
	var row StoryRow
	err := s.db.GetContext(ctx, &row, `SELECT * FROM stories WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, fmt.Errorf("get story %d: %w", id, err)
	}
	return row, nil
}

// SetMark sets Good, Bad or no mark. A Good or Bad mark also reads the story.
func (s *Store) SetMark(ctx context.Context, id int64, mark string, now time.Time) error {
	if mark != MarkNone && mark != MarkGood && mark != MarkBad {
		return fmt.Errorf("unknown mark %q", mark)
	}
	if mark == MarkNone {
		return s.update(ctx, id, `UPDATE stories SET mark = '', marked_at = NULL WHERE id = ?`, id)
	}
	return s.update(ctx, id, `UPDATE stories SET mark = ?, marked_at = ?, read_at = COALESCE(read_at, ?) WHERE id = ?`,
		mark, now, now, id)
}

// SetRead marks a story read or unread. A story that is read already keeps its first read time.
func (s *Store) SetRead(ctx context.Context, id int64, read bool, now time.Time) error {
	if !read {
		return s.update(ctx, id, `UPDATE stories SET read_at = NULL WHERE id = ?`, id)
	}
	return s.update(ctx, id, `UPDATE stories SET read_at = COALESCE(read_at, ?) WHERE id = ?`, now, id)
}

// MarkRead reads the given stories that are unread and returns how many it changed.
func (s *Store) MarkRead(ctx context.Context, ids []int64, now time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	query, args, err := sqlx.In(`UPDATE stories SET read_at = ? WHERE read_at IS NULL AND id IN (?)`, now, ids)
	if err != nil {
		return 0, fmt.Errorf("mark read query: %w", err)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mark read: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UnreadReadAt makes unread the stories with exactly this read time and
// returns how many it changed. Mark all read gives all its stories one time.
// A story with a Good or Bad mark stays read: Mark all read reads only
// unread stories, and a mark reads a story, so its mark came after.
func (s *Store) UnreadReadAt(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE stories SET read_at = NULL WHERE read_at = ? AND mark = ''`, at.UTC())
	if err != nil {
		return 0, fmt.Errorf("undo mark all read: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Visit records a visit of a client and returns the end of its previous
// visit (nil for the first one). Calls less than visitGap apart are one visit.
func (s *Store) Visit(ctx context.Context, client string, now time.Time) (*time.Time, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var row struct {
		VisitAt   time.Time    `db:"visit_at"`
		LastVisit sql.NullTime `db:"last_visit"`
	}
	err = tx.GetContext(ctx, &row, `SELECT visit_at, last_visit FROM visits WHERE client = ?`, client)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		row.LastVisit = sql.NullTime{}
	case err != nil:
		return nil, fmt.Errorf("read visit: %w", err)
	case now.Sub(row.VisitAt) >= visitGap:
		row.LastVisit = sql.NullTime{Time: row.VisitAt, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO visits (client, visit_at, last_visit) VALUES (?, ?, ?)
		ON CONFLICT(client) DO UPDATE SET visit_at = excluded.visit_at, last_visit = excluded.last_visit`,
		client, now, row.LastVisit); err != nil {
		return nil, fmt.Errorf("save visit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit visit: %w", err)
	}
	return nullTime(row.LastVisit), nil
}

// LastVisit returns the end of the previous visit of a client, or nil.
func (s *Store) LastVisit(ctx context.Context, client string) (*time.Time, error) {
	var last sql.NullTime
	err := s.db.GetContext(ctx, &last, `SELECT last_visit FROM visits WHERE client = ?`, client)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last visit: %w", err)
	}
	return nullTime(last), nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// SetTask records the task made from a story.
func (s *Store) SetTask(ctx context.Context, id int64, key string) error {
	return s.update(ctx, id, `UPDATE stories SET task_key = ? WHERE id = ?`, key, id)
}

func (s *Store) update(ctx context.Context, id int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update story %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecentMarked returns the titles of the newest stories with the given mark.
func (s *Store) RecentMarked(ctx context.Context, mark string, limit int) ([]string, error) {
	var titles []string
	err := s.db.SelectContext(ctx, &titles,
		`SELECT title FROM stories WHERE mark = ? ORDER BY marked_at DESC LIMIT ?`, mark, limit)
	if err != nil {
		return nil, fmt.Errorf("select marked titles: %w", err)
	}
	return titles, nil
}

// Cleanup deletes unread, unmarked stories first seen before t that have no task.
func (s *Store) Cleanup(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM stories WHERE first_seen < ? AND mark = '' AND task_key = '' AND read_at IS NULL`, t)
	if err != nil {
		return 0, fmt.Errorf("cleanup: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Domain returns the host of a link without "www.".
func Domain(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// Usage returns the model calls and stories counted for one day (YYYY-MM-DD).
func (s *Store) Usage(ctx context.Context, day string) (calls, stories int, err error) {
	row := s.db.QueryRowxContext(ctx, `SELECT calls, stories FROM usage WHERE day = ?`, day)
	if err := row.Scan(&calls, &stories); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read usage: %w", err)
	}
	return calls, stories, nil
}

// AddUsage counts model calls and stories for one day.
func (s *Store) AddUsage(ctx context.Context, day string, calls, stories int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage (day, calls, stories) VALUES (?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET calls = calls + excluded.calls, stories = stories + excluded.stories`,
		day, calls, stories)
	if err != nil {
		return fmt.Errorf("save usage: %w", err)
	}
	return nil
}

// Reserve takes one model call and up to want stories from the day's
// allowance in one write transaction (BEGIN IMMEDIATE), so two curio
// processes cannot both use the last of it. It returns the stories granted;
// 0 means the call or story limit is reached.
func (s *Store) Reserve(ctx context.Context, day string, maxCalls, maxStories, want int) (granted int, err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage (day) VALUES (?) ON CONFLICT(day) DO NOTHING`, day); err != nil {
		return 0, fmt.Errorf("usage row: %w", err)
	}
	var calls, stories int
	if err := tx.QueryRowxContext(ctx, `SELECT calls, stories FROM usage WHERE day = ?`, day).Scan(&calls, &stories); err != nil {
		return 0, fmt.Errorf("read usage: %w", err)
	}
	granted = min(want, maxStories-stories)
	if calls >= maxCalls || granted <= 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage SET calls = calls + 1, stories = stories + ? WHERE day = ?`,
		granted, day); err != nil {
		return 0, fmt.Errorf("save usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit usage: %w", err)
	}
	return granted, nil
}
