package curio

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/go-pkgz/lgr"

	"github.com/umputun/newscope/pkg/hn"
)

// maxScoreAttempts is how many times a story is sent to the model before curio gives up.
const maxScoreAttempts = 3

// Service runs refreshes: fetch the front page, get article text, score new stories.
type Service struct {
	Cfg       Config
	Store     *Store
	HN        FrontPage
	Extractor TextExtractor // nil turns extraction off
	Scorer    Scorer
	Now       func() time.Time

	mu          sync.Mutex
	running     bool
	lastStart   time.Time
	lastRefresh time.Time
	lastError   string
	note        string
	trigger     chan struct{}
	once        sync.Once
}

// FrontPage gives the current front page.
type FrontPage interface {
	TopStories(ctx context.Context, limit int) ([]hn.Story, error)
}

// TextExtractor returns the readable text of a web page.
type TextExtractor interface {
	Text(ctx context.Context, url string) (string, error)
}

// Scorer scores one batch of stories.
type Scorer interface {
	Score(ctx context.Context, in ScoreInput) ([]Scored, error)
}

// Status is the refresh state for the API.
type Status struct {
	Refreshing  bool       `json:"refreshing"`
	LastRefresh *time.Time `json:"last_refresh"`
	LastError   string     `json:"last_error,omitempty"`
	Note        string     `json:"note,omitempty"` // for example a reached daily limit
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) init() {
	s.once.Do(func() { s.trigger = make(chan struct{}, 1) })
}

// Status returns the refresh state.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Refreshing: s.running, LastError: s.lastError, Note: s.note}
	if !s.lastRefresh.IsZero() {
		t := s.lastRefresh
		st.LastRefresh = &t
	}
	return st
}

// Trigger asks the run loop for a refresh now. It refuses, and returns the
// time to wait, while a refresh runs or waits, or when the last refresh
// started less than limits.min_refresh ago.
func (s *Service) Trigger() (ok bool, wait time.Duration) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false, s.Cfg.Limits.MinRefresh
	}
	if !s.lastStart.IsZero() {
		if left := s.Cfg.Limits.MinRefresh - s.now().Sub(s.lastStart); left > 0 {
			return false, left
		}
	}
	select {
	case s.trigger <- struct{}{}:
		s.lastStart = s.now() // counts as started, so a second request waits
		return true, 0
	default:
		return false, s.Cfg.Limits.MinRefresh
	}
}

// Run refreshes at start, on every interval and on Trigger, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.init()
	ticker := time.NewTicker(s.Cfg.Interval)
	defer ticker.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[WARN] refresh: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.trigger:
		}
	}
}

// Refresh runs one full refresh. It returns at once if a refresh is running.
func (s *Service) Refresh(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = true
	s.lastStart = s.now()
	s.mu.Unlock()

	err := s.refresh(ctx)

	s.mu.Lock()
	s.running = false
	s.lastRefresh = s.now()
	s.lastError = ""
	if err != nil {
		s.lastError = err.Error()
	}
	s.mu.Unlock()
	return err
}

func (s *Service) refresh(ctx context.Context) error {
	stories, err := s.HN.TopStories(ctx, s.Cfg.FrontN)
	if err != nil {
		return fmt.Errorf("front page: %w", err)
	}
	added, err := s.Store.UpsertFront(ctx, stories, s.now())
	if err != nil {
		return err
	}
	log.Printf("[INFO] front page: %d stories, %d new", len(stories), added)

	if err := s.extractTexts(ctx); err != nil {
		return err
	}
	scored, err := s.scoreAll(ctx)
	if err != nil {
		return err
	}
	removed, err := s.Store.Cleanup(ctx, s.now().AddDate(0, 0, -s.Cfg.KeepDays))
	if err != nil {
		return err
	}
	log.Printf("[INFO] refresh done: %d scored, %d removed", scored, removed)
	return nil
}

func (s *Service) extractTexts(ctx context.Context) error {
	rows, err := s.Store.NeedText(ctx, 100)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, msg := "", ""
		switch {
		case s.Extractor == nil || !s.Cfg.Extract:
			msg = "extraction off"
		case row.HNText != "" && Domain(row.URL) == "news.ycombinator.com":
			msg = "text post"
		default:
			var xerr error
			text, xerr = s.Extractor.Text(ctx, row.URL)
			if xerr != nil {
				msg = truncate(xerr.Error(), 200)
			}
		}
		if err := s.Store.SaveText(ctx, row.ID, truncate(text, 20000), msg, s.now()); err != nil {
			return err
		}
	}
	return nil
}

// scoreAll scores waiting stories in batches; a failed batch is recorded and skipped.
func (s *Service) scoreAll(ctx context.Context) (int, error) {
	picks, err := s.Store.RecentMarked(ctx, MarkGood, s.Cfg.Claude.Picks)
	if err != nil {
		return 0, err
	}
	rejects, err := s.Store.RecentMarked(ctx, MarkBad, s.Cfg.Claude.Picks)
	if err != nil {
		return 0, err
	}
	now := s.now()
	rows, err := s.Store.NeedScore(ctx, maxScoreAttempts, now.Add(-s.Cfg.Limits.RetryAfter), 200)
	if err != nil {
		return 0, err
	}
	s.setNote("")
	day := now.Format(time.DateOnly)
	total := 0
	for start := 0; start < len(rows); {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		calls, sent, err := s.Store.Usage(ctx, day)
		if err != nil {
			return total, err
		}
		room := min(s.Cfg.Claude.BatchSize, s.Cfg.Limits.DailyStories-sent, len(rows)-start)
		if calls >= s.Cfg.Limits.DailyCalls || room <= 0 {
			left := len(rows) - start
			log.Printf("[WARN] daily limit reached (%d calls, %d stories); %d stories wait for tomorrow", calls, sent, left)
			s.setNote(fmt.Sprintf("daily limit reached; stories that wait for the next UTC day: %d", left))
			break
		}
		batch := rows[start : start+room]
		start += room
		if err := s.Store.AddUsage(ctx, day, 1, len(batch)); err != nil {
			return total, err
		}
		res, err := s.Scorer.Score(ctx, ScoreInput{Stories: batch, PastPicks: picks, PastRejects: rejects})
		if err != nil {
			log.Printf("[WARN] score batch of %d: %v", len(batch), err)
			for _, row := range batch {
				if serr := s.Store.SaveScoreError(ctx, row.ID, truncate(err.Error(), 300), s.now()); serr != nil {
					return total, serr
				}
			}
			continue
		}
		got := map[int64]bool{}
		for _, r := range res {
			if err := s.Store.SaveScore(ctx, r.ID, r.Summary, r.Answers, s.now()); err != nil {
				return total, err
			}
			got[r.ID] = true
			total++
		}
		for _, row := range batch {
			if !got[row.ID] {
				if err := s.Store.SaveScoreError(ctx, row.ID, "missing from model answer", s.now()); err != nil {
					return total, err
				}
			}
		}
	}
	return total, nil
}

func (s *Service) setNote(note string) {
	s.mu.Lock()
	s.note = note
	s.mu.Unlock()
}
