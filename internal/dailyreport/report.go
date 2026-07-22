package dailyreport

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

const (
	maxMessageLength = 3500

	telegramGreetingLine     = "🌻 Колеги доброго ранку!\nСписок лідів на які потрібно звернути увагу:"
	telegramEmptyMessage     = "🌻 Колеги доброго ранку!\nНаразі немає лідів, які потребують уваги. Гарного дня!"
	telegramSectionReminders = "<b>Сьогоднішні нагадування:</b>"
	telegramSectionNew       = "<b>Нові заявки:</b>"
	telegramSectionNoAnswer  = "<b>Не дозволилися:</b>"
	telegramSectionCallback  = "<b>Потрібно передзвонити:</b>"
	telegramOpenLabel        = "відкрити"

	slackGreetingLine     = "🌻 Dzień dobry!\nLista leadów, na które warto zwrócić uwagę:"
	slackEmptyMessage     = "🌻 Dzień dobry!\nObecnie nie ma leadów wymagających uwagi. Miłego dnia!"
	slackSectionReminders = "*Dzisiejsze przypomnienia:*"
	slackSectionNew       = "*Nowe zgłoszenia:*"
	slackSectionNoAnswer  = "*Nieodebrane:*"
	slackSectionCallback  = "*Do oddzwonienia:*"
	slackOpenLabel        = "otwórz"

	emojiReminder = "⏰"
	emojiNew      = "🆕"
	emojiNoAnswer = "📵"
	emojiCallback = "📞"

	channelTelegram = "telegram"
	channelSlack    = "slack"
)

// ChatSource resolves delivery destinations configured for an office.
type ChatSource interface {
	TelegramChatIDs(officeCode string) []string
	SlackChannelID(officeCode string) string
}

type office struct {
	code    string
	loc     *time.Location
	channel string
}

var scheduledOffices = []struct {
	code    string
	tz      string
	channel string
	enabled bool
}{
	{code: "kyiv", tz: "Europe/Kyiv", channel: channelTelegram, enabled: true},
	{code: "warsaw", tz: "Europe/Warsaw", channel: channelSlack, enabled: true},
}

// Scheduler sends a per-office morning report at a fixed local hour.
type Scheduler struct {
	Pool             *pgxpool.Pool
	Credentials      notifications.DeliveryCredentials
	Chats            ChatSource
	CRMSiteURLPublic string
	HourLocal        int
	Logger           *slog.Logger
	HTTP             *http.Client
}

func New(pool *pgxpool.Pool, credentials notifications.DeliveryCredentials, chats ChatSource, crmSiteURLPublic string, hourLocal int, logger *slog.Logger) *Scheduler {
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
		if !def.enabled {
			continue
		}
		loc, err := time.LoadLocation(def.tz)
		if err != nil {
			s.log().Error("daily report timezone load failed", "office", def.code, "timezone", def.tz, "error", err)
			continue
		}
		out = append(out, office{code: def.code, loc: loc, channel: def.channel})
	}
	return out
}

