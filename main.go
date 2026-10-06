// narc is an autoscaler for ephemeral GitHub Actions runners on Nomad.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "narc.toml", "path to the TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *configPath, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

type target struct {
	cfg *TargetConfig
	gh  *scaleset.Client
}

func run(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	nomad, err := newNomadClient(cfg.Nomad.Namespace)
	if err != nil {
		return fmt.Errorf("nomad client: %w", err)
	}

	var scalers []*Scaler
	var ready atomic.Pointer[[]*Scaler]
	srv := &http.Server{Addr: cfg.Listen, Handler: mux(&ready)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	// Startup is the only reconciliation point; retry until GitHub and Nomad
	// cooperate rather than crash-looping.
	var targets []target
	err = retry(ctx, log, "reconcile scale sets", func() error {
		targets, scalers, err = setup(ctx, cfg, nomad, log)
		return err
	})
	if err != nil {
		return err
	}
	ready.Store(&scalers)

	var wg sync.WaitGroup
	wg.Go(func() { nomad.Watch(ctx, scalers, log) })
	for _, s := range scalers {
		gh := targets[slices.IndexFunc(targets, func(t target) bool { return t.cfg.URL == s.Target })].gh
		wg.Go(func() { runScaleSet(ctx, s, gh, nomad, log) })
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				if err := s.Reconcile(ctx); err != nil {
					s.log.Error("reconcile failed", "error", err)
				}
			}
		})
	}
	wg.Wait()
	return ctx.Err()
}

// setup reconciles scale sets on GitHub, builds a scaler per scale set and
// recovers state left by a previous process.
func setup(ctx context.Context, cfg *Config, nomad *nomadClient, log *slog.Logger) ([]target, []*Scaler, error) {
	owned, err := nomad.ReadOwned(ctx, cfg.Nomad.StateVariable)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", cfg.Nomad.StateVariable, err)
	}
	var targets []target
	var scalers []*Scaler
	for _, t := range cfg.Targets {
		gh, err := newGitHubClient(t)
		if err != nil {
			return nil, nil, fmt.Errorf("target %s: %w", t.URL, err)
		}
		ids, keep, err := reconcileScaleSets(ctx, gh, t, owned[t.URL], log)
		if err != nil {
			return nil, nil, fmt.Errorf("target %s: %w", t.URL, err)
		}
		owned[t.URL] = keep
		// Persist after each target so a later failure doesn't forget what was created.
		if err := nomad.WriteOwned(ctx, cfg.Nomad.StateVariable, owned); err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", cfg.Nomad.StateVariable, err)
		}
		targets = append(targets, target{t, gh})
		for _, sc := range t.ScaleSets {
			scalers = append(scalers, NewScaler(sc, t.URL, ids[sc.Name], gh, nomad, log))
		}
	}
	for t, ids := range owned {
		if !slices.ContainsFunc(cfg.Targets, func(c *TargetConfig) bool { return c.URL == t }) && len(ids) > 0 {
			log.Warn("target no longer configured; its scale sets can't be deleted without credentials", "target", t, "ids", ids)
		}
	}

	jobs := map[string]bool{}
	for _, s := range scalers {
		if !jobs[s.Job] {
			jobs[s.Job] = true
			if err := Recover(ctx, nomad, s.Job, scalers, log); err != nil {
				return nil, nil, err
			}
		}
	}
	return targets, scalers, nil
}

// runScaleSet keeps a listener running for one scale set. A runner job that
// breaks the contract stops only its own scale set, and is rechecked on
// every retry.
func runScaleSet(ctx context.Context, s *Scaler, gh *scaleset.Client, nomad *nomadClient, log *slog.Logger) {
	owner, _ := os.Hostname()
	_ = retry(ctx, s.log, "listener", func() error {
		if err := nomad.CheckRunnerJob(ctx, s.Job); err != nil {
			return err
		}
		session, err := gh.MessageSessionClient(ctx, s.ID, owner)
		if err != nil {
			return fmt.Errorf("message session: %w", err)
		}
		defer session.Close(context.WithoutCancel(ctx))
		l, err := listener.New(session, listener.Config{ScaleSetID: s.ID, MaxRunners: s.Max, Logger: s.log},
			listener.WithMetricsRecorder(statsRecorder{s.Target, s.Name}))
		if err != nil {
			return err
		}
		s.active.Store(true)
		defer s.active.Store(false)
		s.log.Info("listening")
		return l.Run(ctx, s)
	})
}

func retry(ctx context.Context, log *slog.Logger, what string, f func() error) error {
	backoff := 5 * time.Second
	for {
		start := time.Now()
		err := f()
		if time.Since(start) > 5*time.Minute {
			backoff = 5 * time.Second // it ran fine for a while; this is a fresh failure
		}
		if err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error(what+" failed, retrying", "error", err, "in", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func mux(ready *atomic.Pointer[[]*Scaler]) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.Handler())
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ss := ready.Load()
		if ss == nil {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		for _, s := range *ss {
			if !s.active.Load() {
				http.Error(w, "no message session for "+s.Target+" "+s.Name, http.StatusServiceUnavailable)
				return
			}
		}
		fmt.Fprintln(w, "ok")
	})
	return m
}
