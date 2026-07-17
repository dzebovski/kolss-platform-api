package dailyreport

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

const (
	greetingLine     = "Доброго ранку колеги, ось заявки на які треба звернути увагу:"
	emptyMessage     = "Доброго ранку колеги, наразі немає заявок, які потребують уваги. Гарного дня!"
	maxMessageLength = 3500
)

// ChatSource resolves the Telegram chat IDs configured for an office.
type ChatSource interface {
	TelegramChatIDs(officeCode string) []string
}

type office struct {
	code string
	loc  *time.Location
}

var scheduledOffices = []struct {
	code string
	tz   string
}{
	{code: "kyiv", tz: "Europe/Kyiv"},
	{code: "warsaw", tz: "Europe/Warsaw"},
}

// Scheduler sends a per-office morning report at a fixed local hour.
type Scheduler struct {
	Pool             *pgxpool.Pool
	Credentials      notifications.TelegramCredentials
	Chats            ChatSource
	CRMSiteURLPublic string
	HourLocal        int
	Logger           *slog.Logger
	HTTP             *http.Client
}

func New(pool *pgxpool.Pool, credentials notifications.TelegramCredentials, chats ChatSource, crmSiteURLPublic string, hourLocal int, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if hourLocal < 0 || hourLocal > 23 {
		hourLocal = 9
	}
	return &Scheduler{
		Pool:             pool,
		Credentials:      credentials,
		Chats:            chats,
		CRMSiteURLPublic: crmSiteURLPublic,
		HourLocal:        hourLocal,
		Logger:           logger,
		HTTP:             &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	offices := s.offices()
	if len(offices) == 0 {
		s.log().Error("daily report scheduler has no valid offices")
		return
	}
	for {
		now := time.Now()
		fires := make(map[string]time.Time, len(offices))
		var next time.Time
		for _, off := range offices {
			fire := nextFireTime(now, off.loc, s.HourLocal)
			fires[off.code] = fire
			if next.IsZero() || fire.Before(next) {
				next = fire
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.log().Info("daily report scheduler stopped")
			return
		case <-timer.C:
		}
		fired := time.Now()
		for _, off := range offices {
			if !fires[off.code].After(fired) {
				s.runForOffice(ctx, off)
			}
		}
	}
}

func (s *Scheduler) offices() []office {
	out := make([]office, 0, len(scheduledOffices))
	for _, def := range scheduledOffices {
		loc, err := time.LoadLocation(def.tz)
		if err != nil {
			s.log().Error("daily report timezone load failed", "office", def.code, "timezone", def.tz, "error", err)
			continue
		}
		out = append(out, office{code: def.code, loc: loc})
	}
	return out
}

func (s *Scheduler) runForOffice(ctx context.Context, off office) {
	reportDate := time.Now().In(off.loc).Format("2006-01-02")
	claimed, err := s.claim(ctx, off.code, reportDate)
	if err != nil {
		s.log().Error("daily report claim failed", "office", off.code, "date", reportDate, "error", err)
		return
	}
	if !claimed {
		s.log().Info("daily report already sent", "office", off.code, "date", reportDate)
		return
	}

	leads, err := s.fetchLeads(ctx, off.code)
	if err != nil {
		s.log().Error("daily report query failed", "office", off.code, "error", err)
		return
	}

	chatIDs := s.Chats.TelegramChatIDs(off.code)
	if len(chatIDs) == 0 {
		s.log().Warn("daily report has no chat IDs", "office", off.code)
		return
	}
	token := s.Credentials.TelegramBotTokenFor(off.code)
	messages := s.formatMessages(leads)
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	sent := 0
	for _, chatID := range chatIDs {
		for _, message := range messages {
			if err := notifications.SendTelegramMessage(ctx, client, token, chatID, message); err != nil {
				s.log().Warn("daily report send failed", "office", off.code, "chat_id", chatID, "error", err)
				continue
			}
			sent++
		}
	}
	s.log().Info("daily report sent", "office", off.code, "date", reportDate, "leads", len(leads), "messages", len(messages), "chats", len(chatIDs), "delivered", sent)
}

func (s *Scheduler) claim(ctx context.Context, officeCode, reportDate string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		insert into public.daily_report_runs (office_code, report_date)
		values ($1, $2::date)
		on conflict (office_code, report_date) do nothing
	`, officeCode, reportDate)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

type reportLead struct {
	ID         uuid.UUID
	Name       string
	Phone      string
	CallStatus *string
}

func (s *Scheduler) fetchLeads(ctx context.Context, officeCode string) ([]reportLead, error) {
	rows, err := s.Pool.Query(ctx, `
		select l.id, coalesce(l.name,''), coalesce(l.phone,''), l.call_status
		from public.leads l
		join public.offices o on o.id = l.office_id
		where l.archived_at is null
		  and o.code = $1
		  and (l.call_status is null or l.call_status in ('no_answer','callback_requested'))
		  and (l.client_status is null or l.client_status not in ('closed_lost','contract_signed'))
		order by (l.call_status is not null), coalesce(l.source_created_at, l.created_at) asc
	`, officeCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var leads []reportLead
	for rows.Next() {
		var lead reportLead
		if err := rows.Scan(&lead.ID, &lead.Name, &lead.Phone, &lead.CallStatus); err != nil {
			return nil, err
		}
		leads = append(leads, lead)
	}
	return leads, rows.Err()
}

func (s *Scheduler) formatMessages(leads []reportLead) []string {
	if len(leads) == 0 {
		return []string{emptyMessage}
	}
	var messages []string
	var builder strings.Builder
	builder.WriteString(greetingLine)
	for _, lead := range leads {
		line := s.formatLine(lead)
		if builder.Len()+1+len(line) > maxMessageLength {
			messages = append(messages, builder.String())
			builder.Reset()
			builder.WriteString(greetingLine)
		}
		builder.WriteByte('\n')
		builder.WriteString(line)
	}
	messages = append(messages, builder.String())
	return messages
}

func (s *Scheduler) formatLine(lead reportLead) string {
	name := strings.TrimSpace(lead.Name)
	if name == "" {
		name = "—"
	}
	phone := strings.TrimSpace(lead.Phone)
	if phone == "" {
		phone = "—"
	}
	line := "- " + html.EscapeString(name) + ", " + html.EscapeString(phone) + ", " + callStatusLabel(lead.CallStatus)
	if link := crmLeadURL(s.CRMSiteURLPublic, lead.ID); link != "" {
		line += ", <a href=\"" + html.EscapeString(link) + "\">Відкрити в CRM</a>"
	}
	return line
}

func callStatusLabel(status *string) string {
	if status == nil {
		return "Нова заявка"
	}
	switch *status {
	case "no_answer":
		return "Не дозвонилися"
	case "callback_requested":
		return "Передзвонити"
	default:
		return "Нова заявка"
	}
}

func crmLeadURL(base string, leadID uuid.UUID) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.Path = "/crm/leads/" + leadID.String()
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func nextFireTime(now time.Time, loc *time.Location, hourLocal int) time.Time {
	local := now.In(loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), hourLocal, 0, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func (s *Scheduler) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
