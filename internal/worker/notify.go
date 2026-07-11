package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID       string
	LeadID   string
	Channel  string
	Payload  map[string]any
	Attempts int
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
		select id, lead_id, channel::text, payload, attempts
		from public.lead_notifications
		where status in ('pending', 'failed')
		  and attempts < 10
		  and next_attempt_at <= now()
		  and (claimed_at is null or claimed_at < now() - interval '5 minutes')
		order by created_at asc
		limit 1
		for update skip locked
	`).Scan(&row.ID, &row.LeadID, &row.Channel, &payload, &row.Attempts)
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

	text := BuildNotificationMessage(row.Payload)
	officeCode, _ := row.Payload["office_code"].(string)
	var sendErr error
	switch row.Channel {
	case "telegram":
		sendErr = n.sendTelegram(ctx, client, officeCode, text, stringify(row.Payload["crm_url"]))
	case "slack":
		sendErr = n.sendSlack(ctx, client, officeCode, text)
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

func (n *Notifier) sendTelegram(ctx context.Context, client *http.Client, officeCode, text, crmURL string) error {
	token := n.Creds.TelegramBotTokenFor(officeCode)
	chatID := n.Creds.TelegramChatIDFor(officeCode)
	if token == "" || chatID == "" {
		return fmt.Errorf("missing Telegram config for office: %s", emptyOffice(officeCode))
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if crmURL != "" {
		payload["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]string{{{
				"text": "Відкрити заявку в CRM",
				"url":  crmURL,
			}}},
		}
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

// BuildNotificationMessage matches Edge process.ts formatting.
func BuildNotificationMessage(payload map[string]any) string {
	source := stringify(payload["source_system"])
	sourceLabel := sourceLabels[source]
	if sourceLabel == "" {
		if source == "" {
			sourceLabel = "—"
		} else {
			sourceLabel = source
		}
	}
	name := stringify(payload["name"])
	if name == "" {
		name = "—"
	}
	phone := stringify(payload["phone"])
	if phone == "" {
		phone = "—"
	}
	lines := []string{
		"🔔 Нова заявка!",
		"🏢 Офіс: " + officeLabel(stringify(payload["office_code"])),
		"👤 Ім'я: " + name,
		"📞 Тел: " + phone,
		"🌐 Джерело: " + sourceLabel,
	}
	return strings.Join(lines, "\n")
}

func officeLabel(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "kyiv":
		return "Kyiv"
	case "warsaw":
		return "Warsaw"
	default:
		return "—"
	}
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
