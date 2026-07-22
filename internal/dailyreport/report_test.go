package dailyreport

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func strptr(s string) *string { return &s }

func timeptr(t time.Time) *time.Time { return &t }

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

func TestFormatTelegramMessagesAllStatuses(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := kyivLoc(t)
	leadNew := reportLead{ID: uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b"), Name: "Іван", Phone: "+380501112233", CallStatus: nil}
	leadNoAnswer := reportLead{ID: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Name: "Петро", Phone: "+380671112233", CallStatus: strptr("no_answer")}
	leadCallback := reportLead{ID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), Name: "Марія", Phone: "+380931112233", CallStatus: strptr("callback_requested")}

	messages := s.formatTelegramMessages([]reportLead{leadCallback, leadNew, leadNoAnswer}, nil, loc)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0]

	if !strings.HasPrefix(msg, telegramGreetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}

	newIdx := strings.Index(msg, telegramSectionNew)
	noAnswerIdx := strings.Index(msg, telegramSectionNoAnswer)
	callbackIdx := strings.Index(msg, telegramSectionCallback)
	if newIdx < 0 || noAnswerIdx < 0 || callbackIdx < 0 {
		t.Fatalf("missing section headers in message:\n%s", msg)
	}
	if !(newIdx < noAnswerIdx && noAnswerIdx < callbackIdx) {
		t.Fatalf("sections out of order: new=%d no_answer=%d callback=%d\n%s", newIdx, noAnswerIdx, callbackIdx, msg)
	}
	if strings.Contains(msg, telegramSectionReminders) {
		t.Fatalf("reminders section must be omitted when empty, got %q", msg)
	}

	wants := []string{
		"🆕 Іван, +380501112233, <a href=\"https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b\">відкрити</a>",
		"📵 Петро, +380671112233, <a href=\"https://crm.kolss.eu/crm/leads/11111111-2222-3333-4444-555555555555\">відкрити</a>",
		"📞 Марія, +380931112233, <a href=\"https://crm.kolss.eu/crm/leads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\">відкрити</a>",
	}
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestFormatTelegramMessagesOmitsDatedCallbackFromCallbackSection(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := kyivLoc(t)
	due := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	dated := reportLead{
		ID:            uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		Name:          "Марія",
		Phone:         "+380931112233",
		CallStatus:    strptr("callback_requested"),
		CallbackDueAt: timeptr(due),
	}
	undated := reportLead{
		ID:         uuid.MustParse("bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"),
		Name:       "Олег",
		Phone:      "+380501112233",
		CallStatus: strptr("callback_requested"),
	}
	reminder := dated

	msg := s.formatTelegramMessages([]reportLead{dated, undated}, []reportLead{reminder}, loc)[0]
	if !strings.Contains(msg, telegramSectionReminders) {
		t.Fatalf("expected reminders section, got %q", msg)
	}
	if !strings.Contains(msg, "⏰ Марія, +380931112233, 21.07.2026,") {
		t.Fatalf("expected dated reminder line, got %q", msg)
	}
	if !strings.Contains(msg, telegramSectionCallback) {
		t.Fatalf("expected callback section for undated lead, got %q", msg)
	}
	if !strings.Contains(msg, "📞 Олег, +380501112233,") {
		t.Fatalf("expected undated callback line, got %q", msg)
	}
	// Dated callback must not appear under the callback section emoji line without the date.
	callbackPart := msg[strings.Index(msg, telegramSectionCallback):]
	if strings.Contains(callbackPart, "Марія") {
		t.Fatalf("dated callback must not appear in callback section, got %q", callbackPart)
	}
}

func TestFormatTelegramMessagesRemindersShowTodayAndOverdueDates(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := kyivLoc(t)
	today := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	reminders := []reportLead{
		{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "Вчора", Phone: "+380111", CallbackDueAt: timeptr(yesterday)},
		{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "Сьогодні", Phone: "+380222", CallbackDueAt: timeptr(today)},
	}

	msg := s.formatTelegramMessages(nil, reminders, loc)[0]
	remindersIdx := strings.Index(msg, telegramSectionReminders)
	if remindersIdx < 0 {
		t.Fatalf("missing reminders header:\n%s", msg)
	}
	if strings.Contains(msg, telegramSectionNew) || strings.Contains(msg, telegramSectionCallback) {
		t.Fatalf("attention sections must be omitted, got %q", msg)
	}
	if !strings.Contains(msg, "⏰ Вчора, +380111, 21.07.2026,") {
		t.Fatalf("expected overdue date in line, got %q", msg)
	}
	if !strings.Contains(msg, "⏰ Сьогодні, +380222, 22.07.2026,") {
		t.Fatalf("expected today date in line, got %q", msg)
	}
	yesterdayIdx := strings.Index(msg, "⏰ Вчора,")
	todayIdx := strings.Index(msg, "⏰ Сьогодні,")
	if !(yesterdayIdx >= 0 && todayIdx > yesterdayIdx) {
		t.Fatalf("reminders must keep due-date order, got %q", msg)
	}
}

