package config

import (
	"strings"
	"testing"
)

func TestMetaConfigurationValidation(t *testing.T) {
	setBaseEnvironment(t)
	t.Setenv("META_INTEGRATION_ENABLED", "true")
	t.Setenv("META_INGEST_AFTER", "2026-07-21T18:00:00Z")
	t.Setenv("META_APP_ID", "app-id")
	t.Setenv("META_APP_SECRET", "app-secret")
	t.Setenv("META_WEBHOOK_VERIFY_TOKEN", "verify-token")
	t.Setenv("META_PAGE_ID_KYIV", "page-kyiv")
	t.Setenv("META_PAGE_ACCESS_TOKEN_KYIV", "token-kyiv")
	t.Setenv("META_PAGE_ID_WARSAW", "page-warsaw")
	t.Setenv("META_PAGE_ACCESS_TOKEN_WARSAW", "token-warsaw")
	t.Setenv("META_ALERT_TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-token")
	t.Setenv("TELEGRAM_CHAT_ID_KYIV", "kyiv-chat")
	t.Setenv("SLACK_BOT_TOKEN_WARSAW", "xoxb-warsaw")
	t.Setenv("SLACK_CHANNEL_ID_WARSAW", "C123WARSAW")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MetaIntegrationEnabled || cfg.MetaGraphAPIVersion != "v25.0" || cfg.MetaIngestAfter.IsZero() {
		t.Fatalf("unexpected Meta config: %+v", cfg)
	}
}

func TestMetaConfigurationRejectsMissingPageToken(t *testing.T) {
	setBaseEnvironment(t)
	t.Setenv("META_INTEGRATION_ENABLED", "true")
	t.Setenv("META_INGEST_AFTER", "2026-07-21T18:00:00Z")
	t.Setenv("META_APP_ID", "app-id")
	t.Setenv("META_APP_SECRET", "app-secret")
	t.Setenv("META_WEBHOOK_VERIFY_TOKEN", "verify-token")
	t.Setenv("META_PAGE_ID_KYIV", "page-kyiv")
	t.Setenv("META_PAGE_ACCESS_TOKEN_KYIV", "token-kyiv")
	t.Setenv("META_PAGE_ID_WARSAW", "page-warsaw")
	t.Setenv("META_PAGE_ACCESS_TOKEN_WARSAW", "")
	t.Setenv("META_ALERT_TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-token")
	t.Setenv("TELEGRAM_CHAT_ID_KYIV", "kyiv-chat")
	t.Setenv("SLACK_BOT_TOKEN_WARSAW", "xoxb-warsaw")
	t.Setenv("SLACK_CHANNEL_ID_WARSAW", "C123WARSAW")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "META_PAGE_ACCESS_TOKEN_WARSAW") {
		t.Fatalf("error=%v", err)
	}
}

func TestMetaConfigurationRejectsMissingWarsawSlackChannel(t *testing.T) {
	setBaseEnvironment(t)
	t.Setenv("META_INTEGRATION_ENABLED", "true")
	t.Setenv("META_INGEST_AFTER", "2026-07-21T18:00:00Z")
	t.Setenv("META_APP_ID", "app-id")
	t.Setenv("META_APP_SECRET", "app-secret")
	t.Setenv("META_WEBHOOK_VERIFY_TOKEN", "verify-token")
	t.Setenv("META_PAGE_ID_KYIV", "page-kyiv")
	t.Setenv("META_PAGE_ACCESS_TOKEN_KYIV", "token-kyiv")
	t.Setenv("META_PAGE_ID_WARSAW", "page-warsaw")
	t.Setenv("META_PAGE_ACCESS_TOKEN_WARSAW", "token-warsaw")
	t.Setenv("META_ALERT_TELEGRAM_CHAT_ID", "chat-id")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-token")
	t.Setenv("TELEGRAM_CHAT_ID_KYIV", "kyiv-chat")
	t.Setenv("SLACK_BOT_TOKEN_WARSAW", "xoxb-warsaw")
	t.Setenv("SLACK_CHANNEL_ID_WARSAW", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SLACK_CHANNEL_ID_WARSAW") {
		t.Fatalf("error=%v", err)
	}
}

func setBaseEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgresql://example")
	t.Setenv("SUPABASE_URL", "https://example.supabase.co")
	t.Setenv("PUBLIC_SITE_FORMS_ENABLED", "false")
	t.Setenv("SUPABASE_S3_ENDPOINT", "")
	t.Setenv("SUPABASE_S3_ACCESS_KEY_ID", "")
	t.Setenv("SUPABASE_S3_SECRET_ACCESS_KEY", "")
}
