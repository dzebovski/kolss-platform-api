package notifications

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type LeadInfo struct {
	ID           uuid.UUID
	Name         *string
	Phone        *string
	Email        *string
	OfficeCode   string
	SourceSystem string
}

type Enqueuer struct {
	CRMSiteURLPublic       string
	TelegramBotToken       string
	TelegramBotTokenKyiv   string
	TelegramBotTokenWarsaw string
	TelegramChatIDKyiv     string
	TelegramChatIDWarsaw   string
	SlackWebhookURLKyiv    string
	SlackWebhookURLWarsaw  string
}

func (e Enqueuer) channelsFor(officeCode string) []string {
	var out []string
	if e.telegramConfigured(officeCode) {
		out = append(out, "telegram")
	}
	if e.slackWebhook(officeCode) != "" {
		out = append(out, "slack")
	}
	return out
}

func (e Enqueuer) telegramConfigured(officeCode string) bool {
	return e.telegramToken(officeCode) != "" && e.telegramChatID(officeCode) != ""
}

func (e Enqueuer) telegramToken(officeCode string) string {
	switch officeCode {
	case "kyiv":
		if e.TelegramBotTokenKyiv != "" {
			return e.TelegramBotTokenKyiv
		}
	case "warsaw":
		if e.TelegramBotTokenWarsaw != "" {
			return e.TelegramBotTokenWarsaw
		}
	}
	return e.TelegramBotToken
}

func (e Enqueuer) telegramChatID(officeCode string) string {
	switch officeCode {
	case "kyiv":
		return e.TelegramChatIDKyiv
	case "warsaw":
		return e.TelegramChatIDWarsaw
	default:
		return ""
	}
}

func (e Enqueuer) slackWebhook(officeCode string) string {
	switch officeCode {
	case "kyiv":
		return e.SlackWebhookURLKyiv
	case "warsaw":
		if e.SlackWebhookURLWarsaw != "" {
			return e.SlackWebhookURLWarsaw
		}
		return e.SlackWebhookURLKyiv
	default:
		return e.SlackWebhookURLKyiv
	}
}

func (e Enqueuer) Enqueue(ctx context.Context, tx pgx.Tx, lead LeadInfo) error {
	channels := e.channelsFor(lead.OfficeCode)
	if len(channels) == 0 {
		return nil
	}

	var crmURL *string
	if e.CRMSiteURLPublic != "" {
		u := fmt.Sprintf("%s/crm/leads/%s", trimSlash(e.CRMSiteURLPublic), lead.ID.String())
		crmURL = &u
	}

	payload := map[string]any{
		"lead_id":          lead.ID.String(),
		"name":             lead.Name,
		"phone":            lead.Phone,
		"email":            lead.Email,
		"product_interest": nil,
		"source_system":    lead.SourceSystem,
		"office_code":      lead.OfficeCode,
		"crm_url":          crmURL,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	const q = `
insert into public.lead_notifications (lead_id, channel, status, payload, attempts, last_error)
values ($1, $2::public.notification_channel, 'pending', $3::jsonb, 0, null)
on conflict (lead_id, channel) do nothing
`
	for _, ch := range channels {
		if _, err := tx.Exec(ctx, q, lead.ID, ch, raw); err != nil {
			return err
		}
	}
	return nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
