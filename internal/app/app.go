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
	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/service"
	"github.com/atdayev/submission-triage/pkg/logger"
)

const (
	httpReadHeaderTimeout = 5 * time.Second // tighter slow-header bound than the full read timeout
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
)

// buildInfo reports version and VCS revision from the embedded build info.
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

// Run loads config, builds the app, and serves until shutdown.
func Run() error {
	_ = godotenv.Load() // best-effort; never overrides existing env vars

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

	built, err := Build(rootCtx, cfg, log, "migrations")
	if err != nil {
		return err
	}
	defer built.DB.Close()

	if *migrateOnly {
		built.Service.Shutdown()
		log.Info("migrations applied; exiting")
		return nil
	}

	var wg sync.WaitGroup
	server, listenErr := startHTTPServer(cfg, built.Router, log, &wg, cancel)
	startWorkers(rootCtx, built, cfg, log, &wg)
	awaitShutdownAndStop(rootCtx, server, built.Service, cfg, &wg, log)

	// a fatal listen failure must exit non-zero, not 0
	select {
	case err := <-listenErr:
		return fmt.Errorf("http server failed: %w", err)
	default:
		return nil
	}
}

// startHTTPServer builds the http.Server and launches its listen goroutine.
func startHTTPServer(cfg *config.Config, router httpstd.Handler, log *logrus.Entry, wg *sync.WaitGroup, cancel context.CancelFunc) (*httpstd.Server, <-chan error) {
	server := &httpstd.Server{
		Addr:              net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.HTTP.Port)),
		Handler:           router,
		ReadTimeout:       cfg.HTTP.ReadTimeout(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout(),
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}

	// buffered so a fatal listen error survives for Run to read after wg.Wait
	listenErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.WithField("addr", server.Addr).Info("http server listening")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, httpstd.ErrServerClosed) {
			log.WithError(err).Error("http server stopped")
			listenErr <- err
			cancel()
		}
	}()

	return server, listenErr
}

// startWorkers launches the escalation worker and, if configured, the IMAP poller.
func startWorkers(rootCtx context.Context, built *BuiltApp, cfg *config.Config, log *logrus.Entry, wg *sync.WaitGroup) {
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
}

// awaitShutdownAndStop blocks until rootCtx is done, then gracefully stops the server and service.
func awaitShutdownAndStop(rootCtx context.Context, server *httpstd.Server, svc *service.SubmissionsService, cfg *config.Config, wg *sync.WaitGroup, log *logrus.Entry) {
	<-rootCtx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout())
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Warn("http server shutdown error")
	}

	wg.Wait()
	svc.Shutdown()
	log.Info("shutdown complete")
}
