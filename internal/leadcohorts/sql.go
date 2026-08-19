package leadcohorts

// TerminalClientStatusesSQL is the literal SQL IN-list of client_status
// values that close a lead for reminder purposes. A closed lead carries no
// reminders and belongs to none of the six digest groups.
//
// It mirrors isTerminalClientStatus in internal/crmapi/activities.go and the
// backfill in supabase/migrations/20260814120000_close_lead_clears_reminders.sql,
// which clears leads.callback_due_at and cancels scheduled visits on this
// same transition.
const TerminalClientStatusesSQL = `'closed_lost','contract_signed'`

// CallbackDueContextSQL is a jsonb-valued, correlated SQL expression that
// resolves what the shared leads.callback_due_at column currently means for
// the lead aliased `l` in the enclosing query:
//
//   - {"event_category":"call_status","status_code":"callback_requested"}
//     when the due date is a callback reminder,
//   - {"event_category":"client_status","status_code":"thinking"}
//     when it is a "thinking" reminder,
//   - null when the lead is terminal, has no due date, or the due date on
//     the column cannot be attributed to either kind.
//
// This is the single source of truth for that resolution: internal/crmapi's
// lead JSON ('callback_due_context' field, see leadJSONExpression in
// internal/crmapi/leads.go) and this package's cohort queries both embed
// this exact constant, so the CRM lead detail view and the morning digest
// can never disagree about which kind a given callback_due_at belongs to.
//
// It is a correlated subquery: any query embedding it must have `l` (the
// public.leads row) in scope, and must not already bind the alias `e` to
// something else in that same scope.
const CallbackDueContextSQL = `case
	when l.client_status in (` + TerminalClientStatusesSQL + `) then null
	when l.callback_due_at is null then null
	else coalesce((
		select case
			when jsonb_typeof(e.new_value->'callback_due_at') = 'string' then
				jsonb_build_object(
					'event_category', e.event_category,
					'status_code', e.status_code
				)
			when l.call_status = 'callback_requested' then
				jsonb_build_object(
					'event_category', 'call_status',
					'status_code', 'callback_requested'
				)
			else null
		end
		from public.lead_events e
		where e.lead_id = l.id
			and e.new_value ? 'callback_due_at'
		order by e.created_at desc
		limit 1
	), case
		when l.call_status = 'callback_requested' then
			jsonb_build_object('event_category', 'call_status', 'status_code', 'callback_requested')
		when l.client_status = 'thinking' then
			jsonb_build_object('event_category', 'client_status', 'status_code', 'thinking')
		else null
	end)
end`

// latestCommentReminderSQL selects the latest explicit comment event and the
// optional reminder date it recorded. Keeping the event id and timestamp next
// to the due date lets report consumers compare this action with callbacks and
// appointments without independently reimplementing "latest comment".
const latestCommentReminderSQL = `
	select
		e.id,
		e.created_at as action_at,
		case
			when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
				then e.new_value->>'callback_due_at'
			else null
		end as due_at
	from public.lead_events e
	where e.lead_id = l.id
		and e.event_category = 'comment'
	order by e.created_at desc, e.id desc
	limit 1
`

// CommentReminderDueAtSQL is a text-valued, correlated, parenthesized
// subquery returning the due date stored on the latest comment-category
// public.lead_events row for the lead aliased `l` in the enclosing query, or
// null when that event set none.
//
// It is the sole source of the "comment" reminder kind's due date — distinct
// from, and independent of, the shared callback_due_at column — and is
// shared verbatim between internal/crmapi's lead JSON
// ('comment_reminder_due_at' field) and this package's cohort queries.
//
// Cast the result with `::timestamptz` where a real date comparison is
// needed; crmapi instead keeps it as text since it feeds straight into a
// JSON response field.
const CommentReminderDueAtSQL = `(
	select reminder.due_at
	from lateral (` + latestCommentReminderSQL + `) reminder
)`

// latestCallbackActionAtSQL returns when the currently active shared callback
// due date was recorded. Matching the event value to l.callback_due_at avoids
// treating a later, unrelated lead edit as a newly scheduled action. The
// fallback in ActiveReminderCandidatesSQL covers legacy rows without a
// matching event.
const latestCallbackActionAtSQL = `(
	select e.created_at
	from public.lead_events e
	where e.lead_id = l.id
		and jsonb_typeof(e.new_value->'callback_due_at') = 'string'
		and (e.new_value->>'callback_due_at')::timestamptz = l.callback_due_at
	order by e.created_at desc, e.id desc
	limit 1
)`

// ActiveReminderCandidatesSQL is the canonical correlated SQL source for all
// active dated actions on the lead aliased `l`: callback, thinking, comment,
// showroom, and measurement. It yields one row per active action with both its
// due date and the timestamp at which it was last scheduled or rescheduled.
//
// Consumers choose their own aggregation semantics. The morning digest keeps
// every row; the management report orders by action_at and selects one latest
// action per lead. Cleared/completed/canceled actions and terminal or archived
// leads produce no rows.
const ActiveReminderCandidatesSQL = `(
	select candidate.kind, candidate.due_at, candidate.action_at, candidate.source_id
	from (
		select
			'callback'::text as kind,
			l.callback_due_at as due_at,
			coalesce(` + latestCallbackActionAtSQL + `, l.updated_at, l.created_at) as action_at,
			l.id::text as source_id
		where l.callback_due_at is not null
			and (` + CallbackDueContextSQL + `) ->> 'status_code' = 'callback_requested'

		union all

		select
			'thinking',
			l.callback_due_at,
			coalesce(` + latestCallbackActionAtSQL + `, l.updated_at, l.created_at),
			l.id::text
		where l.callback_due_at is not null
			and (` + CallbackDueContextSQL + `) ->> 'status_code' = 'thinking'

		union all

		select
			'comment',
			comment_reminder.due_at::timestamptz,
			comment_reminder.action_at,
			comment_reminder.id::text
		from lateral (` + latestCommentReminderSQL + `) comment_reminder
		where comment_reminder.due_at is not null

		union all

		select
			v.kind,
			v.scheduled_at,
			v.updated_at,
			v.id::text
		from public.lead_showroom_visits v
		where v.lead_id = l.id
			and v.status = 'scheduled'
			and v.kind in ('showroom', 'measurement')
	) candidate
	where l.archived_at is null
		and l.client_status not in (` + TerminalClientStatusesSQL + `)
)`
