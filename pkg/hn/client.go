// Package hn reads stories from the Hacker News Firebase API.
package hn

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the public Hacker News API root.
const DefaultBaseURL = "https://hacker-news.firebaseio.com/v0"

// Client fetches front page stories with points and comment counts.
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	Workers   int
	UserAgent string
}

// NewClient makes a client with sane defaults.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		BaseURL:   DefaultBaseURL,
		HTTP:      &http.Client{Timeout: timeout},
		Workers:   8,
		UserAgent: "curio/1.0 (+https://github.com/11me/curio)",
	}
}

// Story is one Hacker News story.
type Story struct {
	ID       int64
	Title    string
	URL      string // link of the story, or the HN discussion page for text posts
	Text     string // plain text of a text post (Ask HN), empty for link posts
	By       string
	Points   int
	Comments int
	Posted   time.Time
	Rank     int // 1-based position in the list it came from
}

// DiscussionURL returns the HN comments page of a story.
func DiscussionURL(id int64) string {
	return "https://news.ycombinator.com/item?id=" + strconv.FormatInt(id, 10)
}

// TopStories returns the first limit stories of the front page, in rank order.
// Items that fail to load, are dead or deleted, or are not stories are skipped.
func (c *Client) TopStories(ctx context.Context, limit int) ([]Story, error) {
	var ids []int64
	if err := c.getJSON(ctx, "/topstories.json", &ids); err != nil {
		return nil, fmt.Errorf("top stories: %w", err)
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}

	results := make([]*Story, len(ids))
	workers := max(c.Workers, 1)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			st, err := c.Item(ctx, id)
			if err != nil || st == nil {
				return
			}
			st.Rank = i + 1
			results[i] = st
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]Story, 0, len(results))
	for _, st := range results {
		if st != nil {
			out = append(out, *st)
		}
	}
	return out, nil
}

// Item loads one story. It returns nil without error for items that are not live stories.
func (c *Client) Item(ctx context.Context, id int64) (*Story, error) {
	var it item
	if err := c.getJSON(ctx, "/item/"+strconv.FormatInt(id, 10)+".json", &it); err != nil {
		return nil, fmt.Errorf("item %d: %w", id, err)
	}
	if it.ID == 0 || it.Dead || it.Deleted || it.Type != "story" || strings.TrimSpace(it.Title) == "" {
		return nil, nil
	}
	st := &Story{
		ID:       it.ID,
		Title:    strings.TrimSpace(it.Title),
		URL:      strings.TrimSpace(it.URL),
		Text:     PlainText(it.Text),
		By:       it.By,
		Points:   it.Score,
		Comments: it.Descendants,
		Posted:   time.Unix(it.Time, 0).UTC(),
	}
	if !isWebURL(st.URL) {
		st.URL = DiscussionURL(it.ID)
	}
	return st, nil
}

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	spaceRe = regexp.MustCompile(`[ \t]+`)
)

// PlainText turns the small HTML subset of HN text posts into plain text.
func PlainText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "<p>", "\n\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func isWebURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

type item struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	By          string `json:"by"`
	Time        int64  `json:"time"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Text        string `json:"text"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
	Dead        bool   `json:"dead"`
	Deleted     bool   `json:"deleted"`
}
