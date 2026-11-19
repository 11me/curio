// Package curio is a small Hacker News curator: it fetches the front page,
// scores each story against weighted criteria with the claude CLI, and serves
// the result as a JSON API on the loopback interface.
package curio

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the curio configuration file.
type Config struct {
	Listen   string        `yaml:"listen"`    // loopback address, for example 127.0.0.1:8090
	DataDir  string        `yaml:"data_dir"`  // folder for the SQLite database
	Interval time.Duration `yaml:"interval"`  // time between front page refreshes
	FrontN   int           `yaml:"front_n"`   // number of front page stories to read
	KeepDays int           `yaml:"keep_days"` // unmarked stories older than this are deleted
	Days     int           `yaml:"days"`      // the list shows stories first seen in this many days
	Cutoff   float64       `yaml:"cutoff"`    // default fit cutoff for clients
	Extract  bool          `yaml:"extract"`   // fetch article text for the scorer

	Claude   ClaudeConfig `yaml:"claude"`
	Bias     float64      `yaml:"bias"`
	Criteria []Criterion  `yaml:"criteria"`
}

// ClaudeConfig controls the claude CLI calls.
type ClaudeConfig struct {
	Command   string        `yaml:"command"`    // path to the claude CLI
	Model     string        `yaml:"model"`      // model alias, for example haiku
	BatchSize int           `yaml:"batch_size"` // stories per call
	Timeout   time.Duration `yaml:"timeout"`    // time limit of one call
	TextChars int           `yaml:"text_chars"` // article text characters sent per story
	Picks     int           `yaml:"picks"`      // recent Good and Bad titles sent as examples
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
		Listen:   "127.0.0.1:8090",
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
		},
		Bias: -2,
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
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen %q: curio has no login, use a loopback address", c.Listen)
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
	if c.Claude.Command == "" || c.Claude.Model == "" {
		return fmt.Errorf("claude.command and claude.model are required")
	}
	if c.Claude.BatchSize < 1 || c.Claude.BatchSize > 30 {
		return fmt.Errorf("claude.batch_size %d: use 1 to 30", c.Claude.BatchSize)
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
