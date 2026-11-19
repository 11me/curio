// Package curio is a small Hacker News curator: it fetches the front page,
// scores each story against weighted criteria with the claude CLI, and serves
// the result as a JSON API on the loopback interface.
package curio

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the curio configuration file.
type Config struct {
	Socket   string        `yaml:"socket"`    // Unix socket of the API, in a folder only the owner can open
	DataDir  string        `yaml:"data_dir"`  // folder for the SQLite database
	Interval time.Duration `yaml:"interval"`  // time between front page refreshes
	FrontN   int           `yaml:"front_n"`   // number of front page stories to read
	KeepDays int           `yaml:"keep_days"` // unmarked stories older than this are deleted
	Days     int           `yaml:"days"`      // the list shows stories first seen in this many days
	Cutoff   float64       `yaml:"cutoff"`    // default fit cutoff for clients
	Extract  bool          `yaml:"extract"`   // fetch article text for the scorer
	APIToken string        `yaml:"api_token"` // bearer token for the API, made on first run

	Claude   ClaudeConfig `yaml:"claude"`
	Limits   LimitsConfig `yaml:"limits"`
	Bias     float64      `yaml:"bias"`
	Criteria []Criterion  `yaml:"criteria"`
}

// ClaudeConfig controls the claude CLI calls.
type ClaudeConfig struct {
	Command   string        `yaml:"command"`        // path to the claude CLI
	Model     string        `yaml:"model"`          // model alias, for example haiku
	BatchSize int           `yaml:"batch_size"`     // stories per call
	Timeout   time.Duration `yaml:"timeout"`        // time limit of one call
	TextChars int           `yaml:"text_chars"`     // article text characters sent per story
	Picks     int           `yaml:"picks"`          // recent Good and Bad titles sent as examples
	MaxTurns  int           `yaml:"max_turns"`      // model turns per call (structured output needs 2)
	MaxBudget float64       `yaml:"max_budget_usd"` // spend cap per call, in USD at API prices
}

// LimitsConfig caps how much of the subscription curio can use.
type LimitsConfig struct {
	DailyCalls   int           `yaml:"daily_calls"`   // claude CLI calls per UTC day
	DailyStories int           `yaml:"daily_stories"` // stories sent to the model per UTC day
	MinRefresh   time.Duration `yaml:"min_refresh"`   // shortest time between two refresh starts
	RetryAfter   time.Duration `yaml:"retry_after"`   // wait before a failed story is sent again
}

// Criterion is one yes or no question with a weight.
type Criterion struct {
	ID       string  `yaml:"id"`
	Label    string  `yaml:"label"`    // short plain-words reason, shown in the view
	Question string  `yaml:"question"` // question for the model
	Weight   float64 `yaml:"weight"`
}

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Socket:   DefaultSocket,
		DataDir:  "./var",
		Interval: 30 * time.Minute,
		FrontN:   30,
		KeepDays: 14,
		Days:     7,
		Cutoff:   0.5,
		Extract:  true,
		Claude: ClaudeConfig{
			Command:   "claude",
			Model:     "haiku",
			BatchSize: 10,
			Timeout:   4 * time.Minute,
			TextChars: 3000,
			Picks:     20,
			MaxTurns:  3,
			MaxBudget: 0.25,
		},
		Limits: LimitsConfig{DailyCalls: 24, DailyStories: 200, MinRefresh: 5 * time.Minute, RetryAfter: 30 * time.Minute},
		Bias:   -2,
	}
}

