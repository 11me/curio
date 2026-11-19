// Command curio fetches the Hacker News front page, scores stories with the
// claude CLI and serves a JSON API on a loopback address.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/jessevdk/go-flags"

	"github.com/umputun/newscope/pkg/content"
	"github.com/umputun/newscope/pkg/curio"
	"github.com/umputun/newscope/pkg/hn"
)

type opts struct {
	Config string `short:"c" long:"config" env:"CURIO_CONFIG" default:"curio.yml" description:"configuration file"`
	Data   string `long:"data" env:"CURIO_DATA" description:"data folder, overrides data_dir"`
	Once   bool   `long:"once" description:"refresh once and exit, no server"`
	Debug  bool   `long:"dbg" description:"debug log"`
}

func main() {
	var o opts
	if _, err := flags.Parse(&o); err != nil {
		var ferr *flags.Error
		if errors.As(err, &ferr) && errors.Is(ferr.Type, flags.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	logOpts := []log.Option{log.Msec, log.LevelBraces}
	if o.Debug {
		logOpts = append(logOpts, log.Debug)
	}
	log.Setup(logOpts...)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, o)
	cancel()
	if err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o opts) error {
	cfg, err := curio.LoadConfig(o.Config)
	if err != nil {
		return err
	}
	if o.Data != "" {
		cfg.DataDir = o.Data
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data folder: %w", err)
	}
	store, err := curio.OpenStore(ctx, filepath.Join(cfg.DataDir, "curio.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	svc := &curio.Service{
		Cfg:       cfg,
		Store:     store,
		HN:        hn.NewClient(20 * time.Second),
		Extractor: textExtractor{x: newExtractor()},
		Scorer:    &curio.ClaudeScorer{Cfg: cfg.Claude, Criteria: cfg.Criteria},
	}
	if o.Once {
		return svc.Refresh(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&curio.API{Svc: svc}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go svc.Run(ctx)
	go func() {
		<-ctx.Done()
		shutCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("[INFO] curio on http://%s, data in %s", cfg.Listen, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func newExtractor() *content.HTTPExtractor {
	x := content.NewHTTPExtractor(20*time.Second, "Mozilla/5.0 (compatible; curio/1.0)")
	x.SetRetryConfig(1, time.Second, time.Second, 0)
	return x
}

type textExtractor struct{ x *content.HTTPExtractor }

func (t textExtractor) Text(ctx context.Context, url string) (string, error) {
	res, err := t.x.Extract(ctx, url)
	if err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	return res.Content, nil
}
