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
		if cfg.DataDir, err = curio.ExpandHome(o.Data); err != nil {
			return err
		}
		if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
			return fmt.Errorf("data folder: %w", err)
		}
	}
	created, err := curio.EnsureAPIToken(o.Config, &cfg)
	if err != nil {
		return err
	}
	if created {
		log.Printf("[INFO] made an API token in %s (api_token). Paste it into the masked field "+
			"bb Settings > Plugins > curio > curio API token", o.Config)
	}
	if err := curio.CheckSubscriptionEnv(os.Environ()); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data folder: %w", err)
	}
	runLock, err := curio.AcquireRunLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer runLock.Close()
	// only the checked, resolved data folder from here on
	cfg.DataDir = runLock.Dir()
	store, err := curio.OpenStore(ctx, filepath.Join(cfg.DataDir, "curio.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	svc := &curio.Service{
		Cfg:       cfg,
		Store:     store,
		HN:        hn.NewClient(20 * time.Second),
		Extractor: curio.NewPageFetcher(30*time.Second, 3<<20),
		Scorer:    &curio.ClaudeScorer{Cfg: cfg.Claude, Criteria: cfg.Criteria},
	}
	if o.Once {
		return svc.Refresh(ctx)
	}

	ln, err := runLock.Listen()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           (&curio.API{Svc: svc, Token: cfg.APIToken}).Handler(),
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
	log.Printf("[INFO] curio on unix socket %s, data in %s", ln.Addr(), cfg.DataDir)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
