package curio

import (
	"crypto/subtle"
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
	Svc   *Service
	Token string // bearer token; an empty token refuses every call
}

// ListItem is one row of the list.
type ListItem struct {
	ID            int64      `json:"id"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	DiscussionURL string     `json:"discussion_url"`
	Domain        string     `json:"domain"`
	Points        int        `json:"points"`
	Comments      int        `json:"comments"`
	PostedAt      time.Time  `json:"posted_at"`
	FirstSeen     time.Time  `json:"first_seen"`
	Rank          int        `json:"rank"` // front page position now, 0 when it left
	Fit           *float64   `json:"fit"`  // null until scored
	Mark          string     `json:"mark"`
	TaskKey       string     `json:"task_key"`
	ReadAt        *time.Time `json:"read_at"` // null while unread
	New           bool       `json:"new"`     // unread and first seen after the last visit of the client
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
	Stories   []ListItem `json:"stories"`
	Listed    int        `json:"listed"`
	Total     int        `json:"total"`
	Good      int        `json:"good"`
	Bad       int        `json:"bad"`
	Unscored  int        `json:"unscored"`
	Unread    int        `json:"unread"`
	New       int        `json:"new"` // new stories at or above min_fit
	Since     time.Time  `json:"since"`
	AsOf      time.Time  `json:"as_of"`      // server time of this list; use it for as_of and Mark all read
	LastVisit *time.Time `json:"last_visit"` // end of the previous visit of the client, null without one
	Next      *int64     `json:"next"`       // cursor (after) for the next page, null on the last page
	Cutoff    float64    `json:"cutoff"`     // default cutoff from the config
	Status    Status     `json:"status"`
}

// listQuery is the parsed query of the list call.
type listQuery struct {
	minFit float64
	limit  int
	sortBy string
	marks  string
	read   string     // all, unread or read
	asOf   *time.Time // stories read at or after this time still count as unread
	after  int64      // cursor: the last story of the previous page
	search string
	client string
}

// VisitResponse is the answer to a visit.
type VisitResponse struct {
	Client    string     `json:"client"`
	VisitAt   time.Time  `json:"visit_at"`
	LastVisit *time.Time `json:"last_visit"` // end of the previous visit, null on the first one
}

var (
	taskKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-\d{1,7}$`)
	clientRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// Handler returns the API handler with the loopback guard.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/stories", a.list)
	mux.HandleFunc("GET /api/v1/stories/{id}", a.detail)
	mux.HandleFunc("POST /api/v1/stories/{id}/mark", a.mark)
	mux.HandleFunc("POST /api/v1/stories/{id}/task", a.task)
	mux.HandleFunc("POST /api/v1/stories/{id}/read", a.read)
	mux.HandleFunc("POST /api/v1/read", a.readAll)
	mux.HandleFunc("POST /api/v1/visit", a.visit)
	mux.HandleFunc("POST /api/v1/refresh", a.refresh)
	return guard(a.Token, mux)
}

