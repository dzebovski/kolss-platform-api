# Manager tasks rollout — 2026-09-08

The dashboard now uses API contract 2.18.0. Standalone manual tasks reuse
`public.tasks`; reminder, appointment, and undated lead rows are derived from
their existing sources. Existing lead/project task records remain intact.

## Release order

1. Apply `supabase/migrations/20260908120000_manual_tasks.sql` through the normal
   migration process after reviewing it for the target database.
2. Deploy the API with `POST /v1/tasks`, `PATCH /v1/tasks/{taskId}`,
   `GET /v1/dashboard/manager-tasks`, and `/v1/me.permissions.canManageTasks`.
3. Deploy the CRM built against contract 2.18.0.

The migration and deployments have **not** been applied as part of implementation.
Deploying the new CRM against the old API will show a task loading error; deploying
the API before the migration will leave the new task queries unavailable.

## Focused acceptance

With an existing authenticated session, check that the current manager is first
and expanded, other managers and history can expand, and office selection scopes
both the feed and manual task creation. Create, complete, cancel, and restore one
task; check that a second manager in the office sees the result. Confirm that
Reminders and existing lead/appointment drawers remain available.

Automated implementation verification used only the five affected Angular spec
files, task/cursor Go tests, the API boundary check, and one production build.
An isolated PostgreSQL runtime with synthetic fixtures also exercised the actual
migration and SQL: feed sources, office/assignee scope, 57-row pagination, exact
timestamp ordering, status compare-and-swap, audit fields, legacy preservation,
and RLS. No shared database or authenticated production UI was used.

If the UI must be rolled back, deploy its previous version; the additive database
changes can remain. Preserve newly created tasks rather than reversing the schema.
