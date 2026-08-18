package dailyreport

import (
	"strings"
	"testing"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/leadcohorts"
)

func kyivLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func warsawLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

// --- URL builders ---------------------------------------------------------

func TestCrmLeadsListURLBuildsFixedQueryContract(t *testing.T) {
	got := crmLeadsListURL("https://crm.kolss.eu", "warsaw", "none", "active")
	want := "https://crm.kolss.eu/crm/leads?office=warsaw&callStatus=none&clientStatus=active&days=all"
	if got != want {
		t.Fatalf("crmLeadsListURL() = %q, want %q", got, want)
	}
}

func TestCrmLeadsListURLEncodesCommaSeparatedCallStatus(t *testing.T) {
	got := crmLeadsListURL("https://crm.kolss.eu", "warsaw", "no_answer,callback_undated", "active")
	want := "https://crm.kolss.eu/crm/leads?office=warsaw&callStatus=no_answer%2Ccallback_undated&clientStatus=active&days=all"
	if got != want {
		t.Fatalf("crmLeadsListURL() = %q, want %q", got, want)
	}
}

func TestCrmCalendarURLBuildsFixedQueryContract(t *testing.T) {
	got := crmCalendarURL("https://crm.kolss.eu", "warsaw", "2026-08-17", "callback")
	want := "https://crm.kolss.eu/crm/calendar?office=warsaw&date=2026-08-17&kind=callback"
	if got != want {
		t.Fatalf("crmCalendarURL() = %q, want %q", got, want)
	}
	if got := crmCalendarURL("https://crm.kolss.eu", "kyiv", "2026-08-17", "visit"); !strings.Contains(got, "kind=visit") {
		t.Fatalf("crmCalendarURL() = %q, want kind=visit", got)
	}
	if got := crmCalendarURL("https://crm.kolss.eu", "kyiv", "2026-08-17", "reminder"); !strings.Contains(got, "kind=reminder") {
		t.Fatalf("crmCalendarURL() = %q, want kind=reminder", got)
	}
}

func TestCrmOverdueCalendarURLHasNoDateOrKind(t *testing.T) {
	got := crmOverdueCalendarURL("https://crm.kolss.eu", "kyiv")
	want := "https://crm.kolss.eu/crm/calendar?office=kyiv&due=overdue"
	if got != want {
		t.Fatalf("crmOverdueCalendarURL() = %q, want %q", got, want)
	}
}

func TestCrmURLDegradesToEmptyOnInvalidBase(t *testing.T) {
	for _, base := range []string{"", "not a url", "ftp://crm.kolss.eu", "https://", "  "} {
		if got := crmLeadsListURL(base, "warsaw", "none", "active"); got != "" {
			t.Fatalf("crmLeadsListURL(%q) = %q, want empty string", base, got)
		}
		if got := crmCalendarURL(base, "warsaw", "2026-08-17", "visit"); got != "" {
			t.Fatalf("crmCalendarURL(%q) = %q, want empty string", base, got)
		}
		if got := crmOverdueCalendarURL(base, "warsaw"); got != "" {
			t.Fatalf("crmOverdueCalendarURL(%q) = %q, want empty string", base, got)
		}
	}
}

// --- Message composition ---------------------------------------------------