func (s *Scheduler) runForOffice(ctx context.Context, off office) {
	nowLocal := time.Now().In(off.loc)
	if nowLocal.Weekday() == time.Sunday {
		s.log().Info("daily report skipped on Sunday", "office", off.code)
		return
	}
	reportDate := nowLocal.Format("2006-01-02")
	claimed, err := s.claim(ctx, off.code, reportDate)
	if err != nil {
		s.log().Error("daily report claim failed", "office", off.code, "date", reportDate, "error", err)
		return
	}
	if !claimed {
		s.log().Info("daily report already sent", "office", off.code, "date", reportDate)
		return
	}

	timezone := off.loc.String()
	attention, err := s.fetchLeads(ctx, off.code, timezone)
	if err != nil {
		s.log().Error("daily report query failed", "office", off.code, "error", err)
		return
	}
	reminders, err := s.fetchReminderLeads(ctx, off.code, timezone)
	if err != nil {
		s.log().Error("daily report reminders query failed", "office", off.code, "error", err)
		return
	}

	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	var messages []string
	var destinations []string
	sent := 0

	switch off.channel {
	case channelSlack:
		channelID := strings.TrimSpace(s.Chats.SlackChannelID(off.code))
		if channelID == "" {
			s.log().Warn("daily report has no Slack channel", "office", off.code)
			return
		}
		token := strings.TrimSpace(s.Credentials.SlackBotTokenFor(off.code))
		if token == "" {
			s.log().Warn("daily report missing Slack token", "office", off.code)
			return
		}
		messages = s.formatSlackMessages(attention, reminders, off.loc)
		destinations = []string{channelID}
		for _, destination := range destinations {
			for _, message := range messages {
				if err := notifications.SendSlackMessage(ctx, client, token, destination, message); err != nil {
					s.log().Warn("daily report send failed", "office", off.code, "channel", channelSlack, "destination", destination, "error", err)
					continue
				}
				sent++
			}
		}
	default:
		chatIDs := s.Chats.TelegramChatIDs(off.code)
		if len(chatIDs) == 0 {
			s.log().Warn("daily report has no chat IDs", "office", off.code)
			return
		}
		token := s.Credentials.TelegramBotTokenFor(off.code)
		messages = s.formatTelegramMessages(attention, reminders, off.loc)
		destinations = chatIDs
		for _, chatID := range destinations {
			for _, message := range messages {
				if err := notifications.SendTelegramMessage(ctx, client, token, chatID, message); err != nil {
					s.log().Warn("daily report send failed", "office", off.code, "channel", channelTelegram, "chat_id", chatID, "error", err)
					continue
				}
				sent++
			}
		}
	}

	s.log().Info("daily report sent", "office", off.code, "channel", off.channel, "date", reportDate, "leads", len(attention), "reminders", len(reminders), "messages", len(messages), "destinations", len(destinations), "delivered", sent)
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
	ID            uuid.UUID
	Name          string
	Phone         string
	CallStatus    *string
	CallbackDueAt *time.Time
}

const attentionLeadsQuery = `
	select
	  l.id,
	  coalesce(l.name,''),
	  coalesce(l.phone,''),
	  l.call_status,
	  case
	    when l.call_status is distinct from 'callback_requested' then null
	    when active_call.found then active_call.due_at
	    else l.callback_due_at
	  end as callback_due_at
	from public.leads l
	join public.offices o on o.id = l.office_id
	left join lateral (
	  select
	    true as found,
	    case
	      when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
	        then (e.new_value->>'callback_due_at')::timestamptz
	      else null
	    end as due_at
	  from public.lead_events e
	  where e.lead_id = l.id
	    and e.event_category = 'call_status'
	    and e.status_code = 'callback_requested'
	  order by e.created_at desc
	  limit 1
	) active_call on l.call_status = 'callback_requested'
	where l.archived_at is null
	  and o.code = $1
	  and (l.call_status is null or l.call_status in ('no_answer','callback_requested'))
	  and (l.client_status is null or l.client_status not in ('closed_lost','contract_signed'))
	  and (
	    l.call_status is distinct from 'callback_requested'
	    or coalesce(active_call.due_at, l.callback_due_at) is null
	    or (coalesce(active_call.due_at, l.callback_due_at) at time zone $2)::date <=
	      (now() at time zone $2)::date
	  )
	order by (l.call_status is not null), coalesce(l.source_created_at, l.created_at) asc
`

const reminderLeadCandidatesQuery = `
	select
	  l.id,
	  coalesce(l.name,''),
	  coalesce(l.phone,''),
	  l.call_status,
	  case
	    when l.call_status is distinct from 'callback_requested' then null
	    when active_call.found then active_call.due_at
	    else l.callback_due_at
	  end as call_due_at,
	  case
	    when l.client_status = 'thinking' and active_client.found then active_client.due_at
	    when l.client_status = 'thinking' then l.callback_due_at
	    when l.client_status = 'showroom_invited' then active_client.due_at
	    else null
	  end as client_due_at,
	  latest_comment.due_at as comment_due_at
	from public.leads l
	join public.offices o on o.id = l.office_id
	left join lateral (
	  select
	    true as found,
	    case
	      when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
	        then (e.new_value->>'callback_due_at')::timestamptz
	      else null
	    end as due_at
	  from public.lead_events e
	  where e.lead_id = l.id
	    and e.event_category = 'call_status'
	    and e.status_code = 'callback_requested'
	  order by e.created_at desc
	  limit 1
	) active_call on l.call_status = 'callback_requested'
	left join lateral (
	  select
	    true as found,
	    case
	      when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
	        then (e.new_value->>'callback_due_at')::timestamptz
	      else null
	    end as due_at
	  from public.lead_events e
	  where e.lead_id = l.id
	    and e.event_category = 'client_status'
	    and e.status_code = l.client_status
	  order by e.created_at desc
	  limit 1
	) active_client on l.client_status in ('thinking','showroom_invited')
	left join lateral (
	  select case
	    when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
	      then (e.new_value->>'callback_due_at')::timestamptz
	    else null
	  end as due_at
	  from public.lead_events e
	  where e.lead_id = l.id
	    and e.event_category = 'comment'
	  order by e.created_at desc
	  limit 1
	) latest_comment on true
	where l.archived_at is null
	  and o.code = $1
	  and (l.client_status is null or l.client_status not in ('closed_lost','contract_signed'))
`

