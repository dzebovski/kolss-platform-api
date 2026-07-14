package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	platformauth "github.com/dzebovski/kolss-platform-api/internal/auth"
	"github.com/dzebovski/kolss-platform-api/internal/botcheck"
	"github.com/dzebovski/kolss-platform-api/internal/config"
	"github.com/dzebovski/kolss-platform-api/internal/crmapi"
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
		objects = storage.NilStorage{}
		logger.Warn("object storage disabled")
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
	outbox := notifications.Outbox{
		CRMSiteURLPublic:              cfg.CRMSiteURLPublic,
		TelegramChatIDKyiv:            cfg.TelegramChatIDKyiv,
		TelegramChatIDWarsaw:          cfg.TelegramChatIDWarsaw,
		TelegramAdditionalChatIDsKyiv: cfg.TelegramAdditionalChatIDsKyiv,
	}
	var dispatcher *notifications.Dispatcher
	var notificationWaker notifications.Waker
	dispatcherCtx, cancelDispatcher := context.WithCancel(context.Background())
	var dispatcherWG sync.WaitGroup
	if cfg.NotificationDispatcherEnabled {
		dispatcher = notifications.NewDispatcher(
			pool,
			cfg,
			logger,
			cfg.NotificationBatchSize,
			cfg.NotificationSweepInterval,
		)
		notificationWaker = dispatcher
		dispatcherWG.Add(1)
		go func() {
			defer dispatcherWG.Done()
			dispatcher.Run(dispatcherCtx)
		}()
	} else {
		logger.Warn("notification dispatcher disabled")
	}
	svc := submissions.NewService(pool, sites, outbox, notificationWaker)

	server := httpapi.NewServer(svc, httpapi.Options{
		Enabled:            cfg.PublicSiteFormsEnabled,
		AllowedOrigins:     cfg.CORSAllowedOrigins,
		BodyLimitBytes:     cfg.BodyLimitBytes,
		RateLimitPerMinute: cfg.RateLimitPerMinute,
		RequireBotToken:    !cfg.BotcheckDisabled,
		BotVerifier:        bots,
		Logger:             logger,
	})
	verifier := &platformauth.Verifier{
		JWKSURL:  cfg.SupabaseJWKSURL,
		Issuer:   cfg.SupabaseJWTIssuer,
		Audience: "authenticated",
	}
	crm := crmapi.New(crmapi.Options{
		Pool:               pool,
		Verifier:           verifier,
		AllowedOrigins:     cfg.CORSAllowedOrigins,
		ImportSecretKyiv:   cfg.ImportSecretKyiv,
		ImportSecretWarsaw: cfg.ImportSecretWarsaw,
		ImportBodyLimit:    cfg.ImportBodyLimit,
		SupabaseURL:        cfg.SupabaseURL,
		SupabaseSecretKey:  cfg.SupabaseSecretKey,
		CRMSiteURLPublic:   cfg.CRMSiteURLPublic,
		Outbox:             outbox,
		NotificationWaker:  notificationWaker,
		Storage:            objects,
		Logger:             logger,
	})
	root := buildRouter(server, crm)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
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
		cancelDispatcher()
		dispatcherWG.Wait()
		os.Exit(1)
	}
	cancelDispatcher()
	dispatcherWG.Wait()
	logger.Info("api shut down")
}

func buildRouter(public *httpapi.Server, crm *crmapi.Server) http.Handler {
	router := chi.NewRouter()
	public.RegisterRoutes(router)
	crm.RegisterRoutes(router)
	return router
}
