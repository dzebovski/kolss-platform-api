package dailyreport

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func strptr(s string) *string { return &s }

func TestFormatMessagesAllStatuses(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	leadNew := reportLead{ID: uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b"), Name: "Іван", Phone: "+380501112233", CallStatus: nil}
	leadNoAnswer := reportLead{ID: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Name: "Петро", Phone: "+380671112233", CallStatus: strptr("no_answer")}
	leadCallback := reportLead{ID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), Name: "Марія", Phone: "+380931112233", CallStatus: strptr("callback_requested")}

	messages := s.formatMessages([]reportLead{leadCallback, leadNew, leadNoAnswer})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0]

	if !strings.HasPrefix(msg, greetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}

	newIdx := strings.Index(msg, sectionNewHeader)
	noAnswerIdx := strings.Index(msg, sectionNoAnswerHeader)
	callbackIdx := strings.Index(msg, sectionCallbackHeader)
	if newIdx < 0 || noAnswerIdx < 0 || callbackIdx < 0 {
		t.Fatalf("missing section headers in message:\n%s", msg)
	}
	if !(newIdx < noAnswerIdx && noAnswerIdx < callbackIdx) {
		t.Fatalf("sections out of order: new=%d no_answer=%d callback=%d\n%s", newIdx, noAnswerIdx, callbackIdx, msg)
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

func TestFormatMessagesOmitsEmptySections(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	lead := reportLead{ID: uuid.New(), Name: "Іван", Phone: "+380501112233", CallStatus: nil}
	msg := s.formatMessages([]reportLead{lead})[0]
	if strings.Contains(msg, sectionNoAnswerHeader) || strings.Contains(msg, sectionCallbackHeader) {
		t.Fatalf("empty sections must be omitted, got %q", msg)
	}
	if !strings.Contains(msg, sectionNewHeader) {
		t.Fatalf("expected new section header, got %q", msg)
	}
}

func TestFormatMessagesEscapesHTML(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	lead := reportLead{ID: uuid.New(), Name: "<b>Іван</b> & Ко", Phone: "+380501112233", CallStatus: nil}
	messages := s.formatMessages([]reportLead{lead})
	if strings.Contains(messages[0], "<b>Іван</b>") {
		t.Fatalf("name must be HTML-escaped, got %q", messages[0])
	}
	if !strings.Contains(messages[0], "&lt;b&gt;Іван&lt;/b&gt; &amp; Ко") {
		t.Fatalf("expected escaped name, got %q", messages[0])
	}
	if !strings.Contains(messages[0], sectionNewHeader) {
		t.Fatalf("section header <b> must remain, got %q", messages[0])
	}
}

func TestFormatMessagesEmptyCase(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	messages := s.formatMessages(nil)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0] != emptyMessage {
		t.Fatalf("expected empty message, got %q", messages[0])
	}
}

func TestFormatMessagesSplitsLongLists(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	leads := make([]reportLead, 0, 200)
	for i := 0; i < 200; i++ {
		leads = append(leads, reportLead{ID: uuid.New(), Name: "Довге ім'я клієнта номер", Phone: "+380501112233", CallStatus: nil})
	}
	messages := s.formatMessages(leads)
	if len(messages) < 2 {
		t.Fatalf("expected multiple messages for long list, got %d", len(messages))
	}
	for i, msg := range messages {
		if len(msg) > maxMessageLength+512 {
			t.Fatalf("message %d exceeds size budget: %d chars", i, len(msg))
		}
		if !strings.HasPrefix(msg, greetingLine) {
			t.Fatalf("each chunk must start with greeting, chunk %d = %q", i, msg)
		}
		if !strings.Contains(msg, sectionNewHeader) {
			t.Fatalf("each chunk with leads must repeat section header, chunk %d", i)
		}
	}
}

func TestNextFireTimeBeforeHour(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// Friday before hour → same day
	now := time.Date(2026, time.July, 17, 6, 30, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 17, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimePastHourRollsToNextDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// Friday after hour → Saturday
	now := time.Date(2026, time.July, 17, 10, 15, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 18, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimeExactlyAtHourRollsToNextDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	now := time.Date(2026, time.July, 17, 9, 0, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 18, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestNextFireTimeSaturdayAfterHourSkipsToMonday(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
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
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// Sunday 2026-07-19 before hour → Monday 2026-07-20
	now := time.Date(2026, time.July, 19, 6, 0, 0, 0, loc)
	got := nextFireTime(now, loc, 9)
	want := time.Date(2026, time.July, 20, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFireTime = %s, want %s", got, want)
	}
}

func TestOfficesSkipsDisabled(t *testing.T) {
	var enabled, disabled []string
	for _, def := range scheduledOffices {
		if def.enabled {
			enabled = append(enabled, def.code)
		} else {
			disabled = append(disabled, def.code)
		}
	}
	if len(enabled) == 0 {
		t.Fatal("expected at least one enabled office")
	}
	foundWarsawDisabled := false
	for _, code := range disabled {
		if code == "warsaw" {
			foundWarsawDisabled = true
		}
	}
	if !foundWarsawDisabled {
		t.Fatal("warsaw must be disabled in scheduledOffices")
	}

	s := &Scheduler{}
	active := s.offices()
	for _, off := range active {
		if off.code == "warsaw" {
			t.Fatal("offices() must not include disabled warsaw")
		}
	}
	foundKyiv := false
	for _, off := range active {
		if off.code == "kyiv" {
			foundKyiv = true
		}
	}
	if !foundKyiv {
		t.Fatal("offices() must include enabled kyiv")
	}
}
