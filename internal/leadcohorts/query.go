package leadcohorts

import "strconv"

// OverdueLookbackDays bounds how far back GroupOverdue (group 6) looks for
// reminders: a reminder counts as overdue only if its due date is strictly
// before Params.LocalDate AND not more than OverdueLookbackDays days before
// it.
//
// This intentionally mirrors a limit on the CRM side, not a product
// decision made here: GET /v1/appointments rejects any [from, to) range
// longer than 63 days per request (internal/crmapi/appointments.go:209), so
// the CRM calendar the digest's group 6 deep link lands on can only fetch
// overdue reminders by paging backwards in 63-day windows, and it caps that
// paging at a fixed 365-day lookback rather than paging back indefinitely.
// If this constant and the CRM's 365-day cap ever drift apart, the digest
// count and the list the manager sees after clicking through will silently
// disagree again — exactly the class of bug this package exists to
// prevent — and nothing will fail loudly, since both sides compile and run
// fine independently. Nothing enforces the two staying equal except this
// comment and TestOverdueLookbackDaysMatches365DayCRMWindow; whoever changes
// one value is responsible for changing the other.
const OverdueLookbackDays = 365

// remindersCTE is the single source of truth for "what reminders exist for
// office $1, and when is each one due" — one row per (lead, kind, due_at).
// It is a `with` clause body (no trailing comma) meant to be embedded as
// `with ` + remindersCTE + ` select ...`.
//
// ActiveReminderCandidatesSQL unions the three independent sources documented
// on Params:
//
//  1. the shared leads.callback_due_at column, disambiguated into the
//     'callback' / 'thinking' kinds via CallbackDueContextSQL;
//  2. the latest comment-category lead_events row, via
//     CommentReminderDueAtSQL — kind 'comment';
//  3. public.lead_showroom_visits read directly — kinds 'showroom' and
//     'measurement' — the same table GET /v1/appointments reads (see
//     appointmentSelect in internal/crmapi/appointments.go).
//
// A single lead can appear more than once in this CTE at once — e.g. a
// 'thinking' due date and an independent 'comment' due date on the same day,
// or a showroom visit and a measurement visit both due today. That is
// intentional, not a bug to dedupe away: the CRM reminders/calendar view and
// GET /v1/appointments each render one row per reminder, not one row per
// lead, so countsQuery and leadIDsQuery below count and list every row from
// this CTE without deduplicating on lead_id (unlike groups 1/2, which are
// plain lead lists and do count one row per lead — see both below).
//
// The five kinds mirror the case set built by activeRemindersForLead in
// kolss-crm-angular/src/app/domain/lead.rules.ts:322-352 (callback,
// thinking, showroom, measurement, comment, in that order) — see
// TestRemindersCTEKindsMirrorLeadRulesTS.
const remindersCTE = `reminders as (
	select l.id as lead_id, reminder.kind, reminder.due_at, reminder.action_at
	from public.leads l
	join public.offices o on o.id = l.office_id
	cross join lateral ` + ActiveReminderCandidatesSQL + ` reminder
	where o.code = $1
)`

