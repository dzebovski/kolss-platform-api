package worker

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
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotifyCredentials resolves per-office Telegram/Slack secrets (same rules as Edge office-env).
type NotifyCredentials interface {
	TelegramBotTokenFor(officeCode string) string
	TelegramChatIDFor(officeCode string) string
	SlackWebhookFor(officeCode string) string
}

// Notifier claims pending/failed lead_notifications and delivers Telegram/Slack messages.
type Notifier struct {
	Pool    *pgxpool.Pool
	Creds   NotifyCredentials
	Logger  *slog.Logger
	HTTP    *http.Client
	Limit   int
	Timeout time.Duration
}

type notificationRow struct {
	ID          string
	LeadID      string
	Channel     string
	Destination string
	Payload     map[string]any
	Attempts    int
}

func (n *Notifier) RunOnce(ctx context.Context) (sent int, failed int, err error) {
	limit := n.Limit
	if limit <= 0 {
		limit = 20
	}
	client := n.HTTP
	if client == nil {
		timeout := n.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}

	for i := 0; i < limit; i++ {
		outcome, err := n.claimAndSendOne(ctx, client)
		if err != nil {
			return sent, failed, err
		}
		if outcome == notifyIdle {
			break
		}
		if outcome == notifySent {
			sent++
		} else {
			failed++
		}
	}
	return sent, failed, nil
}

type notifyOutcome int

const (
	notifyIdle notifyOutcome = iota
	notifySent
	notifyFailed
)

func (n *Notifier) claimAndSendOne(ctx context.Context, client *http.Client) (notifyOutcome, error) {
	tx, err := n.Pool.Begin(ctx)
	if err != nil {
		return notifyIdle, err
	}
	defer tx.Rollback(ctx)

	var row notificationRow
	var payload []byte
	claimToken := uuid.NewString()
	err = tx.QueryRow(ctx, `
		select id, lead_id, channel::text, destination, payload, attempts
		from public.lead_notifications
		where status in ('pending', 'failed')
		  and attempts < 10
		  and next_attempt_at <= now()
		  and (claimed_at is null or claimed_at < now() - interval '5 minutes')
		order by created_at asc
		limit 1
		for update skip locked
	`).Scan(&row.ID, &row.LeadID, &row.Channel, &row.Destination, &payload, &row.Attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notifyIdle, nil
		}
		return notifyIdle, err
	}
	if _, err := tx.Exec(ctx, `
		update public.lead_notifications
		set claimed_at=now(), claim_token=$2::uuid
		where id=$1::uuid
	`, row.ID, claimToken); err != nil {
		return notifyIdle, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notifyIdle, err
	}
	row.Payload = map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &row.Payload); err != nil {
			_ = n.markFailed(ctx, row.ID, claimToken, row.Attempts+1, "decode payload: "+err.Error())
			return notifyFailed, nil
		}
	}

	officeCode, _ := row.Payload["office_code"].(string)
	var sendErr error
	switch row.Channel {
	case "telegram":
		sendErr = n.sendTelegram(ctx, client, officeCode, row.Destination, BuildTelegramNotificationMessage(row.Payload))
	case "slack":
		sendErr = n.sendSlack(ctx, client, officeCode, BuildSlackNotificationMessage(row.Payload))
	default:
		sendErr = fmt.Errorf("unknown channel %q", row.Channel)
	}

	if sendErr != nil {
		n.log().Warn("notification send failed", "id", row.ID, "channel", row.Channel, "error", sendErr)
		if err := n.markFailed(ctx, row.ID, claimToken, row.Attempts+1, sendErr.Error()); err != nil {
			return notifyFailed, err
		}
		return notifyFailed, nil
	}

	if err := n.markSent(ctx, row.ID, claimToken, row.Attempts+1); err != nil {
		return notifyFailed, err
	}
	return notifySent, nil
}

func (n *Notifier) markSent(ctx context.Context, id, claimToken string, attempts int) error {
	_, err := n.Pool.Exec(ctx, `
		update public.lead_notifications
		set status = 'sent',
		    sent_at = now(),
		    attempts = $2,
		    last_error = null,
		    claimed_at = null,
		    claim_token = null
		where id = $1::uuid and claim_token=$3::uuid
	`, id, attempts, claimToken)
	return err
}

func (n *Notifier) markFailed(ctx context.Context, id, claimToken string, attempts int, lastError string) error {
	_, err := n.Pool.Exec(ctx, `
		update public.lead_notifications
		set status = 'failed',
		    attempts = $2,
		    last_error = $3,
		    next_attempt_at = now() + $4::interval,
		    claimed_at = null,
		    claim_token = null
		where id = $1::uuid and claim_token=$5::uuid
	`, id, attempts, truncateErr(lastError, 2000), workerRetryDelay(attempts), claimToken)
	return err
}

func (n *Notifier) sendTelegram(ctx context.Context, client *http.Client, officeCode, destination, text string) error {
	token := n.Creds.TelegramBotTokenFor(officeCode)
	chatID := strings.TrimSpace(destination)
	if chatID == "" {
		chatID = n.Creds.TelegramChatIDFor(officeCode)
	}
	if token == "" || chatID == "" {
		return fmt.Errorf("missing Telegram config for office: %s", emptyOffice(officeCode))
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	body, _ := json.Marshal(payload)
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Telegram error: status %d", res.StatusCode)
	}
	return nil
}

func (n *Notifier) sendSlack(ctx context.Context, client *http.Client, officeCode, text string) error {
	webhook := n.Creds.SlackWebhookFor(officeCode)
	if webhook == "" {
		return fmt.Errorf("missing Slack webhook for office: %s", emptyOffice(officeCode))
	}
	body, _ := json.Marshal(map[string]any{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Slack error: %d %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (n *Notifier) log() *slog.Logger {
	if n.Logger != nil {
		return n.Logger
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

func BuildSlackNotificationMessage(payload map[string]any) string {
	return buildSlackNotificationMessage(payload, func(value string) string { return value }, func(crmURL string) string {
		return "🔗 Посилання на CRM: " + crmURL
	})
}

func buildSlackNotificationMessage(payload map[string]any, escape func(string) string, crmLine func(string) string) string {
	lines := []string{
		"🔔 Нова заявка!",
		"👤 Ім'я: " + escape(notificationName(payload)),
	}
	if clientInfo := strings.TrimSpace(stringify(payload["client_info"])); clientInfo != "" {
		lines = append(lines, "", escape(clientInfo))
	}
	lines = append(lines,
		"📞 Тел: "+escape(notificationPhone(payload)),
		"🌐 Джерело: "+escape(notificationSourceLabel(payload)),
	)
	if crmURL := strings.TrimSpace(stringify(payload["crm_url"])); crmURL != "" {
		lines = append(lines, crmLine(crmURL))
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
	if sourceLabel := sourceLabels[source]; sourceLabel != "" {
		return sourceLabel
	}
	if source == "" {
		return "—"
	}
	return source
}

// BuildNotificationMessage is retained for callers that need a plain-text message.
func BuildNotificationMessage(payload map[string]any) string {
	return BuildSlackNotificationMessage(payload)
}

func workerRetryDelay(attempt int) string {
	switch attempt {
	case 1:
		return "30 seconds"
	case 2:
		return "2 minutes"
	case 3:
		return "10 minutes"
	case 4:
		return "30 minutes"
	default:
		return "2 hours"
	}
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func emptyOffice(code string) string {
	if strings.TrimSpace(code) == "" {
		return "unknown"
	}
	return code
}

func truncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
