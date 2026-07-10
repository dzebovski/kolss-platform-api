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

	"github.com/dzebovski/kolss-platform-api/internal/botcheck"
	"github.com/dzebovski/kolss-platform-api/internal/config"
	"github.com/dzebovski/kolss-platform-api/internal/httpapi"
	"github.com/dzebovski/kolss-platform-api/internal/leads"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
	"github.com/dzebovski/kolss-platform-api/internal/postgres"
	"github.com/dzebovski/kolss-platform-api/internal/storage"
	"github.com/dzebovski/kolss-platform-api/internal/submissions"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
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

	var objects storage.ObjectStorage
	if cfg.HasS3() {
		s3, err := storage.NewS3(storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
		})
		if err != nil {
			logger.Error("s3 client init failed", "error", err)
			os.Exit(1)
		}
		objects = s3
	} else {
		objects = storage.NewMemory("https://storage.local/presign")
		logger.Warn("using in-memory object storage; file uploads will not persist")
	}

	var bots botcheck.BotVerifier
	if cfg.BotcheckDisabled {
		bots = botcheck.DisabledVerifier{}
		logger.Warn("bot check disabled")
	} else {
		bots = botcheck.NewTurnstileVerifier(botcheck.TurnstileConfig{
			SecretKey:        cfg.TurnstileSecretKey,
			AllowedHostnames: botcheck.HostnamesSet(cfg.TurnstileAllowedHostnames),
			ExpectedAction:   cfg.TurnstileExpectedAction,
		})
	}

	sites := leads.NewRepository(pool)
	notify := notifications.Enqueuer{
		CRMSiteURLPublic:       cfg.CRMSiteURLPublic,
		TelegramBotToken:       cfg.TelegramBotToken,
		TelegramBotTokenKyiv:   cfg.TelegramBotTokenKyiv,
		TelegramBotTokenWarsaw: cfg.TelegramBotTokenWarsaw,
		TelegramChatIDKyiv:     cfg.TelegramChatIDKyiv,
		TelegramChatIDWarsaw:   cfg.TelegramChatIDWarsaw,
		SlackWebhookURLKyiv:    cfg.SlackWebhookURLKyiv,
		SlackWebhookURLWarsaw:  cfg.SlackWebhookURLWarsaw,
	}
	svc := submissions.NewService(
		pool, sites, objects, notify,
		cfg.SubmissionTokenPepper, cfg.QuarantineBucket,
		cfg.SubmissionTTL, cfg.PresignTTL,
	)

	server := httpapi.NewServer(svc, httpapi.Options{
		AllowedOrigins:     cfg.CORSAllowedOrigins,
		BodyLimitBytes:     cfg.BodyLimitBytes,
		CompleteLimitBytes: cfg.CompleteBodyLimit,
		RateLimitPerMinute: cfg.RateLimitPerMinute,
		RequireBotToken:    !cfg.BotcheckDisabled,
		BotVerifier:        bots,
		Logger:             logger,
	})

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("api shut down")
}
