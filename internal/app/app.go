package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	httpstd "net/http"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/logger"
	"github.com/atdayev/submission-triage/pkg/telemetry"
)

// buildInfo reports the module version and VCS revision Go embeds at build time.
func buildInfo() (version, revision string) {
	version, revision = "dev", "unknown"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, revision
	}
	if bi.Main.Version != "" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	return version, revision
}

func Run() error {
	_ = godotenv.Load() // best-effort: .env into process env if present, never overrides existing vars

	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, closeLog, err := logger.SetupLogger(cfg.Log.Level, cfg.Log.Format, cfg.Service.Name,
		cfg.Log.Directory, cfg.Log.MaxAgeDays, cfg.Log.RotationHours)
	if err != nil {
		return fmt.Errorf("setup logger: %w", err)
	}
	defer func() { _ = closeLog() }()

	version, revision := buildInfo()
	log.WithField("version", version).WithField("revision", revision).Info("submission-triage starting")

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	telemetryShutdown, err := telemetry.Init(rootCtx, cfg.Service.Name, log)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			log.WithError(err).Warn("telemetry shutdown error")
		}
	}()

	metrics, err := telemetry.NewMetrics()
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}

	built, err := Build(rootCtx, cfg, log, "migrations", metrics)
	if err != nil {
		return err
	}
	defer built.DB.Close()

	if *migrateOnly {
		log.Info("migrations applied; exiting")
		return nil
	}

	server := &httpstd.Server{
		Addr:         net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.HTTP.Port)),
		Handler:      built.Router,
		ReadTimeout:  cfg.HTTP.ReadTimeout(),
		WriteTimeout: cfg.HTTP.WriteTimeout(),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.WithField("addr", server.Addr).Info("http server listening")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, httpstd.ErrServerClosed) {
			log.WithError(err).Error("http server stopped")
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		service.NewEscalationWorker(built.Service, cfg.Escalation.Interval(), log).Run(rootCtx)
	}()

	if built.Poller != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			built.Poller.Run(rootCtx)
		}()
	}

	<-rootCtx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout())
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Warn("http server shutdown error")
	}

	wg.Wait()
	built.Service.Shutdown()
	log.Info("shutdown complete")
	return nil
}
