package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/config"
	"github.com/dzebovski/kolss-platform-api/internal/postgres"
	"github.com/dzebovski/kolss-platform-api/internal/storage"
	"github.com/dzebovski/kolss-platform-api/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.LoadWorker()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &worker.Runner{
		Notify: &worker.Notifier{
			Pool:   pool,
			Creds:  cfg,
			Logger: logger,
			Limit:  cfg.NotifyBatchSize,
		},
		Logger:          logger,
		CleanupInterval: cfg.CleanupInterval,
		ScanInterval:    cfg.ScanInterval,
		NotifyInterval:  cfg.NotifyInterval,
	}
	if cfg.SiteJobsEnabled {
		s3, err := storage.NewS3(storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
		})
		if err != nil {
			logger.Error("storage client failed", "error", err)
			os.Exit(1)
		}
		store := worker.StorageAdapter{Inner: s3}
		runner.Cleanup = &worker.Cleanup{Pool: pool, Store: store, Logger: logger}
		runner.Scanner = &worker.Scanner{Pool: pool, Store: store, Malware: worker.NoopMalwareScanner{}, Logger: logger}
	} else {
		logger.Info("worker running in notification-only mode")
	}
	runner.Start(runCtx)

	health := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           healthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("worker health listening", "addr", cfg.HealthAddr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker health stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("worker shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	if err := health.Shutdown(shutdownCtx); err != nil {
		logger.Error("health shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("worker shut down")
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}
