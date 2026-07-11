package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// WorkerConfig holds env for the background worker process.
// Kept separate from Load() so API-only validation (Turnstile, pepper) does not block the worker.
type WorkerConfig struct {
	DatabaseURL     string
	HealthAddr      string
	ShutdownTimeout time.Duration

	CleanupInterval time.Duration
	ScanInterval    time.Duration
	NotifyInterval  time.Duration
	NotifyBatchSize int
	SiteJobsEnabled bool

	QuarantineBucket string

	S3Endpoint        string
	S3Region          string
	S3AccessKeyID     string
	S3SecretAccessKey string

	CRMSiteURLPublic string

	TelegramBotToken       string
	TelegramBotTokenKyiv   string
	TelegramBotTokenWarsaw string
	TelegramChatIDKyiv     string
	TelegramChatIDWarsaw   string
	SlackWebhookURLKyiv    string
	SlackWebhookURLWarsaw  string
}

func LoadWorker() (WorkerConfig, error) {
	cfg := WorkerConfig{
		DatabaseURL:     strings.TrimSpace(os.Getenv("DATABASE_URL")),
		HealthAddr:      getenv("WORKER_HEALTH_ADDR", ":8081"),
		ShutdownTimeout: time.Duration(getenvInt("SHUTDOWN_TIMEOUT_SECONDS", 10)) * time.Second,

		CleanupInterval: time.Duration(getenvInt("WORKER_CLEANUP_INTERVAL_SECONDS", 60)) * time.Second,
		ScanInterval:    time.Duration(getenvInt("WORKER_SCAN_INTERVAL_SECONDS", 15)) * time.Second,
		NotifyInterval:  time.Duration(getenvInt("WORKER_NOTIFY_INTERVAL_SECONDS", 10)) * time.Second,
		NotifyBatchSize: getenvInt("WORKER_NOTIFY_BATCH_SIZE", 20),
		SiteJobsEnabled: getenvBool("WORKER_SITE_JOBS_ENABLED", false),

		QuarantineBucket: getenv("STORAGE_QUARANTINE_BUCKET", "lead-uploads-quarantine"),

		S3Endpoint:        strings.TrimSpace(os.Getenv("SUPABASE_S3_ENDPOINT")),
		S3Region:          getenv("SUPABASE_S3_REGION", "auto"),
		S3AccessKeyID:     strings.TrimSpace(os.Getenv("SUPABASE_S3_ACCESS_KEY_ID")),
		S3SecretAccessKey: strings.TrimSpace(os.Getenv("SUPABASE_S3_SECRET_ACCESS_KEY")),

		CRMSiteURLPublic: strings.TrimSpace(firstNonEmpty(
			os.Getenv("CRM_SITE_URL_PUBLIC"),
			os.Getenv("SITE_URL_PUBLIC"),
		)),

		TelegramBotToken:       strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramBotTokenKyiv:   strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN_KYIV")),
		TelegramBotTokenWarsaw: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN_WARSAW")),
		TelegramChatIDKyiv:     strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID_KYIV")),
		TelegramChatIDWarsaw:   strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID_WARSAW")),
		SlackWebhookURLKyiv:    strings.TrimSpace(os.Getenv("SLACK_WEBHOOK_URL_KYIV")),
		SlackWebhookURLWarsaw:  strings.TrimSpace(os.Getenv("SLACK_WEBHOOK_URL_WARSAW")),
	}
	if cfg.DatabaseURL == "" {
		return WorkerConfig{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.SiteJobsEnabled && (cfg.S3Endpoint == "" || cfg.S3AccessKeyID == "" || cfg.S3SecretAccessKey == "") {
		return WorkerConfig{}, fmt.Errorf("SUPABASE_S3_ENDPOINT, SUPABASE_S3_ACCESS_KEY_ID, and SUPABASE_S3_SECRET_ACCESS_KEY are required for the worker")
	}
	if cfg.CleanupInterval <= 0 || cfg.ScanInterval <= 0 || cfg.NotifyInterval <= 0 {
		return WorkerConfig{}, fmt.Errorf("worker intervals must be positive")
	}
	return cfg, nil
}

func (c WorkerConfig) HasTelegram(officeCode string) bool {
	return c.TelegramBotTokenFor(officeCode) != "" && c.TelegramChatIDFor(officeCode) != ""
}

func (c WorkerConfig) TelegramBotTokenFor(officeCode string) string {
	switch strings.ToLower(strings.TrimSpace(officeCode)) {
	case "kyiv":
		if c.TelegramBotTokenKyiv != "" {
			return c.TelegramBotTokenKyiv
		}
	case "warsaw":
		if c.TelegramBotTokenWarsaw != "" {
			return c.TelegramBotTokenWarsaw
		}
	}
	return c.TelegramBotToken
}

func (c WorkerConfig) TelegramChatIDFor(officeCode string) string {
	switch strings.ToLower(strings.TrimSpace(officeCode)) {
	case "kyiv":
		return c.TelegramChatIDKyiv
	case "warsaw":
		return c.TelegramChatIDWarsaw
	}
	return ""
}

func (c WorkerConfig) SlackWebhookFor(officeCode string) string {
	switch strings.ToLower(strings.TrimSpace(officeCode)) {
	case "warsaw":
		if c.SlackWebhookURLWarsaw != "" {
			return c.SlackWebhookURLWarsaw
		}
	case "kyiv":
		if c.SlackWebhookURLKyiv != "" {
			return c.SlackWebhookURLKyiv
		}
	}
	if c.SlackWebhookURLKyiv != "" {
		return c.SlackWebhookURLKyiv
	}
	return c.SlackWebhookURLWarsaw
}
