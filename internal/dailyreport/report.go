package dailyreport

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/leadcohorts"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

const (
	telegramGreetingLine = "🌻 Колеги доброго ранку!\nЛіди, які потребують уваги сьогодні:"
	telegramEmptyMessage = "🌻 Колеги доброго ранку!\nНаразі немає лідів, які потребують уваги. Гарного дня!"
	telegramOpenLabel    = "відкрити"

	slackGreetingLine = "🌻 Dzień dobry!\nLeady, które wymagają dzisiaj uwagi:"
	slackEmptyMessage = "🌻 Dzień dobry!\nObecnie nie ma leadów wymagających uwagi. Miłego dnia!"
	slackOpenLabel    = "otwórz"

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

// OfficeTimezone returns the IANA timezone name configured for a scheduled
// office code (e.g. "kyiv" -> "Europe/Kyiv"), and whether that office is
// known. Exported so cmd/dailyreport-preview can resolve the office-local
// report date the same way Scheduler.runForOffice does, instead of keeping
// its own copy of the office table.
func OfficeTimezone(officeCode string) (string, bool) {
	for _, def := range scheduledOffices {
		if def.code == officeCode {
			return def.tz, true
		}
	}
	return "", false
}

// OfficeChannel returns the delivery channel ("telegram" or "slack")
// configured for a scheduled office code, and whether that office is known.
func OfficeChannel(officeCode string) (string, bool) {
	for _, def := range scheduledOffices {
		if def.code == officeCode {
			return def.channel, true
		}
	}
	return "", false
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
	counts, err := leadcohorts.FetchCounts(ctx, s.Pool, leadcohorts.Params{
		OfficeCode: off.code,
		LocalDate:  reportDate,
		Timezone:   timezone,
	})
	if err != nil {
		s.log().Error("daily report cohort query failed", "office", off.code, "error", err)
		return
	}

	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	var message string
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
		message = s.FormatSlackMessage(counts, off.code, reportDate)
		destinations = []string{channelID}
		for _, destination := range destinations {
			if err := notifications.SendSlackMessage(ctx, client, token, destination, message); err != nil {
				s.log().Warn("daily report send failed", "office", off.code, "channel", channelSlack, "destination", destination, "error", err)
				continue
			}
			sent++
		}
	default:
		chatIDs := s.Chats.TelegramChatIDs(off.code)
		if len(chatIDs) == 0 {
			s.log().Warn("daily report has no chat IDs", "office", off.code)
			return
		}
		token := s.Credentials.TelegramBotTokenFor(off.code)
		message = s.FormatTelegramMessage(counts, off.code, reportDate)
		destinations = chatIDs
		for _, chatID := range destinations {
			if err := notifications.SendTelegramMessage(ctx, client, token, chatID, message); err != nil {
				s.log().Warn("daily report send failed", "office", off.code, "channel", channelTelegram, "chat_id", chatID, "error", err)
				continue
			}
			sent++
		}
	}

	s.log().Info("daily report sent",
		"office", off.code,
		"channel", off.channel,
		"date", reportDate,
		"newLeads", counts.NewLeads,
		"noAnswerOrCallbackUndated", counts.NoAnswerOrCallbackUndated,
		"callbackDueToday", counts.CallbackDueToday,
		"visitsDueToday", counts.VisitsDueToday,
		"reminderDueToday", counts.ReminderDueToday,
		"overdue", counts.Overdue,
		"destinations", len(destinations),
		"delivered", sent,
	)
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

// digestGroup pairs one leadcohorts.Group with the presentation the digest
// needs for it: emoji, the Ukrainian (Telegram) and Polish (Slack) labels,
// and the CRM deep link a manager lands on after clicking "відкрити"/"otwórz".
//
// Order matches leadcohorts.Groups, which is also the digest's display
// order (see the "Group lines and links" table in the task brief).
type digestGroup struct {
	cohort leadcohorts.Group
	emoji  string
	uk     string
	pl     string
	url    func(base, officeCode, localDate string) string
}

var digestGroups = []digestGroup{
	{
		cohort: leadcohorts.GroupNewLeads,
		emoji:  "🆕",
		uk:     "Нові заявки",
		pl:     "Nowe zgłoszenia",
		url: func(base, officeCode, _ string) string {
			return crmLeadsListURL(base, officeCode, "none", "active")
		},
	},
	{
		cohort: leadcohorts.GroupNoAnswerOrCallbackUndated,
		emoji:  "📵",
		uk:     "Недозвон + перезвон",
		pl:     "Nieodebrane i do oddzwonienia",
		url: func(base, officeCode, _ string) string {
			return crmLeadsListURL(base, officeCode, "no_answer,callback_undated", "active")
		},
	},
	{
		cohort: leadcohorts.GroupCallbackDueToday,
		emoji:  "⏰",
		uk:     "Перезвони на сьогодні",
		pl:     "Oddzwonienia na dziś",
		url: func(base, officeCode, localDate string) string {
			return crmCalendarURL(base, officeCode, localDate, "callback")
		},
	},
	{
		cohort: leadcohorts.GroupVisitsDueToday,
		emoji:  "🏠",
		uk:     "Візити в салон",
		pl:     "Wizyty w salonie",
		url: func(base, officeCode, localDate string) string {
			return crmCalendarURL(base, officeCode, localDate, "visit")
		},
	},
	{
		cohort: leadcohorts.GroupReminderDueToday,
		emoji:  "💬",
		uk:     "Інші нагадування",
		pl:     "Pozostałe przypomnienia",
		url: func(base, officeCode, localDate string) string {
			return crmCalendarURL(base, officeCode, localDate, "reminder")
		},
	},
	{
		cohort: leadcohorts.GroupOverdue,
		emoji:  "⚠️",
		uk:     "Прострочені нагадування",
		pl:     "Zaległe przypomnienia",
		url: func(base, officeCode, _ string) string {
			return crmOverdueCalendarURL(base, officeCode)
		},
	},
}

// FormatTelegramMessage renders the Kyiv digest: one line per non-empty
// group, in digestGroups order, with an HTML link. Returns
// telegramEmptyMessage when every count is zero. Exported so
// cmd/dailyreport-preview can render exactly what production would send
// without duplicating the template.
func (s *Scheduler) FormatTelegramMessage(counts leadcohorts.Counts, officeCode, localDate string) string {
	lines := make([]string, 0, len(digestGroups))
	for _, g := range digestGroups {
		count := counts.Value(g.cohort)
		if count <= 0 {
			continue
		}
		line := g.emoji + " <b>" + html.EscapeString(g.uk) + "</b> — " + strconv.Itoa(count)
		if link := g.url(s.CRMSiteURLPublic, officeCode, localDate); link != "" {
			line += " · <a href=\"" + html.EscapeString(link) + "\">" + telegramOpenLabel + "</a>"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return telegramEmptyMessage
	}
	return telegramGreetingLine + "\n\n" + strings.Join(lines, "\n")
}

// FormatSlackMessage renders the Warsaw digest: one line per non-empty
// group, in digestGroups order, with a Slack mrkdwn link. Returns
// slackEmptyMessage when every count is zero. Exported so
// cmd/dailyreport-preview can render exactly what production would send
// without duplicating the template.
func (s *Scheduler) FormatSlackMessage(counts leadcohorts.Counts, officeCode, localDate string) string {
	lines := make([]string, 0, len(digestGroups))
	for _, g := range digestGroups {
		count := counts.Value(g.cohort)
		if count <= 0 {
			continue
		}
		line := g.emoji + " *" + escapeSlackText(g.pl) + "* — " + strconv.Itoa(count)
		if link := g.url(s.CRMSiteURLPublic, officeCode, localDate); link != "" {
			line += " · <" + escapeSlackText(link) + "|" + slackOpenLabel + ">"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return slackEmptyMessage
	}
	return slackGreetingLine + "\n\n" + strings.Join(lines, "\n")
}

func escapeSlackText(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// crmLeadsListURL builds the deep link for the two lead-list groups (new
// leads; no-answer + undated callback). The query-param names/values
// (office, callStatus, clientStatus, days=all) are a fixed contract shared
// with the CRM's leads-list filters (see callStatusFilterWhere /
// clientStatusFilterWhere in internal/crmapi/leads.go) and with the
// calendar package implemented against the same contract in parallel.
func crmLeadsListURL(base, officeCode, callStatus, clientStatus string) string {
	return crmURL(base, "/crm/leads", [][2]string{
		{"office", officeCode},
		{"callStatus", callStatus},
		{"clientStatus", clientStatus},
		{"days", "all"},
	})
}

// crmCalendarURL builds the deep link for the three "due today" reminder
// groups (callback, visit, reminder), scoped to office and the office-local
// report date.
func crmCalendarURL(base, officeCode, localDate, kind string) string {
	return crmURL(base, "/crm/calendar", [][2]string{
		{"office", officeCode},
		{"date", localDate},
		{"kind", kind},
	})
}

// crmOverdueCalendarURL builds the deep link for the overdue-reminders
// group. It has no "kind" or "date" — due=overdue spans every kind and every
// date strictly before today.
func crmOverdueCalendarURL(base, officeCode string) string {
	return crmURL(base, "/crm/calendar", [][2]string{
		{"office", officeCode},
		{"due", "overdue"},
	})
}

// crmURL joins path and an ordered list of query params onto base, after
// validating base has an http/https scheme and a non-empty host. It returns
// "" when base is not a usable absolute URL, so the caller can degrade the
// digest line to plain text instead of emitting a broken link.
//
// Params are encoded in the given order (rather than via url.Values, which
// sorts keys alphabetically) so the generated URL matches the fixed
// query-param contract byte-for-byte modulo percent-encoding.
func crmURL(base, path string, params [][2]string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.Fragment = ""
	var q strings.Builder
	for i, kv := range params {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString(url.QueryEscape(kv[0]))
		q.WriteByte('=')
		q.WriteString(url.QueryEscape(kv[1]))
	}
	parsed.RawQuery = q.String()
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
