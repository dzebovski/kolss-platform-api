# HR portal contract (API 2.45.0)

Plan and decisions D1–D20: `kolss/web/.agents/skills/how-to-dev-kolss/references/hr-portal-plan.md`.
Exact shapes: `api/openapi.yaml`, tag `HR`. This file records the rules the shapes do not show.

## Data

| Table | Purpose |
|---|---|
| `hr_office_settings` | Default vacation days per office (20) |
| `hr_office_hours` | Opening hours per office and ISO weekday; `null` = closed (seed: Mon–Fri 10–19, Sat 10–15) |
| `hr_employees` | Tracked people: one row per CRM profile; `tracked_until` = last tracked day after "stop tracking" |
| `hr_work_patterns` | Schedule history (`fixed`: weekdays + start/end; `flexible`: days/week × hours/day) |
| `hr_calendar_days` | Public holidays and office-closed days per office |
| `hr_leave_limits` | Vacation limit per employee and year (falls back to the office default) |
| `hr_absences` | Absences (soft delete); half days only for a single day; `other` needs a comment |
| `hr_shifts` | Planned hours and make-up days, one per person and day |
| `hr_notifications` | Slack outbox for the HR channel (same dispatcher as lead notifications) |

`public.hr_days(office, from, to, employee)` expands everything into one row per tracked person and day.
Rules are in the header comment of `20261117120200_hr_days.sql`; Go only sums the rows.

## Access

- Every HR endpoint needs a tracked person or a super admin (`hr_not_tracked`, 403).
- Team, accounts, employee settings, schedules, limits, office settings, holiday changes and reports:
  super admin only (`hr_forbidden`, 403). `/v1/me` has `permissions.canManageHr`.
- Calendar: own office; super admin any office.
- Sick leave is `absent` and comments are hidden for everyone except the person and super admin (D8).

## Edit windows (office time zone)

| Who | Planned hours | Absences | Closed month (before the current one) |
|---|---|---|---|
| Employee, own entries | from tomorrow | from today | never (`hr_closed_month`) |
| Super admin | any date | any date | allowed |

Entries carry `editable` and `lockReason` so the CRM shows the lock without repeating the rules.

## Errors

`hr_past_date`, `hr_closed_month`, `hr_outside_office_hours`, `hr_end_before_start`, `hr_comment_required`
(400); `hr_not_tracked`, `hr_forbidden` (403); `hr_overlap`, `hr_already_tracked`, `hr_holiday_exists` (409).
Week plans return per-day errors as `fieldErrors["days.YYYY-MM-DD"]`. The weekly norm and "nobody in the
office" are warnings only.

## Slack

Every create, change and delete posts one Ukrainian line to the HR channel (`SLACK_CHANNEL_ID_HR`; token
`SLACK_BOT_TOKEN_HR`, falls back to the Warsaw bot token, which must be a member of the channel). Sick leave
is posted as "Відсутність"; comments are never posted. Empty channel id = nothing is queued.

## Proposed defaults waiting for the owner (`TODO(hr)` in code)

- Q-H24: sick leave in Slack as "Відсутність".
- Q-H27: 30-minute planning steps.
- New: Slack language Ukrainian; overlapping absences rejected (`hr_overlap`); a make-up day is clamped to
  the office closing time and counted in half days; CSV export only (XLSX needs a new dependency).
