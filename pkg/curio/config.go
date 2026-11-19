// Package curio is a small Hacker News curator: it fetches the front page,
// scores each story against weighted criteria with the claude CLI, and serves
// the result as a JSON API on the loopback interface.
package curio

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the curio configuration file.
type Config struct {
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
		DataDir:  "~/.curio",
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
	if cfg.DataDir, err = ExpandHome(cfg.DataDir); err != nil {
		return cfg, err
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(filepath.Dir(path), cfg.DataDir)
	}
	if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
		return cfg, fmt.Errorf("data_dir: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the values that would make curio unsafe or useless.
func (c Config) Validate() error {
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

// EnsureAPIToken makes a random API token when the config has none. It holds
// a lock on path+".lock" so two first starts end with one token, reads the
// file again under the lock and reuses a token saved meanwhile, sets the
// api_token key in the parsed YAML, checks the result, and writes it through
// a new 0600 file and a rename. An existing config with a token gets mode 0600.
func EnsureAPIToken(path string, cfg *Config) (created bool, err error) {
	if cfg.APIToken != "" {
		return false, makePrivate(path)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path comes from the operator
	if err != nil {
		return false, fmt.Errorf("token lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // fd fits in int
		return false, fmt.Errorf("token lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }() //nolint:gosec // fd fits in int

	data, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	if n, err := yamlDocuments(data); err != nil {
		return false, err
	} else if n > 1 {
		return false, fmt.Errorf("%s has more than one YAML document; keep one document so curio can add api_token", path)
	}
	var saved struct {
		APIToken string `yaml:"api_token"`
	}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	if saved.APIToken != "" {
		cfg.APIToken = saved.APIToken
		return false, makePrivate(path)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false, fmt.Errorf("make token: %w", err)
	}
	token := hex.EncodeToString(buf)
	updated, err := setTokenYAML(data, token)
	if err != nil {
		return false, err
	}
	if err := writePrivate(path, updated); err != nil {
		return false, err
	}
	cfg.APIToken = token
	return true, nil
}

// setTokenYAML sets api_token in a YAML document and checks that the result
// parses with that token.
func setTokenYAML(data []byte, token string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config is not a YAML mapping")
	}
	root := doc.Content[0]
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: token}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "api_token" {
			value.LineComment = root.Content[i+1].LineComment
			root.Content[i+1] = value
			found = true
		}
	}
	if !found {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "api_token",
			HeadComment: "API token for the bb plugin, made by curio. Keep this file private."}
		root.Content = append(root.Content, key, value)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	var check Config
	if err := yaml.Unmarshal(out.Bytes(), &check); err != nil {
		return nil, fmt.Errorf("the updated config does not read back; it was not saved: %w", err)
	}
	if check.APIToken != token {
		return nil, fmt.Errorf("the updated config has another api_token; it was not saved")
	}
	return out.Bytes(), nil
}

// yamlDocuments counts the documents in a YAML stream.
func yamlDocuments(data []byte) (int, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	n := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("parse config: %w", err)
		}
		n++
	}
}

func makePrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("make config private: %w", err)
		}
	}
	return nil
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