func TestFormatTelegramMessageAllGroups(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	counts := leadcohorts.Counts{
		NewLeads:                  12,
		NoAnswerOrCallbackUndated: 8,
		CallbackDueToday:          3,
		VisitsDueToday:            2,
		ReminderDueToday:          5,
		Overdue:                   1,
	}
	msg := s.FormatTelegramMessage(counts, "warsaw", "2026-08-17")

	if !strings.HasPrefix(msg, telegramGreetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}
	if strings.Contains(msg, "\n\n\n") {
		t.Fatalf("expected exactly one blank line after the greeting, got %q", msg)
	}

	wants := []string{
		`🆕 <b>Нові заявки</b> — 12 · <a href="https://crm.kolss.eu/crm/leads?office=warsaw&amp;callStatus=none&amp;clientStatus=active&amp;days=all">відкрити</a>`,
		`📵 <b>Недозвон + перезвон</b> — 8 · <a href="https://crm.kolss.eu/crm/leads?office=warsaw&amp;callStatus=no_answer%2Ccallback_undated&amp;clientStatus=active&amp;days=all">відкрити</a>`,
		`⏰ <b>Перезвони на сьогодні</b> — 3 · <a href="https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=callback">відкрити</a>`,
		`🏠 <b>Візити в салон</b> — 2 · <a href="https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=visit">відкрити</a>`,
		`💬 <b>Інші нагадування</b> — 5 · <a href="https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=reminder">відкрити</a>`,
		`⚠️ <b>Прострочені нагадування</b> — 1 · <a href="https://crm.kolss.eu/crm/calendar?office=warsaw&amp;due=overdue">відкрити</a>`,
	}
	lastIdx := -1
	for _, want := range wants {
		idx := strings.Index(msg, want)
		if idx < 0 {
			t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
		}
		if idx <= lastIdx {
			t.Fatalf("group lines out of digestGroups order at %q\nfull message:\n%s", want, msg)
		}
		lastIdx = idx
	}
}

func TestFormatTelegramMessageOmitsZeroGroups(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	msg := s.FormatTelegramMessage(leadcohorts.Counts{NewLeads: 5}, "kyiv", "2026-08-17")
	if !strings.Contains(msg, "🆕 <b>Нові заявки</b> — 5") {
		t.Fatalf("expected new leads line, got %q", msg)
	}
	for _, label := range []string{
		"Недозвон + перезвон", "Перезвони на сьогодні", "Візити в салон",
		"Інші нагадування", "Прострочені нагадування",
	} {
		if strings.Contains(msg, label) {
			t.Fatalf("zero-count group %q must be omitted, got %q", label, msg)
		}
	}
}

func TestFormatTelegramMessageEmptyWhenAllZero(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	msg := s.FormatTelegramMessage(leadcohorts.Counts{}, "kyiv", "2026-08-17")
	if msg != telegramEmptyMessage {
		t.Fatalf("expected empty message, got %q", msg)
	}
}

func TestFormatTelegramMessageDegradesToTextWithoutLink(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: ""}
	msg := s.FormatTelegramMessage(leadcohorts.Counts{NewLeads: 3}, "kyiv", "2026-08-17")
	if strings.Contains(msg, "<a href") {
		t.Fatalf("expected no link when base URL is invalid, got %q", msg)
	}
	if !strings.Contains(msg, "🆕 <b>Нові заявки</b> — 3") {
		t.Fatalf("expected line without link, got %q", msg)
	}
}