// LoadConfig reads a YAML file over the defaults and checks it.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(filepath.Dir(path), cfg.DataDir)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the values that would make curio unsafe or useless.
func (c Config) Validate() error {
	if c.Socket == "" || (!filepath.IsAbs(c.Socket) && !strings.HasPrefix(c.Socket, "~/")) {
		return fmt.Errorf("socket %q: use an absolute path or a path in ~/", c.Socket)
	}
	if c.Interval < time.Minute {
		return fmt.Errorf("interval %s: use one minute or more", c.Interval)
	}
	if c.FrontN < 1 || c.FrontN > 100 {
		return fmt.Errorf("front_n %d: use 1 to 100", c.FrontN)
	}
	if c.Cutoff < 0 || c.Cutoff > 1 {
		return fmt.Errorf("cutoff %v: use 0 to 1", c.Cutoff)
	}
	if c.Days < 1 || c.KeepDays < c.Days {
		return fmt.Errorf("days %d, keep_days %d: days must be 1 or more and not above keep_days", c.Days, c.KeepDays)
	}
	if c.APIToken != "" && len(c.APIToken) < 32 {
		return fmt.Errorf("api_token: use 32 characters or more")
	}
	if c.Claude.Command == "" || c.Claude.Model == "" {
		return fmt.Errorf("claude.command and claude.model are required")
	}
	if c.Claude.BatchSize < 1 || c.Claude.BatchSize > 30 {
		return fmt.Errorf("claude.batch_size %d: use 1 to 30", c.Claude.BatchSize)
	}
	if c.Claude.MaxTurns < 2 || c.Claude.MaxTurns > 5 {
		return fmt.Errorf("claude.max_turns %d: use 2 to 5", c.Claude.MaxTurns)
	}
	if c.Claude.MaxBudget <= 0 || c.Claude.MaxBudget > 5 {
		return fmt.Errorf("claude.max_budget_usd %v: use more than 0 and at most 5", c.Claude.MaxBudget)
	}
	if c.Limits.DailyCalls < 1 || c.Limits.DailyStories < 1 {
		return fmt.Errorf("limits.daily_calls and limits.daily_stories must be 1 or more")
	}
	if c.Limits.MinRefresh < time.Minute || c.Limits.RetryAfter < time.Minute {
		return fmt.Errorf("limits.min_refresh and limits.retry_after must be one minute or more")
	}
	if len(c.Criteria) == 0 || len(c.Criteria) > 20 {
		return fmt.Errorf("criteria: give 1 to 20 criteria")
	}
	seen := map[string]bool{}
	for _, cr := range c.Criteria {
		if !idRe.MatchString(cr.ID) {
			return fmt.Errorf("criterion id %q: use lowercase letters, digits and _", cr.ID)
		}
		if seen[cr.ID] {
			return fmt.Errorf("criterion id %q: used twice", cr.ID)
		}
		seen[cr.ID] = true
		if cr.Question == "" || cr.Label == "" {
			return fmt.Errorf("criterion %q: label and question are required", cr.ID)
		}
	}
	return nil
}

var emptyTokenRe = regexp.MustCompile(`(?m)^api_token:[ \t]*(""|'')?[ \t]*(#.*)?$`)

// EnsureAPIToken makes a random API token when the config has none. It writes
// the new config to a 0600 temp file in the same folder and renames it over
// the old file, so the token never sits in a file that others can read.
// An existing config with a token gets mode 0600.
func EnsureAPIToken(path string, cfg *Config) (created bool, err error) {
	if cfg.APIToken != "" {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat config: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return false, fmt.Errorf("make config private: %w", err)
			}
		}
		return false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false, fmt.Errorf("make token: %w", err)
	}
	token := hex.EncodeToString(buf)
	old, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	entry := "api_token: " + token
	var updated string
	if emptyTokenRe.Match(old) {
		updated = emptyTokenRe.ReplaceAllLiteralString(string(old), entry)
	} else {
		updated = strings.TrimRight(string(old), "\n") +
			"\n\n# API token for the bb plugin, made by curio. Keep this file private.\n" + entry + "\n"
	}
	if err := writePrivate(path, []byte(updated)); err != nil {
		return false, err
	}
	cfg.APIToken = token
	return true, nil
}

// writePrivate replaces path with data through a new 0600 file and a rename.
func writePrivate(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".curio-*.yml") // CreateTemp uses mode 0600
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("make temp config private: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
