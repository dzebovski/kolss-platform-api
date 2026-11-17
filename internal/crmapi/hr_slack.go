package crmapi

// HR portal — Slack messages (HB5, D3). Every create / change / delete is queued in the same
// transaction (hr_notifications outbox) and posted to the HR channel by the dispatcher.
//
// Privacy: sick leave is written as "Відсутність" (Absent) and comments are never posted, because
// colleagues read the channel. TODO(hr): Q-H24 proposed default. Language: Ukrainian (team
// working language). TODO(hr): Q-H28 proposed default.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var hrSlackTypeLabels = map[string]string{
	hrTypeVacation:     "Відпустка",
	hrTypeSick:         "Відсутність", // D8 / Q-H24: never "sick" in the channel
	hrTypeDayOffUnpaid: "Вихідний (неоплачуваний)",
	hrTypeDayOffMakeup: "Вихідний з відпрацюванням",
	hrTypeRemote:       "Віддалений день",
	hrTypeOccasional:   "Відпустка за обставинами",
	hrTypeOther:        "Інше",
	hrTypeMakeupDay:    "День відпрацювання",
}

var hrSlackPartLabels = map[string]string{
	hrPartFirst:  "перша половина дня",
	hrPartSecond: "друга половина дня",
}

func hrSlackName(name *string) string {
	if name == nil || strings.TrimSpace(*name) == "" {
		return "—"
	}
	return escapeHRSlack(strings.TrimSpace(*name))
}

func escapeHRSlack(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

func hrSlackDates(from, to time.Time) string {
	if from.Equal(to) {
		return from.Format("02.01.2006")
	}
	if from.Year() == to.Year() {
		return from.Format("02.01") + "–" + to.Format("02.01.2006")
	}
	return from.Format("02.01.2006") + "–" + to.Format("02.01.2006")
}

// hrSlackAbsence describes one absence (or make-up day) in a single line.
func hrSlackAbsence(absenceType string, from, to time.Time, part string) string {
	label := hrSlackTypeLabels[absenceType]
	if label == "" {
		label = absenceType
	}
	text := label + ", " + hrSlackDates(from, to)
	if p := hrSlackPartLabels[part]; p != "" {
		text += " (" + p + ")"
	}
	return text
}

type hrSlackMessage struct {
	Icon   string // 🗓️ added, ✏️ changed, 🗑️ deleted, 👤 people, ⚙️ settings
	Action string // e.g. "Нова відсутність"
	Person *string
	Detail string
	Before string
	Actor  *string
}

func (m hrSlackMessage) text() string {
	var b strings.Builder
	b.WriteString(m.Icon + " HR · " + m.Action)
	if m.Person != nil {
		b.WriteString(": " + hrSlackName(m.Person))
	}
	if m.Detail != "" {
		b.WriteString(" — " + escapeHRSlack(m.Detail))
	}
	if m.Before != "" {
		b.WriteString(" (було: " + escapeHRSlack(m.Before) + ")")
	}
	b.WriteString("\nАвтор: " + hrSlackName(m.Actor))
	return b.String()
}

// hrNotify queues the message; the caller wakes the dispatcher after commit (hrWake).
func (s *Server) hrNotify(ctx context.Context, tx pgx.Tx, message hrSlackMessage) (bool, error) {
	return s.outbox.EnqueueHR(ctx, tx, message.text())
}

func (s *Server) hrWake(queued bool) {
	if queued && s.notificationWaker != nil {
		s.notificationWaker.Wake()
	}
}

func hrSlackHours(start, end string) string {
	return fmt.Sprintf("%s–%s", start, end)
}