func (s *Scheduler) fetchLeads(ctx context.Context, officeCode, timezone string) ([]reportLead, error) {
	rows, err := s.Pool.Query(ctx, attentionLeadsQuery, officeCode, timezone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var leads []reportLead
	for rows.Next() {
		var lead reportLead
		if err := rows.Scan(&lead.ID, &lead.Name, &lead.Phone, &lead.CallStatus, &lead.CallbackDueAt); err != nil {
			return nil, err
		}
		leads = append(leads, lead)
	}
	return leads, rows.Err()
}

func (s *Scheduler) fetchReminderLeads(ctx context.Context, officeCode, timezone string) ([]reportLead, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, reminderLeadCandidatesQuery, officeCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now()
	var leads []reportLead
	for rows.Next() {
		var lead reportLead
		var callDueAt, clientDueAt, commentDueAt *time.Time
		if err := rows.Scan(
			&lead.ID,
			&lead.Name,
			&lead.Phone,
			&lead.CallStatus,
			&callDueAt,
			&clientDueAt,
			&commentDueAt,
		); err != nil {
			return nil, err
		}
		lead.CallbackDueAt = earliestDueAt(callDueAt, clientDueAt, commentDueAt)
		if lead.CallbackDueAt == nil || !isDueOnOrBeforeLocalDate(*lead.CallbackDueAt, now, loc) {
			continue
		}
		leads = append(leads, lead)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(leads, func(i, j int) bool {
		return leads[i].CallbackDueAt.Before(*leads[j].CallbackDueAt)
	})
	return leads, nil
}

func earliestDueAt(dates ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, dueAt := range dates {
		if dueAt == nil || (earliest != nil && !dueAt.Before(*earliest)) {
			continue
		}
		value := *dueAt
		earliest = &value
	}
	return earliest
}

func isDueOnOrBeforeLocalDate(dueAt, now time.Time, loc *time.Location) bool {
	dueYear, dueMonth, dueDay := dueAt.In(loc).Date()
	nowYear, nowMonth, nowDay := now.In(loc).Date()
	dueDate := time.Date(dueYear, dueMonth, dueDay, 0, 0, 0, 0, loc)
	nowDate := time.Date(nowYear, nowMonth, nowDay, 0, 0, 0, 0, loc)
	return !dueDate.After(nowDate)
}

type leadSection struct {
	header      string
	emoji       string
	leads       []reportLead
	withDueDate bool
}

func groupAttentionBySection(leads []reportLead, newHeader, noAnswerHeader, callbackHeader string) []leadSection {
	sections := []leadSection{
		{header: newHeader, emoji: emojiNew},
		{header: noAnswerHeader, emoji: emojiNoAnswer},
		{header: callbackHeader, emoji: emojiCallback},
	}
	for _, lead := range leads {
		switch {
		case lead.CallStatus != nil && *lead.CallStatus == "no_answer":
			sections[1].leads = append(sections[1].leads, lead)
		case lead.CallStatus != nil && *lead.CallStatus == "callback_requested":
			// Dated callbacks belong only in today's reminders, not this section.
			if lead.CallbackDueAt != nil {
				continue
			}
			sections[2].leads = append(sections[2].leads, lead)
		default:
			sections[0].leads = append(sections[0].leads, lead)
		}
	}
	return sections
}

func buildReportSections(
	attention, reminders []reportLead,
	remindersHeader, newHeader, noAnswerHeader, callbackHeader string,
) []leadSection {
	sections := make([]leadSection, 0, 4)
	if len(reminders) > 0 {
		sections = append(sections, leadSection{
			header:      remindersHeader,
			emoji:       emojiReminder,
			leads:       reminders,
			withDueDate: true,
		})
	}
	sections = append(sections, groupAttentionBySection(attention, newHeader, noAnswerHeader, callbackHeader)...)
	return sections
}

func (s *Scheduler) formatTelegramMessages(attention, reminders []reportLead, loc *time.Location) []string {
	return s.formatMessages(
		attention,
		reminders,
		loc,
		telegramGreetingLine,
		telegramEmptyMessage,
		telegramSectionReminders,
		telegramSectionNew,
		telegramSectionNoAnswer,
		telegramSectionCallback,
		s.formatTelegramLine,
	)
}

func (s *Scheduler) formatSlackMessages(attention, reminders []reportLead, loc *time.Location) []string {
	return s.formatMessages(
		attention,
		reminders,
		loc,
		slackGreetingLine,
		slackEmptyMessage,
		slackSectionReminders,
		slackSectionNew,
		slackSectionNoAnswer,
		slackSectionCallback,
		s.formatSlackLine,
	)
}

type lineFormatter func(lead reportLead, emoji string, loc *time.Location, withDueDate bool) string

func (s *Scheduler) formatMessages(
	attention, reminders []reportLead,
	loc *time.Location,
	greeting, empty, remindersHeader, newHeader, noAnswerHeader, callbackHeader string,
	formatLine lineFormatter,
) []string {
	sections := buildReportSections(attention, reminders, remindersHeader, newHeader, noAnswerHeader, callbackHeader)
	hasContent := false
	for _, section := range sections {
		if len(section.leads) > 0 {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return []string{empty}
	}

	var messages []string
	var builder strings.Builder
	builder.WriteString(greeting)
	activeHeader := ""

	flush := func() {
		messages = append(messages, builder.String())
		builder.Reset()
		builder.WriteString(greeting)
		activeHeader = ""
	}

	for _, section := range sections {
		if len(section.leads) == 0 {
			continue
		}
		for _, lead := range section.leads {
			line := formatLine(lead, section.emoji, loc, section.withDueDate)
			block := leadBlock(section.header, line, activeHeader != section.header)
			if builder.Len()+len(block) > maxMessageLength && builder.Len() > len(greeting) {
				flush()
				block = leadBlock(section.header, line, true)
			}
			builder.WriteString(block)
			activeHeader = section.header
		}
	}

	messages = append(messages, builder.String())
	return messages
}

func leadBlock(header, line string, withHeader bool) string {
	var b strings.Builder
	if withHeader {
		b.WriteString("\n\n")
		b.WriteString(header)
		b.WriteString("\n")
	}
	b.WriteByte('\n')
	b.WriteString(line)
	return b.String()
}

func (s *Scheduler) formatTelegramLine(lead reportLead, emoji string, loc *time.Location, withDueDate bool) string {
	name, phone := leadDisplay(lead)
	line := emoji + " " + html.EscapeString(name) + ", " + html.EscapeString(phone)
	if withDueDate {
		if due := formatDueDate(lead.CallbackDueAt, loc); due != "" {
			line += ", " + html.EscapeString(due)
		}
	}
	if link := crmLeadURL(s.CRMSiteURLPublic, lead.ID); link != "" {
		line += ", <a href=\"" + html.EscapeString(link) + "\">" + telegramOpenLabel + "</a>"
	}
	return line
}

func (s *Scheduler) formatSlackLine(lead reportLead, emoji string, loc *time.Location, withDueDate bool) string {
	name, phone := leadDisplay(lead)
	line := emoji + " " + escapeSlackText(name) + ", " + escapeSlackText(phone)
	if withDueDate {
		if due := formatDueDate(lead.CallbackDueAt, loc); due != "" {
			line += ", " + escapeSlackText(due)
		}
	}
	if link := crmLeadURL(s.CRMSiteURLPublic, lead.ID); link != "" {
		line += ", <" + escapeSlackText(link) + "|" + slackOpenLabel + ">"
	}
	return line
}

func formatDueDate(dueAt *time.Time, loc *time.Location) string {
	if dueAt == nil {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	return dueAt.In(loc).Format("02.01.2006")
}

func leadDisplay(lead reportLead) (name, phone string) {
	name = strings.TrimSpace(lead.Name)
	if name == "" {
		name = "—"
	}
	phone = strings.TrimSpace(lead.Phone)
	if phone == "" {
		phone = "—"
	}
	return name, phone
}

func escapeSlackText(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
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
	for candidate.Weekday() == time.Sunday {
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
