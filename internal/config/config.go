package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr               string
	DatabaseURL            string
	CORSAllowedOrigins     []string
	BodyLimitBytes         int64
	CompleteBodyLimit      int64
	RateLimitPerMinute     int
	ShutdownTimeout        time.Duration
	PublicSiteFormsEnabled bool

	SupabaseURL        string
	SupabaseJWKSURL    string
	SupabaseJWTIssuer  string
	SupabaseSecretKey  string
	ImportSecretKyiv   string
	ImportSecretWarsaw string
	ImportBodyLimit    int64

	SubmissionTokenPepper string
	SubmissionTTL         time.Duration
	PresignTTL            time.Duration
	QuarantineBucket      string

	BotcheckDisabled          bool
	TurnstileSecretKey        string
	TurnstileAllowedHostnames []string
	TurnstileExpectedAction   string

	S3Endpoint        string
	S3Region          string
	S3AccessKeyID     string
	S3SecretAccessKey string

	CRMSiteURLPublic string

	TelegramBotToken              string
	TelegramBotTokenKyiv          string
	TelegramBotTokenWarsaw        string
	TelegramChatIDKyiv            string
	TelegramChatIDWarsaw          string
	TelegramAdditionalChatIDsKyiv string
	SlackWebhookURLKyiv           string
	SlackWebhookURLWarsaw         string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		CORSAllowedOrigins: splitCSV(getenv(
			"CORS_ALLOWED_ORIGINS",
			"http://localhost:4200,http://localhost:4201,http://127.0.0.1:4200,http://127.0.0.1:4201",
		)),
		BodyLimitBytes:         int64(getenvInt("BODY_LIMIT_BYTES", 64*1024)),
		CompleteBodyLimit:      int64(getenvInt("COMPLETE_BODY_LIMIT_BYTES", 16*1024)),
		RateLimitPerMinute:     getenvInt("RATE_LIMIT_PER_MINUTE", 30),
		ShutdownTimeout:        time.Duration(getenvInt("SHUTDOWN_TIMEOUT_SECONDS", 10)) * time.Second,
		PublicSiteFormsEnabled: getenvBool("PUBLIC_SITE_FORMS_ENABLED", false),

		SupabaseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("SUPABASE_URL")), "/"),
		SupabaseJWKSURL:    strings.TrimSpace(os.Getenv("SUPABASE_JWKS_URL")),
		SupabaseJWTIssuer:  strings.TrimSpace(os.Getenv("SUPABASE_JWT_ISSUER")),
		SupabaseSecretKey:  strings.TrimSpace(os.Getenv("SUPABASE_SECRET_KEY")),
		ImportSecretKyiv:   strings.TrimSpace(os.Getenv("GOOGLE_SHEETS_IMPORT_SECRET_KYIV")),
		ImportSecretWarsaw: strings.TrimSpace(os.Getenv("GOOGLE_SHEETS_IMPORT_SECRET_WARSAW")),
		ImportBodyLimit:    int64(getenvInt("IMPORT_BODY_LIMIT_BYTES", 512*1024)),

		SubmissionTokenPepper: strings.TrimSpace(os.Getenv("SUBMISSION_TOKEN_PEPPER")),
		SubmissionTTL:         time.Duration(getenvInt("SUBMISSION_TTL_MINUTES", 60)) * time.Minute,
		PresignTTL:            time.Duration(getenvInt("PRESIGN_TTL_MINUTES", 10)) * time.Minute,
		QuarantineBucket:      getenv("STORAGE_QUARANTINE_BUCKET", "lead-uploads-quarantine"),

		BotcheckDisabled:          getenvBool("BOTCHECK_DISABLED", false),
		TurnstileSecretKey:        strings.TrimSpace(os.Getenv("TURNSTILE_SECRET_KEY")),
		TurnstileAllowedHostnames: splitCSV(os.Getenv("TURNSTILE_ALLOWED_HOSTNAMES")),
		TurnstileExpectedAction:   getenv("TURNSTILE_EXPECTED_ACTION", "lead_submission"),

		S3Endpoint:        strings.TrimSpace(os.Getenv("SUPABASE_S3_ENDPOINT")),
		S3Region:          getenv("SUPABASE_S3_REGION", "auto"),
		S3AccessKeyID:     strings.TrimSpace(os.Getenv("SUPABASE_S3_ACCESS_KEY_ID")),
		S3SecretAccessKey: strings.TrimSpace(os.Getenv("SUPABASE_S3_SECRET_ACCESS_KEY")),

		CRMSiteURLPublic: strings.TrimSpace(firstNonEmpty(
			os.Getenv("CRM_SITE_URL_PUBLIC"),
			os.Getenv("SITE_URL_PUBLIC"),
		)),

		TelegramBotToken:              strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramBotTokenKyiv:          strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN_KYIV")),
		TelegramBotTokenWarsaw:        strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN_WARSAW")),
		TelegramChatIDKyiv:            strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID_KYIV")),
		TelegramChatIDWarsaw:          strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID_WARSAW")),
		TelegramAdditionalChatIDsKyiv: strings.TrimSpace(os.Getenv("TELEGRAM_ADDITIONAL_CHAT_IDS_KYIV")),
		SlackWebhookURLKyiv:           strings.TrimSpace(os.Getenv("SLACK_WEBHOOK_URL_KYIV")),
		SlackWebhookURLWarsaw:         strings.TrimSpace(os.Getenv("SLACK_WEBHOOK_URL_WARSAW")),
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.PublicSiteFormsEnabled && cfg.SubmissionTokenPepper == "" {
		return Config{}, fmt.Errorf("SUBMISSION_TOKEN_PEPPER is required")
	}
	if cfg.PublicSiteFormsEnabled && !cfg.BotcheckDisabled && cfg.TurnstileSecretKey == "" {
		return Config{}, fmt.Errorf("TURNSTILE_SECRET_KEY is required unless BOTCHECK_DISABLED=true")
	}
	if cfg.PublicSiteFormsEnabled && !cfg.BotcheckDisabled && len(cfg.TurnstileAllowedHostnames) == 0 {
		return Config{}, fmt.Errorf("TURNSTILE_ALLOWED_HOSTNAMES is required unless BOTCHECK_DISABLED=true")
	}
	if cfg.HasS3() {
		if cfg.S3Endpoint == "" || cfg.S3AccessKeyID == "" || cfg.S3SecretAccessKey == "" {
			return Config{}, fmt.Errorf("SUPABASE_S3_ENDPOINT, SUPABASE_S3_ACCESS_KEY_ID, and SUPABASE_S3_SECRET_ACCESS_KEY are required together")
		}
	} else if cfg.PublicSiteFormsEnabled && !cfg.BotcheckDisabled {
		return Config{}, fmt.Errorf("S3 storage credentials are required unless BOTCHECK_DISABLED=true")
	}
	if cfg.SupabaseURL == "" {
		return Config{}, fmt.Errorf("SUPABASE_URL is required")
	}
	if cfg.SupabaseJWKSURL == "" {
		cfg.SupabaseJWKSURL = cfg.SupabaseURL + "/auth/v1/.well-known/jwks.json"
	}
	if cfg.SupabaseJWTIssuer == "" {
		cfg.SupabaseJWTIssuer = cfg.SupabaseURL + "/auth/v1"
	}
	return cfg, nil
}

func (c Config) HasS3() bool {
	return c.S3Endpoint != "" || c.S3AccessKeyID != "" || c.S3SecretAccessKey != ""
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