func TestFormatTelegramMessagesSectionOrderWithReminders(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := kyivLoc(t)
	due := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	attention := []reportLead{
		{ID: uuid.New(), Name: "Новий", Phone: "+3801", CallStatus: nil},
		{ID: uuid.New(), Name: "Немає", Phone: "+3802", CallStatus: strptr("no_answer")},
		{ID: uuid.New(), Name: "Передзвін", Phone: "+3803", CallStatus: strptr("callback_requested")},
	}
	reminders := []reportLead{
		{ID: uuid.New(), Name: "Нагадування", Phone: "+3804", CallbackDueAt: timeptr(due)},
	}
	msg := s.formatTelegramMessages(attention, reminders, loc)[0]
	r := strings.Index(msg, telegramSectionReminders)
	n := strings.Index(msg, telegramSectionNew)
	a := strings.Index(msg, telegramSectionNoAnswer)
	c := strings.Index(msg, telegramSectionCallback)
	if !(r >= 0 && n > r && a > n && c > a) {
		t.Fatalf("section order wrong: reminders=%d new=%d no_answer=%d callback=%d\n%s", r, n, a, c, msg)
	}
}

func TestFormatTelegramMessagesOmitsEmptySections(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	lead := reportLead{ID: uuid.New(), Name: "Іван", Phone: "+380501112233", CallStatus: nil}
	msg := s.formatTelegramMessages([]reportLead{lead}, nil, kyivLoc(t))[0]
	if strings.Contains(msg, telegramSectionNoAnswer) || strings.Contains(msg, telegramSectionCallback) || strings.Contains(msg, telegramSectionReminders) {
		t.Fatalf("empty sections must be omitted, got %q", msg)
	}
	if !strings.Contains(msg, telegramSectionNew) {
		t.Fatalf("expected new section header, got %q", msg)
	}
}

func TestFormatTelegramMessagesEscapesHTML(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	lead := reportLead{ID: uuid.New(), Name: "<b>Іван</b> & Ко", Phone: "+380501112233", CallStatus: nil}
	messages := s.formatTelegramMessages([]reportLead{lead}, nil, kyivLoc(t))
	if strings.Contains(messages[0], "<b>Іван</b>") {
		t.Fatalf("name must be HTML-escaped, got %q", messages[0])
	}
	if !strings.Contains(messages[0], "&lt;b&gt;Іван&lt;/b&gt; &amp; Ко") {
		t.Fatalf("expected escaped name, got %q", messages[0])
	}
	if !strings.Contains(messages[0], telegramSectionNew) {
		t.Fatalf("section header <b> must remain, got %q", messages[0])
	}
}

func TestFormatTelegramMessagesEmptyCase(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	messages := s.formatTelegramMessages(nil, nil, kyivLoc(t))
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0] != telegramEmptyMessage {
		t.Fatalf("expected empty message, got %q", messages[0])
	}
}

func TestFormatTelegramMessagesSplitsLongLists(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	leads := make([]reportLead, 0, 200)
	for i := 0; i < 200; i++ {
		leads = append(leads, reportLead{ID: uuid.New(), Name: "Довге ім'я клієнта номер", Phone: "+380501112233", CallStatus: nil})
	}
	messages := s.formatTelegramMessages(leads, nil, kyivLoc(t))
	if len(messages) < 2 {
		t.Fatalf("expected multiple messages for long list, got %d", len(messages))
	}
	for i, msg := range messages {
		if len(msg) > maxMessageLength+512 {
			t.Fatalf("message %d exceeds size budget: %d chars", i, len(msg))
		}
		if !strings.HasPrefix(msg, telegramGreetingLine) {
			t.Fatalf("each chunk must start with greeting, chunk %d = %q", i, msg)
		}
		if !strings.Contains(msg, telegramSectionNew) {
			t.Fatalf("each chunk with leads must repeat section header, chunk %d", i)
		}
	}
}

