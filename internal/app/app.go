package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	httpstd "net/http"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/logger"
	"github.com/atdayev/submission-triage/pkg/telemetry"
)

func Run() error {
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	cfgPath := flag.String("config", config.DefaultPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, closeLog, err := logger.SetupLogger(cfg.Log.Level, cfg.Log.Format, cfg.Service.Name,
		cfg.Log.Directory, cfg.Log.MaxAgeDays, cfg.Log.RotationHours)
	if err != nil {
		return fmt.Errorf("setup logger: %w", err)
	}
	defer func() { _ = closeLog() }()

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
