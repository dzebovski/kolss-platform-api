package dailyreport

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func strptr(s string) *string { return &s }

func TestCallStatusLabelMapping(t *testing.T) {
	cases := []struct {
		name   string
		status *string
		want   string
	}{
		{name: "nil is new lead", status: nil, want: "Нова заявка"},
		{name: "no_answer", status: strptr("no_answer"), want: "Не дозвонилися"},
		{name: "callback_requested", status: strptr("callback_requested"), want: "Передзвонити"},
		{name: "unknown falls back to new lead", status: strptr("something_else"), want: "Нова заявка"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callStatusLabel(tc.status); got != tc.want {
				t.Fatalf("callStatusLabel(%v) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestFormatMessagesAllStatuses(t *testing.T) {
	s := &Scheduler{CRMSiteURLPublic: "https://crm.kolss.eu"}
	leadNew := reportLead{ID: uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b"), Name: "Іван", Phone: "+380501112233", CallStatus: nil}
	leadNoAnswer := reportLead{ID: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Name: "Петро", Phone: "+380671112233", CallStatus: strptr("no_answer")}
	leadCallback := reportLead{ID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), Name: "Марія", Phone: "+380931112233", CallStatus: strptr("callback_requested")}

	messages := s.formatMessages([]reportLead{leadNew, leadNoAnswer, leadCallback})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0]

	if !strings.HasPrefix(msg, greetingLine) {
		t.Fatalf("message must start with greeting, got %q", msg)
	}
	wants := []string{
		"- Іван, +380501112233, Нова заявка, <a href=\"https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b\">Відкрити в CRM</a>",
		"- Петро, +380671112233, Не дозвонилися, <a href=\"https://crm.kolss.eu/crm/leads/11111111-2222-3333-4444-555555555555\">Відкрити в CRM</a>",
		"- Марія, +380931112233, Передзвонити, <a href=\"https://crm.kolss.eu/crm/leads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\">Відкрити в CRM</a>",
	}
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing line %q\nfull message:\n%s", want, msg)
		}
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
	}
}

func TestNextFireTimeBeforeHour(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
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