// countsQuery returns the six digest group counts as one row, in the fixed
// column order (new_leads, no_answer_or_callback_undated, callback_due_today,
// visits_due_today, reminder_due_today, overdue). Args: $1 office code, $2
// local date ("2006-01-02"), $3 IANA timezone name.
//
// Groups 3/5/6 compare (due_at at time zone $3)::date against $2::date using
// exact-day equality ('on the local date') or strict '<' ('overdue') — never
// '<=' — per the digest redesign: overdue is now its own group, not folded
// into "due on or before" the way internal/dailyreport's now-superseded
// isDueOnOrBeforeLocalDate did.
//
// Every reminder-based subquery counts raw rows from `reminders` (count(*)),
// not distinct leads: the CRM reminders/calendar view renders one chip per
// reminder (calendar-page.ts's remindersByDate puts a separate chip on a
// callback and on a comment/task reminder) and GET /v1/appointments renders
// one card per scheduled visit, so a lead with two simultaneous reminders in
// the same group (e.g. 'thinking' and 'comment' both due today) must count
// as 2, matching the two rows the manager actually sees after clicking the
// digest's deep link — not 1, which counting distinct leads would wrongly
// give. Groups 1 and 2 count public.leads rows directly instead: those two
// groups list leads, not reminders, so one row per lead is already correct
// there.
var countsQuery = `
with ` + remindersCTE + `
select
	(
		select count(*)
		from public.leads l
		join public.offices o on o.id = l.office_id
		where o.code = $1
			and l.archived_at is null
			and l.call_status is null
			and l.client_status not in (` + TerminalClientStatusesSQL + `)
	) as new_leads,
	(
		select count(*)
		from public.leads l
		join public.offices o on o.id = l.office_id
		where o.code = $1
			and l.archived_at is null
			and (
				l.call_status = 'no_answer'
				or (l.call_status = 'callback_requested' and l.callback_due_at is null)
			)
			and l.client_status not in (` + TerminalClientStatusesSQL + `)
	) as no_answer_or_callback_undated,
	(
		select count(*)
		from reminders
		where kind = 'callback'
			and (due_at at time zone $3)::date = $2::date
	) as callback_due_today,
	(
		select count(*)
		from reminders
		where kind in ('showroom', 'measurement')
			and (due_at at time zone $3)::date = $2::date
	) as visits_due_today,
	(
		select count(*)
		from reminders
		where kind in ('comment', 'thinking')
			and (due_at at time zone $3)::date = $2::date
	) as reminder_due_today,
	(
		select count(*)
		from reminders
		where (due_at at time zone $3)::date < $2::date
			and (due_at at time zone $3)::date >= $2::date - ` + strconv.Itoa(OverdueLookbackDays) + `
	) as overdue
`

// leadIDsQuery returns the SQL and positional args that select the lead ids
// belonging to a single group, for callers that want the underlying rows
// (e.g. to build the list a digest deep link points at) rather than just a
// count. For groups 1/2 (plain lead lists) each row is a distinct lead. For
// groups 3-6 (reminder-based) a lead can appear more than once — once per
// matching reminder, mirroring what countsQuery counts and what the CRM
// reminders/calendar view and GET /v1/appointments render as separate rows.
// It reports ok=false for an unknown group.
func leadIDsQuery(group Group, params Params) (query string, args []any, ok bool) {
	switch group {
	case GroupNewLeads:
		return `
			select l.id
			from public.leads l
			join public.offices o on o.id = l.office_id
			where o.code = $1
				and l.archived_at is null
				and l.call_status is null
				and l.client_status not in (` + TerminalClientStatusesSQL + `)
			order by l.created_at
		`, []any{params.OfficeCode}, true
	case GroupNoAnswerOrCallbackUndated:
		return `
			select l.id
			from public.leads l
			join public.offices o on o.id = l.office_id
			where o.code = $1
				and l.archived_at is null
				and (
					l.call_status = 'no_answer'
					or (l.call_status = 'callback_requested' and l.callback_due_at is null)
				)
				and l.client_status not in (` + TerminalClientStatusesSQL + `)
			order by l.created_at
		`, []any{params.OfficeCode}, true
	case GroupCallbackDueToday:
		return `
			with ` + remindersCTE + `
			select lead_id
			from reminders
			where kind = 'callback'
				and (due_at at time zone $3)::date = $2::date
		`, []any{params.OfficeCode, params.LocalDate, params.Timezone}, true
	case GroupVisitsDueToday:
		return `
			with ` + remindersCTE + `
			select lead_id
			from reminders
			where kind in ('showroom', 'measurement')
				and (due_at at time zone $3)::date = $2::date
		`, []any{params.OfficeCode, params.LocalDate, params.Timezone}, true
	case GroupReminderDueToday:
		return `
			with ` + remindersCTE + `
			select lead_id
			from reminders
			where kind in ('comment', 'thinking')
				and (due_at at time zone $3)::date = $2::date
		`, []any{params.OfficeCode, params.LocalDate, params.Timezone}, true
	case GroupOverdue:
		return `
			with ` + remindersCTE + `
			select lead_id
			from reminders
			where (due_at at time zone $3)::date < $2::date
				and (due_at at time zone $3)::date >= $2::date - ` + strconv.Itoa(OverdueLookbackDays) + `
		`, []any{params.OfficeCode, params.LocalDate, params.Timezone}, true
	default:
		return "", nil, false
	}
}
