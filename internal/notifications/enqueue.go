package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type LeadInfo struct {
	ID                      uuid.UUID
	Name                    *string
	Phone                   *string
	Email                   *string
	ClientInfo              *string
	ProductInterest         *string
	ProjectStage            *string
	CommunicationPreference *string
	CreatedAt               *time.Time
	OfficeCode              string
	SourceSystem            string
}

type Enqueuer struct {
	CRMSiteURLPublic              string
	TelegramBotToken              string
	TelegramBotTokenKyiv          string
	TelegramBotTokenWarsaw        string
	TelegramChatIDKyiv            string
	TelegramChatIDWarsaw          string
	TelegramAdditionalChatIDsKyiv string
	SlackWebhookURLKyiv           string
	SlackWebhookURLWarsaw         string
}

func (e Enqueuer) telegramConfigured(officeCode string) bool {
	return e.telegramToken(officeCode) != "" && len(e.telegramChatIDs(officeCode)) > 0
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

func (e Enqueuer) telegramChatIDs(officeCode string) []string {
	var primary, additional string
	switch officeCode {
	case "kyiv":
		primary = e.TelegramChatIDKyiv
		additional = e.TelegramAdditionalChatIDsKyiv
	case "warsaw":
		primary = e.TelegramChatIDWarsaw
	}
	return uniqueChatIDs(primary, additional)
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
	if !e.telegramConfigured(lead.OfficeCode) && e.slackWebhook(lead.OfficeCode) == "" {
		return nil
	}

	createdAt := time.Now().UTC()
	if lead.CreatedAt != nil && !lead.CreatedAt.IsZero() {
		createdAt = lead.CreatedAt.UTC()
	}
	payload := map[string]any{
		"lead_id":                  lead.ID.String(),
		"name":                     lead.Name,
		"phone":                    lead.Phone,
		"email":                    lead.Email,
		"client_info":              lead.ClientInfo,
		"product_interest":         lead.ProductInterest,
		"project_stage":            lead.ProjectStage,
		"communication_preference": lead.CommunicationPreference,
		"created_at":               createdAt.Format(time.RFC3339),
		"source_system":            lead.SourceSystem,
		"office_code":              lead.OfficeCode,
		"crm_url":                  crmLeadURL(e.CRMSiteURLPublic, lead.ID),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	const q = `
insert into public.lead_notifications (lead_id, channel, destination, status, payload, attempts, last_error)
values ($1, $2::public.notification_channel, $3, 'pending', $4::jsonb, 0, null)
on conflict (lead_id, channel, destination) do nothing
`
	if e.telegramConfigured(lead.OfficeCode) {
		for _, chatID := range e.telegramChatIDs(lead.OfficeCode) {
			if _, err := tx.Exec(ctx, q, lead.ID, "telegram", chatID, raw); err != nil {
				return err
			}
		}
	}
	if e.slackWebhook(lead.OfficeCode) != "" {
		if _, err := tx.Exec(ctx, q, lead.ID, "slack", "", raw); err != nil {
			return err
		}
	}
	return nil
}

func uniqueChatIDs(primary, additional string) []string {
	values := append([]string{primary}, strings.Split(additional, ",")...)
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func crmLeadURL(base string, leadID uuid.UUID) *string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil
	}
	parsed.Path = fmt.Sprintf("/crm/leads/%s", leadID.String())
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	value := parsed.String()
	return &value
}
