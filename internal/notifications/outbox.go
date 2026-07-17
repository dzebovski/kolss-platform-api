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

type Waker interface {
	Wake()
}

// Outbox writes Telegram delivery tasks in the same transaction as a lead.
type Outbox struct {
	CRMSiteURLPublic              string
	TelegramChatIDKyiv            string
	TelegramChatIDWarsaw          string
	TelegramAdditionalChatIDsKyiv string
}

// TelegramChatIDs returns the Telegram chat IDs configured for an office.
func (o Outbox) TelegramChatIDs(officeCode string) []string {
	var primary, additional string
	switch officeCode {
	case "kyiv":
		primary = o.TelegramChatIDKyiv
		additional = o.TelegramAdditionalChatIDsKyiv
	case "warsaw":
		primary = o.TelegramChatIDWarsaw
	}
	return uniqueChatIDs(primary, additional)
}

func (o Outbox) telegramChatIDs(officeCode string) []string {
	return o.TelegramChatIDs(officeCode)
}

func (o Outbox) Enqueue(ctx context.Context, tx pgx.Tx, lead LeadInfo) error {
	chatIDs := o.telegramChatIDs(lead.OfficeCode)
	if len(chatIDs) == 0 {
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
		"crm_url":                  crmLeadURL(o.CRMSiteURLPublic, lead.ID),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	const query = `
insert into public.lead_notifications (lead_id, channel, destination, status, payload, attempts, last_error)
values ($1, 'telegram'::public.notification_channel, $2, 'pending', $3::jsonb, 0, null)
on conflict (lead_id, channel, destination) do nothing
`
	for _, chatID := range chatIDs {
		if _, err := tx.Exec(ctx, query, lead.ID, chatID, raw); err != nil {
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
