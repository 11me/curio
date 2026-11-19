package curio

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/umputun/newscope/pkg/hn"
)

// API serves the JSON API for the bb view.
type API struct {
	Svc *Service
}

// ListItem is one row of the list.
type ListItem struct {
	ID            int64     `json:"id"`
	Title         string    `json:"title"`
	URL           string    `json:"url"`
	DiscussionURL string    `json:"discussion_url"`
	Domain        string    `json:"domain"`
	Points        int       `json:"points"`
	Comments      int       `json:"comments"`
	PostedAt      time.Time `json:"posted_at"`
	FirstSeen     time.Time `json:"first_seen"`
	Rank          int       `json:"rank"` // front page position now, 0 when it left
	Fit           *float64  `json:"fit"`  // null until scored
	Mark          string    `json:"mark"`
	TaskKey       string    `json:"task_key"`
}

// Detail is one story with its summary and reasons.
type Detail struct {
	ListItem
	Author     string     `json:"author"`
	Summary    string     `json:"summary"`
	Reasons    []Reason   `json:"reasons"`  // up to three rows that explain the fit
	Criteria   []Reason   `json:"criteria"` // all rows, in config order
	Bias       float64    `json:"bias"`
	ScoredAt   *time.Time `json:"scored_at"`
	ScoreError string     `json:"score_error,omitempty"`
}

// ListResponse is the list with counts.
type ListResponse struct {
	Stories  []ListItem `json:"stories"`
	Listed   int        `json:"listed"`
	Total    int        `json:"total"`
	Good     int        `json:"good"`
	Bad      int        `json:"bad"`
	Unscored int        `json:"unscored"`
	Since    time.Time  `json:"since"`
	Cutoff   float64    `json:"cutoff"` // default cutoff from the config
	Status   Status     `json:"status"`
}

var taskKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[0-9]{1,7}$`)

// Handler returns the API handler with the loopback guard.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/stories", a.list)
	mux.HandleFunc("GET /api/v1/stories/{id}", a.detail)
	mux.HandleFunc("POST /api/v1/stories/{id}/mark", a.mark)
	mux.HandleFunc("POST /api/v1/stories/{id}/task", a.task)
	mux.HandleFunc("POST /api/v1/refresh", a.refresh)
	return guard(mux)
}

// guard lets in only local, non-browser calls: a loopback Host (no DNS
// rebinding), no Origin header (no cross-site pages), and JSON bodies on POST
// (a form post from a page cannot set that type without a CORS preflight).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeErr(w, http.StatusForbidden, "browser calls are not allowed")
			return
		}
		if r.Method == http.MethodPost {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" {
				writeErr(w, http.StatusUnsupportedMediaType, "use application/json")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *API) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Svc.Status())
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minFit, err := floatParam(q.Get("min_fit"), 0)
	if err != nil || minFit < 0 || minFit > 1 {
		writeErr(w, http.StatusBadRequest, "min_fit must be 0 to 1")
		return
	}
	limit, err := strconv.Atoi(defaultStr(q.Get("limit"), "200"))
	if err != nil || limit < 1 || limit > 500 {
		writeErr(w, http.StatusBadRequest, "limit must be 1 to 500")
		return
	}
	sortBy := defaultStr(q.Get("sort"), "fit")
	if sortBy != "fit" && sortBy != "date" && sortBy != "points" {
		writeErr(w, http.StatusBadRequest, "sort must be fit, date or points")
		return
	}
	marks := defaultStr(q.Get("marks"), "all")
	if marks != "all" && marks != "hide" && marks != "good" && marks != "bad" {
		writeErr(w, http.StatusBadRequest, "marks must be all, hide, good or bad")
		return
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))

	cfg := a.Svc.Cfg
	since := a.Svc.now().AddDate(0, 0, -cfg.Days)
	rows, err := a.Svc.Store.Since(r.Context(), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := ListResponse{Stories: []ListItem{}, Since: since, Cutoff: cfg.Cutoff, Status: a.Svc.Status(), Total: len(rows)}
	for _, row := range rows {
		item := a.listItem(row)
		switch {
		case row.Mark == MarkGood:
			resp.Good++
		case row.Mark == MarkBad:
			resp.Bad++
		}
		if item.Fit == nil {
			resp.Unscored++
		}
		if !keep(item, row, minFit, marks, search) {
			continue
		}
		resp.Stories = append(resp.Stories, item)
	}
	sortItems(resp.Stories, sortBy)
	resp.Listed = len(resp.Stories)
	if len(resp.Stories) > limit {
		resp.Stories = resp.Stories[:limit]
	}
	writeJSON(w, http.StatusOK, resp)
}

func keep(item ListItem, row StoryRow, minFit float64, marks, search string) bool {
	if minFit > 0 && (item.Fit == nil || *item.Fit < minFit) {
		return false
	}
	switch marks {
	case "hide":
		if row.Mark != MarkNone {
			return false
		}
	case "good", "bad":
		if row.Mark != marks {
			return false
		}
	}
	if search != "" {
		hay := strings.ToLower(row.Title + " " + row.Domain + " " + row.Summary)
		for _, word := range strings.Fields(search) {
			if !strings.Contains(hay, word) {
				return false
			}
		}
	}
	return true
}

func sortItems(items []ListItem, by string) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch by {
		case "date":
			return a.PostedAt.After(b.PostedAt)
		case "points":
			return a.Points > b.Points
		}
		fa, fb := fitOr(a.Fit), fitOr(b.Fit)
		if fa != fb {
			return fa > fb
		}
		return a.PostedAt.After(b.PostedAt)
	})
}

func fitOr(f *float64) float64 {
	if f == nil {
		return -1
	}
	return *f
}

func (a *API) listItem(row StoryRow) ListItem {
	item := ListItem{
		ID: row.ID, Title: row.Title, URL: row.URL, DiscussionURL: hn.DiscussionURL(row.ID), Domain: row.Domain,
		Points: row.Points, Comments: row.Comments, PostedAt: row.PostedAt, FirstSeen: row.FirstSeen,
		Rank: row.Rank, Mark: row.Mark, TaskKey: row.TaskKey,
	}
	if row.ScoredAt.Valid {
		fit, _ := Fit(a.Svc.Cfg.Bias, a.Svc.Cfg.Criteria, row.Answers())
		item.Fit = &fit
	}
	return item
}

func (a *API) detail(w http.ResponseWriter, r *http.Request) {
	row, ok := a.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.detailOf(row))
}

func (a *API) detailOf(row StoryRow) Detail {
	d := Detail{ListItem: a.listItem(row), Author: row.Author, Summary: row.Summary, Bias: a.Svc.Cfg.Bias,
		Reasons: []Reason{}, Criteria: []Reason{}, ScoreError: row.ScoreError}
	if row.ScoredAt.Valid {
		t := row.ScoredAt.Time
		d.ScoredAt = &t
		fit, rows := Fit(a.Svc.Cfg.Bias, a.Svc.Cfg.Criteria, row.Answers())
		d.Criteria = rows
		d.Reasons = TopReasons(rows, fit, a.Svc.Cfg.Cutoff, 3)
	}
	return d
}

func (a *API) mark(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Mark string `json:"mark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	mark := body.Mark
	if mark == "none" {
		mark = MarkNone
	}
	if mark != MarkNone && mark != MarkGood && mark != MarkBad {
		writeErr(w, http.StatusBadRequest, "mark must be good, bad or none")
		return
	}
	if err := a.Svc.Store.SetMark(r.Context(), id, mark, a.Svc.now()); err != nil {
		writeStoreErr(w, err)
		return
	}
	a.writeDetail(w, r, id)
}

func (a *API) task(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if body.Key != "" && !taskKeyRe.MatchString(body.Key) {
		writeErr(w, http.StatusBadRequest, "key must look like PER-12")
		return
	}
	if err := a.Svc.Store.SetTask(r.Context(), id, body.Key); err != nil {
		writeStoreErr(w, err)
		return
	}
	a.writeDetail(w, r, id)
}

func (a *API) refresh(w http.ResponseWriter, _ *http.Request) {
	a.Svc.Trigger()
	writeJSON(w, http.StatusAccepted, a.Svc.Status())
}

func (a *API) writeDetail(w http.ResponseWriter, r *http.Request, id int64) {
	row, err := a.Svc.Store.Get(r.Context(), id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.detailOf(row))
}

func (a *API) load(w http.ResponseWriter, r *http.Request) (StoryRow, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return StoryRow{}, false
	}
	row, err := a.Svc.Store.Get(r.Context(), id)
	if err != nil {
		writeStoreErr(w, err)
		return StoryRow{}, false
	}
	return row, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "bad story id")
		return 0, false
	}
	return id, true
}

func floatParam(s string, def float64) (float64, error) {
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return v, nil
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
