package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxDeliveryAttempts = 10

type TelegramCredentials interface {
	TelegramBotTokenFor(officeCode string) string
}

type Dispatcher struct {
	Pool          notificationDatabase
	Credentials   TelegramCredentials
	Logger        *slog.Logger
	HTTP          *http.Client
	BatchSize     int
	SweepInterval time.Duration

	wake chan struct{}
}

type notificationDatabase interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewDispatcher(pool notificationDatabase, credentials TelegramCredentials, logger *slog.Logger, batchSize int, sweepInterval time.Duration) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if batchSize <= 0 {
		batchSize = 20
	}
	if sweepInterval <= 0 {
		sweepInterval = time.Hour
	}
	return &Dispatcher{
		Pool:          pool,
		Credentials:   credentials,
		Logger:        logger,
		BatchSize:     batchSize,
		SweepInterval: sweepInterval,
		wake:          make(chan struct{}, 1),
	}
}

// Wake coalesces concurrent commit notifications and never blocks a request.
func (d *Dispatcher) Wake() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run performs an immediate recovery sweep and then reacts to commits and the hourly fallback.
func (d *Dispatcher) Run(ctx context.Context) {
	d.sweep(ctx, "startup")
	ticker := time.NewTicker(d.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.log().Info("notification dispatcher stopped")
			return
		case <-d.wake:
			d.sweep(ctx, "wake")
		case <-ticker.C:
			d.sweep(ctx, "hourly")
		}
	}
}

func (d *Dispatcher) sweep(ctx context.Context, reason string) {
	started := time.Now()
	totalSent, totalFailed := 0, 0
	for ctx.Err() == nil {
		sent, failed, err := d.runOnce(ctx, reason != "wake")
		totalSent += sent
		totalFailed += failed
		if err != nil {
			d.log().Error("notification sweep failed", "reason", reason, "error", err, "sent", totalSent, "failed", totalFailed, "duration", time.Since(started))
			return
		}
		if sent+failed < d.batchSize() {
			break
		}
	}
	pending, err := d.pendingCount(ctx)
	if err != nil && ctx.Err() == nil {
		d.log().Warn("notification pending count failed", "reason", reason, "error", err)
	}
	d.log().Info("notification sweep complete", "reason", reason, "sent", totalSent, "failed", totalFailed, "pending", pending, "duration", time.Since(started))
}

func (d *Dispatcher) RunOnce(ctx context.Context) (sent int, failed int, err error) {
	return d.runOnce(ctx, true)
}

func (d *Dispatcher) runOnce(ctx context.Context, includeFailed bool) (sent int, failed int, err error) {
	client := d.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	for i := 0; i < d.batchSize(); i++ {
		outcome, err := d.claimAndSendOne(ctx, client, includeFailed)
		if err != nil {
			return sent, failed, err
		}
		switch outcome {
		case deliveryIdle:
			return sent, failed, nil
		case deliverySent:
			sent++
		case deliveryFailed:
			failed++
		}
	}
	return sent, failed, nil
}

func (d *Dispatcher) batchSize() int {
	if d.BatchSize <= 0 {
		return 20
	}
	return d.BatchSize
}

type deliveryOutcome int

const (
	deliveryIdle deliveryOutcome = iota
	deliverySent
	deliveryFailed
)

type notificationRow struct {
	ID          string
	Destination string
	Payload     map[string]any
	Attempts    int
}

func (d *Dispatcher) claimAndSendOne(ctx context.Context, client *http.Client, includeFailed bool) (deliveryOutcome, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return deliveryIdle, err
	}
	defer tx.Rollback(ctx)

	var row notificationRow
	var payload []byte
	claimToken := uuid.NewString()
	err = tx.QueryRow(ctx, `
		select id, destination, payload, attempts
		from public.lead_notifications
		where channel = 'telegram'
		  and (status = 'pending' or ($2 and status = 'failed'))
		  and attempts < $1
		  and next_attempt_at <= now()
		  and (claimed_at is null or claimed_at < now() - interval '5 minutes')
		order by created_at asc
		limit 1
		for update skip locked
	`, maxDeliveryAttempts, includeFailed).Scan(&row.ID, &row.Destination, &payload, &row.Attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deliveryIdle, nil
		}
		return deliveryIdle, err
	}
	if _, err := tx.Exec(ctx, `
		update public.lead_notifications
		set claimed_at=now(), claim_token=$2::uuid
		where id=$1::uuid
	`, row.ID, claimToken); err != nil {
		return deliveryIdle, err
	}
	if err := tx.Commit(ctx); err != nil {
		return deliveryIdle, err
	}

	row.Payload = map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &row.Payload); err != nil {
			_ = d.markFailed(ctx, row.ID, claimToken, row.Attempts+1, "decode payload: "+err.Error())
			return deliveryFailed, nil
		}
	}
	officeCode, _ := row.Payload["office_code"].(string)
	if err := d.sendTelegram(ctx, client, officeCode, row.Destination, BuildTelegramNotificationMessage(row.Payload)); err != nil {
		d.log().Warn("Telegram notification failed", "id", row.ID, "error", err)
		if markErr := d.markFailed(ctx, row.ID, claimToken, row.Attempts+1, err.Error()); markErr != nil {
			return deliveryFailed, markErr
		}
		return deliveryFailed, nil
	}
	if err := d.markSent(ctx, row.ID, claimToken, row.Attempts+1); err != nil {
		return deliveryFailed, err
	}
	return deliverySent, nil
}