func TestFormatSlackMessageAllGroups(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	counts := leadcohorts.Counts{
		NewLeads:                  12,
		NoAnswerOrCallbackUndated: 8,
		CallbackDueToday:          3,
		VisitsDueToday:            2,
		ReminderDueToday:          5,
		Overdue:                   1,
	}
	msg := s.FormatSlackMessage(counts, "warsaw", "2026-08-17")

	if !strings.HasPrefix(msg, slackGreetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}

	wants := []string{
		`🆕 *Nowe zgłoszenia* — 12 · <https://crm.kolss.eu/crm/leads?office=warsaw&amp;callStatus=none&amp;clientStatus=active&amp;days=all|otwórz>`,
		`📵 *Nieodebrane i do oddzwonienia* — 8 · <https://crm.kolss.eu/crm/leads?office=warsaw&amp;callStatus=no_answer%2Ccallback_undated&amp;clientStatus=active&amp;days=all|otwórz>`,
		`⏰ *Oddzwonienia na dziś* — 3 · <https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=callback|otwórz>`,
		`🏠 *Wizyty w salonie* — 2 · <https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=visit|otwórz>`,
		`💬 *Pozostałe przypomnienia* — 5 · <https://crm.kolss.eu/crm/calendar?office=warsaw&amp;date=2026-08-17&amp;kind=reminder|otwórz>`,
		`⚠️ *Zaległe przypomnienia* — 1 · <https://crm.kolss.eu/crm/calendar?office=warsaw&amp;due=overdue|otwórz>`,
	}
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestFormatSlackMessageEmptyWhenAllZero(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	msg := s.FormatSlackMessage(leadcohorts.Counts{}, "warsaw", "2026-08-17")
	if msg != slackEmptyMessage {
		t.Fatalf("expected empty message, got %q", msg)
	}
}

func TestFormatSlackMessageEscapesMrkdwnInLink(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	msg := s.FormatSlackMessage(leadcohorts.Counts{NewLeads: 1}, "warsaw", "2026-08-17")
	if strings.Contains(msg, "?office=warsaw&callStatus") {
		t.Fatalf("raw unescaped '&' must not appear in Slack link, got %q", msg)
	}
}

// TestFormatMessageSixGroupsWithinTelegramLimit is the replacement for the
// old multi-message-splitting behaviour: with one short line per group and
// no per-lead detail, a full six-group digest can never approach Telegram's
// 4096-character hard limit, so splitting into multiple messages is no
// longer needed.
func TestFormatMessageSixGroupsWithinTelegramLimit(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	counts := leadcohorts.Counts{
		NewLeads:                  999,
		NoAnswerOrCallbackUndated: 999,
		CallbackDueToday:          999,
		VisitsDueToday:            999,
		ReminderDueToday:          999,
		Overdue:                   999,
	}
	const telegramHardLimit = 4096
	if msg := s.FormatTelegramMessage(counts, "warsaw", "2026-08-17"); len(msg) >= telegramHardLimit/2 {
		t.Fatalf("telegram six-group message is %d chars, want comfortably under half of Telegram's %d-char limit\n%s", len(msg), telegramHardLimit, msg)
	}
	if msg := s.FormatSlackMessage(counts, "warsaw", "2026-08-17"); len(msg) >= telegramHardLimit/2 {
		t.Fatalf("slack six-group message is %d chars, want comfortably under half of Telegram's %d-char limit\n%s", len(msg), telegramHardLimit, msg)
	}
}

// --- Schedule (unchanged behaviour, still covered) --------------------------

func TestNextFireTimeBeforeHour(t *testing.T) {
	loc := kyivLoc(t)
	// Friday before hour → same day
	now := time.Date(2026, time.July, 17, 6, 30, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 17, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimePastHourRollsToNextDay(t *testing.T) {
	loc := kyivLoc(t)
	// Friday after hour → Saturday
	now := time.Date(2026, time.July, 17, 10, 15, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 18, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimeExactlyAtHourRollsToNextDay(t *testing.T) {
	loc := warsawLoc(t)
	now := time.Date(2026, time.July, 17, 9, 0, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 18, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimeSaturdayAfterHourSkipsToMonday(t *testing.T) {
	loc := kyivLoc(t)
	// Saturday 2026-07-18 after hour → Monday 2026-07-20
	now := time.Date(2026, time.July, 18, 10, 0, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 20, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Fatalf("expected Monday, got %s", got.Weekday())
	}
}

func TestNextFireTimeSundaySkipsToMonday(t *testing.T) {
	loc := kyivLoc(t)
	// Sunday 2026-07-19 before hour → Monday 2026-07-20
	now := time.Date(2026, time.July, 19, 6, 0, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 20, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestOfficesIncludesKyivAndWarsaw(t *testing.T) {
	for _, def := range scheduledOffices {
		if !def.enabled {
			t.Fatalf("office %q must be enabled", def.code)
		}
	}

	s := &Scheduler{}
	active := s.offices()
	found := map[string]string{}
	for _, off := range active {
		found[off.code] = off.channel
	}
	if found["kyiv"] != channelTelegram {
		t.Fatalf("kyiv channel = %q, want %q", found["kyiv"], channelTelegram)
	}
	if found["warsaw"] != channelSlack {
		t.Fatalf("warsaw channel = %q, want %q", found["warsaw"], channelSlack)
	}
}