func TestFormatSlackMessagesAllStatuses(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := warsawLoc(t)
	leadNew := reportLead{ID: uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b"), Name: "Anna", Phone: "+48111111111", CallStatus: nil}
	leadNoAnswer := reportLead{ID: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Name: "Piotr", Phone: "+48222222222", CallStatus: strptr("no_answer")}
	leadCallback := reportLead{ID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), Name: "Maria", Phone: "+48333333333", CallStatus: strptr("callback_requested")}

	messages := s.formatSlackMessages([]reportLead{leadCallback, leadNew, leadNoAnswer}, nil, loc)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0]

	if !strings.HasPrefix(msg, slackGreetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}

	newIdx := strings.Index(msg, slackSectionNew)
	noAnswerIdx := strings.Index(msg, slackSectionNoAnswer)
	callbackIdx := strings.Index(msg, slackSectionCallback)
	if newIdx < 0 || noAnswerIdx < 0 || callbackIdx < 0 {
		t.Fatalf("missing section headers in message:\n%s", msg)
	}
	if !(newIdx < noAnswerIdx && noAnswerIdx < callbackIdx) {
		t.Fatalf("sections out of order: new=%d no_answer=%d callback=%d\n%s", newIdx, noAnswerIdx, callbackIdx, msg)
	}

	wants := []string{
		"🆕 Anna, +48111111111, <https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b|otwórz>",
		"📵 Piotr, +48222222222, <https://crm.kolss.eu/crm/leads/11111111-2222-3333-4444-555555555555|otwórz>",
		"📞 Maria, +48333333333, <https://crm.kolss.eu/crm/leads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee|otwórz>",
	}
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestFormatSlackMessagesRemindersIncludeDate(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	loc := warsawLoc(t)
	due := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	reminder := reportLead{
		ID:            uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b"),
		Name:          "Anna",
		Phone:         "+48111111111",
		CallbackDueAt: timeptr(due),
	}
	msg := s.formatSlackMessages(nil, []reportLead{reminder}, loc)[0]
	if !strings.Contains(msg, slackSectionReminders) {
		t.Fatalf("missing reminders header, got %q", msg)
	}
	want := "⏰ Anna, +48111111111, 21.07.2026, <https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b|otwórz>"
	if !strings.Contains(msg, want) {
		t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
	}
}

func TestFormatSlackMessagesEscapesMrkdwn(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	lead := reportLead{ID: uuid.New(), Name: "Anna <test> & Co", Phone: "+48111111111", CallStatus: nil}
	msg := s.formatSlackMessages([]reportLead{lead}, nil, warsawLoc(t))[0]
	if strings.Contains(msg, "Anna <test>") {
		t.Fatalf("name must be Slack-escaped, got %q", msg)
	}
	if !strings.Contains(msg, "Anna &lt;test&gt; &amp; Co") {
		t.Fatalf("expected escaped name, got %q", msg)
	}
	if !strings.Contains(msg, slackSectionNew) {
		t.Fatalf("section header must remain, got %q", msg)
	}
}

func TestFormatSlackMessagesEmptyCase(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	messages := s.formatSlackMessages(nil, nil, warsawLoc(t))
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0] != slackEmptyMessage {
		t.Fatalf("expected empty message, got %q", messages[0])
	}
}

func TestFormatSlackMessagesSplitsLongLists(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	leads := make([]reportLead, 0, 200)
	for i := 0; i < 200; i++ {
		leads = append(leads, reportLead{ID: uuid.New(), Name: "Długie imię klienta numer", Phone: "+48111111111", CallStatus: nil})
	}
	messages := s.formatSlackMessages(leads, nil, warsawLoc(t))
	if len(messages) < 2 {
		t.Fatalf("expected multiple messages for long list, got %d", len(messages))
	}
	for i, msg := range messages {
		if len(msg) > maxMessageLength+512 {
			t.Fatalf("message %d exceeds size budget: %d chars", i, len(msg))
		}
		if !strings.HasPrefix(msg, slackGreetingLine) {
			t.Fatalf("each chunk must start with greeting, chunk %d = %q", i, msg)
		}
		if !strings.Contains(msg, slackSectionNew) {
			t.Fatalf("each chunk with leads must repeat section header, chunk %d", i)
		}
	}
}

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