func (d *Dispatcher) markSent(ctx context.Context, id, claimToken string, attempts int) error {
	_, err := d.Pool.Exec(ctx, `
		update public.lead_notifications
		set status='sent', sent_at=now(), attempts=$2, last_error=null,
		    claimed_at=null, claim_token=null
		where id=$1::uuid and claim_token=$3::uuid
	`, id, attempts, claimToken)
	return err
}

func (d *Dispatcher) markFailed(ctx context.Context, id, claimToken string, attempts int, lastError string) error {
	_, err := d.Pool.Exec(ctx, `
		update public.lead_notifications
		set status='failed', attempts=$2, last_error=$3,
		    next_attempt_at=now() + interval '1 hour', claimed_at=null, claim_token=null
		where id=$1::uuid and claim_token=$4::uuid
	`, id, attempts, truncateError(lastError, 2000), claimToken)
	return err
}

func (d *Dispatcher) pendingCount(ctx context.Context) (int, error) {
	var count int
	err := d.Pool.QueryRow(ctx, `
		select count(*)::int
		from public.lead_notifications
		where channel='telegram' and status in ('pending','failed') and attempts < $1
	`, maxDeliveryAttempts).Scan(&count)
	return count, err
}

func (d *Dispatcher) sendTelegram(ctx context.Context, client *http.Client, officeCode, destination, message string) error {
	if d.Credentials == nil {
		return errors.New("Telegram credentials are not configured")
	}
	token := d.Credentials.TelegramBotTokenFor(officeCode)
	chatID := strings.TrimSpace(destination)
	if token == "" || chatID == "" {
		return fmt.Errorf("missing Telegram config for office %q", officeCode)
	}
	return SendTelegramMessage(ctx, client, token, chatID, message)
}

// SendTelegramMessage posts a single HTML message to the Telegram Bot API.
func SendTelegramMessage(ctx context.Context, client *http.Client, token, chatID, message string) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return errors.New("missing Telegram token or chat ID")
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  chatID,
		"text":                     message,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Telegram error: status %d", response.StatusCode)
	}
	return nil
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

var sourceLabels = map[string]string{
	"meta_lead_ads": "Facebook Forms",
	"google_ads":    "Google Ads",
	"site_form":     "Site Form",
	"manual":        "Вручну",
}

func BuildTelegramNotificationMessage(payload map[string]any) string {
	escape := html.EscapeString
	lines := []string{
		"🔔 Нова заявка! " + notificationDateTime(payload),
		"👤 Ім'я: " + escape(notificationName(payload)),
	}
	structuredFields := []struct {
		key   string
		label string
	}{
		{"product_interest", "🏠 Що цікавить?: "},
		{"project_stage", "🪜 Етап проекту?: "},
		{"communication_preference", "💬 Як спілкуватися?: "},
	}
	hasStructuredInfo := false
	for _, field := range structuredFields {
		if value := strings.TrimSpace(stringify(payload[field.key])); value != "" {
			lines = append(lines, field.label+escape(value))
			hasStructuredInfo = true
		}
	}
	if !hasStructuredInfo {
		if clientInfo := strings.TrimSpace(stringify(payload["client_info"])); clientInfo != "" {
			lines = append(lines, "ℹ️ Інформація: "+escape(clientInfo))
		}
	}
	lines = append(lines,
		"📞 Тел: "+escape(notificationPhone(payload)),
		"🌐 Джерело: "+escape(notificationSourceLabel(payload)),
	)
	if crmURL := strings.TrimSpace(stringify(payload["crm_url"])); crmURL != "" {
		lines = append(lines, "🔗 <a href=\""+escape(crmURL)+"\">Відкрити в CRM</a>")
	}
	return strings.Join(lines, "\n")
}

func notificationDateTime(payload map[string]any) string {
	createdAt := time.Now().UTC()
	if raw := strings.TrimSpace(stringify(payload["created_at"])); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			createdAt = parsed
		}
	}
	locationName := "Europe/Kyiv"
	if strings.EqualFold(strings.TrimSpace(stringify(payload["office_code"])), "warsaw") {
		locationName = "Europe/Warsaw"
	}
	location, err := time.LoadLocation(locationName)
	if err != nil {
		return createdAt.Format("02.01.2006, 15:04")
	}
	return createdAt.In(location).Format("02.01.2006, 15:04")
}

func notificationName(payload map[string]any) string {
	if name := strings.TrimSpace(stringify(payload["name"])); name != "" {
		return name
	}
	return "—"
}

func notificationPhone(payload map[string]any) string {
	if phone := strings.TrimSpace(stringify(payload["phone"])); phone != "" {
		return phone
	}
	return "—"
}

func notificationSourceLabel(payload map[string]any) string {
	source := stringify(payload["source_system"])
	if label := sourceLabels[source]; label != "" {
		return label
	}
	if source == "" {
		return "—"
	}
	return source
}

func stringify(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func truncateError(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
