// Package leadcohorts computes the six lead groups the morning digest shows
// as "one line per group + count + CRM deep link". It is the single source
// of truth for what counts as "due today" and "overdue", so the number in
// the digest message and the number of rows the manager sees after clicking
// the deep link into the CRM can never disagree. For groups 1/2 that row is
// a lead; for the four reminder-based groups it is a reminder — see Counts
// and LeadIDs below for why those are not the same thing.
//
// Before this package, internal/dailyreport and internal/crmapi computed
// overlapping reminder concepts with independently-written SQL (lateral
// joins over lead_events in the former, a JSON-building expression in the
// latter), which is why they could drift apart. leadcohorts and
// internal/crmapi's lead JSON now share the exact SQL text for the two
// hardest expressions to get right — see CallbackDueContextSQL and
// CommentReminderDueAtSQL in sql.go.
package leadcohorts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Group identifies one of the six digest cohorts.
type Group string

const (
	// GroupNewLeads: call_status is null, not archived, non-terminal
	// client_status. Brand new leads nobody has called yet.
	GroupNewLeads Group = "new_leads"
	// GroupNoAnswerOrCallbackUndated: call_status='no_answer', or
	// call_status='callback_requested' with no callback_due_at yet, not
	// archived, non-terminal client_status.
	GroupNoAnswerOrCallbackUndated Group = "no_answer_or_callback_undated"
	// GroupCallbackDueToday: a 'callback' reminder (see
	// CallbackDueContextSQL) due on LocalDate.
	GroupCallbackDueToday Group = "callback_due_today"
	// GroupVisitsDueToday: a scheduled showroom or measurement visit due on
	// LocalDate, read directly from public.lead_showroom_visits.
	GroupVisitsDueToday Group = "visits_due_today"
	// GroupReminderDueToday: a 'thinking' or 'comment' reminder due on
	// LocalDate.
	GroupReminderDueToday Group = "reminder_due_today"
	// GroupOverdue: a reminder of any kind (callback, thinking, comment,
	// showroom, or measurement) due strictly before LocalDate.
	GroupOverdue Group = "overdue"
)

// Groups lists the six cohorts in the digest's display order.
var Groups = []Group{
	GroupNewLeads,
	GroupNoAnswerOrCallbackUndated,
	GroupCallbackDueToday,
	GroupVisitsDueToday,
	GroupReminderDueToday,
	GroupOverdue,
}

// Params scopes every cohort query to one office and one office-local
// calendar day.
//
// Timezone handling mirrors internal/dailyreport (Scheduler.runForOffice
// passes off.loc.String(), i.e. the office's IANA zone name) and
// GET /v1/appointments (internal/crmapi/appointments.go, which loads
// public.offices.timezone_name into a *time.Location): both resolve the
// office's IANA zone name and compare wall-clock dates in it, never UTC
// dates. Params intentionally does not resolve the timezone itself — the
// caller already knows it from one of those two conventions.
type Params struct {
	// OfficeCode is public.offices.code, e.g. "kyiv" or "warsaw".
	OfficeCode string
	// LocalDate is the office-local calendar date to evaluate "due today"
	// and "overdue" against, formatted "2006-01-02".
	LocalDate string
	// Timezone is an IANA zone name, e.g. "Europe/Kyiv".
	Timezone string
}

func (p Params) validate() error {
	if strings.TrimSpace(p.OfficeCode) == "" {
		return fmt.Errorf("leadcohorts: officeCode is required")
	}
	if _, err := time.Parse("2006-01-02", p.LocalDate); err != nil {
		return fmt.Errorf("leadcohorts: invalid localDate %q: %w", p.LocalDate, err)
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("leadcohorts: invalid timezone %q: %w", p.Timezone, err)
	}
	return nil
}

// Counts holds the six digest cohort counts computed as of Params.
type Counts struct {
	NewLeads                  int
	NoAnswerOrCallbackUndated int
	CallbackDueToday          int
	VisitsDueToday            int
	ReminderDueToday          int
	Overdue                   int
}

// Value returns the count for a single Group, or 0 for an unknown one.
func (c Counts) Value(group Group) int {
	switch group {
	case GroupNewLeads:
		return c.NewLeads
	case GroupNoAnswerOrCallbackUndated:
		return c.NoAnswerOrCallbackUndated
	case GroupCallbackDueToday:
		return c.CallbackDueToday
	case GroupVisitsDueToday:
		return c.VisitsDueToday
	case GroupReminderDueToday:
		return c.ReminderDueToday
	case GroupOverdue:
		return c.Overdue
	default:
		return 0
	}
}

// FetchCounts computes all six digest group counts for one office and one
// office-local day in a single round trip.
func FetchCounts(ctx context.Context, pool *pgxpool.Pool, params Params) (Counts, error) {
	if err := params.validate(); err != nil {
		return Counts{}, err
	}
	var c Counts
	err := pool.QueryRow(ctx, countsQuery, params.OfficeCode, params.LocalDate, params.Timezone).Scan(
		&c.NewLeads,
		&c.NoAnswerOrCallbackUndated,
		&c.CallbackDueToday,
		&c.VisitsDueToday,
		&c.ReminderDueToday,
		&c.Overdue,
	)
	if err != nil {
		return Counts{}, err
	}
	return c, nil
}

// LeadIDs returns the lead ids belonging to a single cohort, e.g. for a
// caller that wants the underlying list a digest deep link points at rather
// than just the count. The count from FetchCounts for the same Group and
// Params always equals len(LeadIDs(...)) for that group — both are built
// from the exact same predicates.
//
// For GroupNewLeads and GroupNoAnswerOrCallbackUndated each id is a distinct
// lead. For the four reminder-based groups a lead's id can appear more than
// once — once per matching reminder — because those groups count and list
// reminders, not leads: a lead with, say, both a 'thinking' and a 'comment'
// reminder due the same day contributes two entries, matching the two
// separate rows the CRM reminders/calendar view (and GET /v1/appointments,
// for visits) would show for it.
func LeadIDs(ctx context.Context, pool *pgxpool.Pool, group Group, params Params) ([]uuid.UUID, error) {
	if err := params.validate(); err != nil {
		return nil, err
	}
	query, args, ok := leadIDsQuery(group, params)
	if !ok {
		return nil, fmt.Errorf("leadcohorts: unknown group %q", group)
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