// guard lets in only local, non-browser calls with the API token: a loopback
// Host (no DNS rebinding), no Origin header (no cross-site pages), the bearer
// token (no other users or pages), and JSON bodies on POST.
func guard(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeErr(w, http.StatusForbidden, "browser calls are not allowed")
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or wrong API token")
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
	lq, msg := parseListQuery(r)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	cfg := a.Svc.Cfg
	now := a.Svc.now()
	since := now.AddDate(0, 0, -cfg.Days)
	rows, err := a.Svc.Store.Since(r.Context(), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := ListResponse{Stories: []ListItem{}, Since: since, AsOf: now, Cutoff: cfg.Cutoff, Status: a.Svc.Status(), Total: len(rows)}
	if lq.client != "" {
		if resp.LastVisit, err = a.Svc.Store.LastVisit(r.Context(), lq.client); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	all := make(map[int64]ListItem, len(rows))
	for _, row := range rows {
		item := a.listItem(row)
		item.New = resp.LastVisit != nil && item.ReadAt == nil && row.FirstSeen.After(*resp.LastVisit)
		all[item.ID] = item
		resp.count(item)
		if item.New && fitAtLeast(item, lq.minFit) {
			resp.New++
		}
		if keep(item, row, lq) {
			resp.Stories = append(resp.Stories, item)
		}
	}
	less := itemLess(lq.sortBy)
	sort.Slice(resp.Stories, func(i, j int) bool { return less(resp.Stories[i], resp.Stories[j]) })
	resp.Listed = len(resp.Stories)
	if lq.after != 0 {
		anchor, ok := all[lq.after]
		if !ok {
			writeErr(w, http.StatusBadRequest, "the after story is not in the list; load the list again")
			return
		}
		start := sort.Search(len(resp.Stories), func(i int) bool { return less(anchor, resp.Stories[i]) })
		resp.Stories = resp.Stories[start:]
	}
	if len(resp.Stories) > lq.limit {
		resp.Stories = resp.Stories[:lq.limit]
		next := resp.Stories[lq.limit-1].ID
		resp.Next = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (resp *ListResponse) count(item ListItem) {
	switch item.Mark {
	case MarkGood:
		resp.Good++
	case MarkBad:
		resp.Bad++
	}
	if item.Fit == nil {
		resp.Unscored++
	}
	if item.ReadAt == nil {
		resp.Unread++
	}
}

// parseListQuery reads the list query; a non-empty message means a bad query.
func parseListQuery(r *http.Request) (lq listQuery, msg string) {
	q := r.URL.Query()
	var err error
	if lq.minFit, err = floatParam(q.Get("min_fit"), 0); err != nil || lq.minFit < 0 || lq.minFit > 1 {
		return lq, "min_fit must be 0 to 1"
	}
	if lq.limit, err = strconv.Atoi(defaultStr(q.Get("limit"), "200")); err != nil || lq.limit < 1 || lq.limit > 500 {
		return lq, "limit must be 1 to 500"
	}
	if lq.sortBy = defaultStr(q.Get("sort"), "fit"); lq.sortBy != "fit" && lq.sortBy != "date" && lq.sortBy != "points" {
		return lq, "sort must be fit, date or points"
	}
	lq.marks = defaultStr(q.Get("marks"), "all")
	if lq.marks != "all" && lq.marks != "hide" && lq.marks != "good" && lq.marks != "bad" {
		return lq, "marks must be all, hide, good or bad"
	}
	if lq.read = defaultStr(q.Get("read"), "all"); lq.read != "all" && lq.read != "unread" && lq.read != "read" {
		return lq, "read must be all, unread or read"
	}
	if v := q.Get("as_of"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return lq, "as_of must be an RFC 3339 time"
		}
		t = t.UTC()
		lq.asOf = &t
	}
	if v := q.Get("after"); v != "" {
		if lq.after, err = strconv.ParseInt(v, 10, 64); err != nil || lq.after <= 0 {
			return lq, "after must be a story id"
		}
	}
	if lq.client = q.Get("client"); lq.client != "" && !clientRe.MatchString(lq.client) {
		return lq, "client must be 1 to 32 lowercase letters, digits or dashes"
	}
	lq.search = strings.ToLower(strings.TrimSpace(q.Get("q")))
	return lq, ""
}

func fitAtLeast(item ListItem, minFit float64) bool {
	return minFit <= 0 || (item.Fit != nil && *item.Fit >= minFit)
}

func keep(item ListItem, row StoryRow, lq listQuery) bool {
	if !fitAtLeast(item, lq.minFit) {
		return false
	}
	switch lq.marks {
	case "hide":
		if row.Mark != MarkNone {
			return false
		}
	case "good", "bad":
		if row.Mark != lq.marks {
			return false
		}
	}
	switch lq.read {
	case "unread":
		if item.ReadAt != nil && (lq.asOf == nil || item.ReadAt.Before(*lq.asOf)) {
			return false
		}
	case "read":
		if item.ReadAt == nil {
			return false
		}
	}
	if lq.search != "" {
		hay := strings.ToLower(row.Title + " " + row.Domain + " " + row.Summary)
		for _, word := range strings.Fields(lq.search) {
			if !strings.Contains(hay, word) {
				return false
			}
		}
	}
	return true
}

// itemLess returns a total order for the list, so a story id works as a page cursor.
func itemLess(by string) func(a, b ListItem) bool {
	return func(a, b ListItem) bool {
		switch by {
		case "date":
			if !a.PostedAt.Equal(b.PostedAt) {
				return a.PostedAt.After(b.PostedAt)
			}
		case "points":
			if a.Points != b.Points {
				return a.Points > b.Points
			}
		default:
			if fa, fb := fitOr(a.Fit), fitOr(b.Fit); fa != fb {
				return fa > fb
			}
			if !a.PostedAt.Equal(b.PostedAt) {
				return a.PostedAt.After(b.PostedAt)
			}
		}
		return a.ID > b.ID
	}
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
		Rank: row.Rank, Mark: row.Mark, TaskKey: row.TaskKey, ReadAt: nullTime(row.ReadAt),
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

func (a *API) read(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Read *bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Read == nil {
		writeErr(w, http.StatusBadRequest, "body must be {\"read\": true} or {\"read\": false}")
		return
	}
	if err := a.Svc.Store.SetRead(r.Context(), id, *body.Read, a.Svc.now()); err != nil {
		writeStoreErr(w, err)
		return
	}
	a.writeDetail(w, r, id)
}

func (a *API) readAll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Before string `json:"before"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	before, err := time.Parse(time.RFC3339Nano, body.Before)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "before must be an RFC 3339 time, for example the as_of of the list")
		return
	}
	n, err := a.Svc.Store.MarkAllRead(r.Context(), before, a.Svc.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"marked": n})
}

func (a *API) visit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !clientRe.MatchString(body.Client) {
		writeErr(w, http.StatusBadRequest, "client must be 1 to 32 lowercase letters, digits or dashes")
		return
	}
	now := a.Svc.now()
	last, err := a.Svc.Store.Visit(r.Context(), body.Client, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, VisitResponse{Client: body.Client, VisitAt: now, LastVisit: last})
}

func (a *API) refresh(w http.ResponseWriter, _ *http.Request) {
	if ok, wait := a.Svc.Trigger(); !ok {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("a refresh ran recently; wait %s", wait.Round(time.Second)))
		return
	}
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
