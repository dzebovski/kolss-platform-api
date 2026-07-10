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
	err = tx.QueryRow(ctx, `
		select id, lead_id, channel::text, payload, attempts
		from public.lead_notifications
		where status in ('pending', 'failed')
		  and attempts < 10
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
	row.Payload = map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &row.Payload); err != nil {
			_ = n.markFailedTx(ctx, tx, row.ID, row.Attempts+1, "decode payload: "+err.Error())
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return notifyFailed, commitErr
			}
			return notifyFailed, nil
		}
	}

	text := BuildNotificationMessage(row.Payload)
	officeCode, _ := row.Payload["office_code"].(string)
	var sendErr error
	switch row.Channel {
	case "telegram":
		sendErr = n.sendTelegram(ctx, client, officeCode, text)
	case "slack":
		sendErr = n.sendSlack(ctx, client, officeCode, text)
	default:
		sendErr = fmt.Errorf("unknown channel %q", row.Channel)
	}

	if sendErr != nil {
		n.log().Warn("notification send failed", "id", row.ID, "channel", row.Channel, "error", sendErr)
		if err := n.markFailedTx(ctx, tx, row.ID, row.Attempts+1, sendErr.Error()); err != nil {
			return notifyFailed, err
		}
		if err := tx.Commit(ctx); err != nil {
			return notifyFailed, err
		}
		return notifyFailed, nil
	}

	if err := n.markSentTx(ctx, tx, row.ID, row.Attempts+1); err != nil {
		return notifyFailed, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notifyFailed, err
	}
	return notifySent, nil
}

func (n *Notifier) markSentTx(ctx context.Context, tx pgx.Tx, id string, attempts int) error {
	_, err := tx.Exec(ctx, `
		update public.lead_notifications
		set status = 'sent',
		    sent_at = now(),
		    attempts = $2,
		    last_error = null
		where id = $1::uuid
	`, id, attempts)
	return err
}

func (n *Notifier) markFailedTx(ctx context.Context, tx pgx.Tx, id string, attempts int, lastError string) error {
	_, err := tx.Exec(ctx, `
		update public.lead_notifications
		set status = 'failed',
		    attempts = $2,
		    last_error = $3
		where id = $1::uuid
	`, id, attempts, truncateErr(lastError, 2000))
	return err
}

func (n *Notifier) sendTelegram(ctx context.Context, client *http.Client, officeCode, text string) error {
	token := n.Creds.TelegramBotTokenFor(officeCode)
	chatID := n.Creds.TelegramChatIDFor(officeCode)
	if token == "" || chatID == "" {
		return fmt.Errorf("missing Telegram config for office: %s", emptyOffice(officeCode))
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
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
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Telegram error: %d %s", res.StatusCode, strings.TrimSpace(string(respBody)))
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
		"👤 Ім'я: " + name,
		"📞 Тел: " + phone,
		"🌐 Джерело: " + sourceLabel,
	}
	if crmURL := stringify(payload["crm_url"]); crmURL != "" {
		lines = append(lines, "🔗 Посилання на CRM: "+crmURL)
	}
	return strings.Join(lines, "\n")
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
