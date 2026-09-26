-- CRM v2 (W9): fields from the "Fill lead info" / "Create project" boards (contract, task W9).
-- Additive only: new nullable columns v1 never reads. about_client and referred_by already exist
-- (W8, migration 20260925120000); this migration adds the clarification checklist (5 booleans),
-- client_informed, project_type and responsible_manager_id.
alter table public.leads
  add column if not exists checklist_budget boolean,
  add column if not exists checklist_location boolean,
  add column if not exists checklist_period boolean,
  add column if not exists checklist_materials boolean,
  add column if not exists checklist_product boolean,
  add column if not exists client_informed boolean,
  add column if not exists project_type text,
  add column if not exists responsible_manager_id uuid references public.profiles(id) on delete set null;

-- Board PTYPES ids (Fill-lead-info.dc.html / Create-project.dc.html): express (Paid express
-- evaluation), measure (Paid measurement and project design), contract (Contract signing).
alter table public.leads
  drop constraint if exists leads_project_type_check,
  add constraint leads_project_type_check check (
    project_type is null or project_type in ('express', 'measure', 'contract')
  );

-- responsible_manager_id is not assigned_to (the lead owner, G4): it is the manager the "Fill
-- lead info" / "Create project" popup names for the planned project. The application layer
-- validates it against the same active, non-super_admin, office-member rule as assigned_to and
-- comment "Assign to" (commentAssigneeExistsQuery); the FK only guards referential integrity.
create index if not exists idx_leads_responsible_manager_id on public.leads(responsible_manager_id);
