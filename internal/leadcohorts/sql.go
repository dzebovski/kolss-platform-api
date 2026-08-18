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
	select case
		when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
			then e.new_value->>'callback_due_at'
		else null
	end
	from public.lead_events e
	where e.lead_id = l.id
		and e.event_category = 'comment'
	order by e.created_at desc
	limit 1
)`
