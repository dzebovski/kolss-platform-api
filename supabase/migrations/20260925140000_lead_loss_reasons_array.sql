-- CRM v2 (W10): several loss reasons per lead (contract, task W10, decision D12). Additive only:
-- a new array column next to the existing singular leads.loss_reason, which v1 keeps reading and
-- writing exactly as before.
alter table public.leads
  add column if not exists loss_reasons text[] not null default '{}';

-- D12: codes and labels must be data, easy to add/rename without a deploy. is_v2 marks which
-- loss_reasons rows the CRM v2 "Lost" popup may offer/accept, replacing what would otherwise be
-- a hardcoded Go list. The 5 codes already offered by v2 since W5 keep working; the 3 new Lost
-- board codes below (Lost.dc.html REASONS) start disabled.
--
-- Why disabled: every kolss-crm-angular call site of i18n.closeReasonLabel(code) passes the code
-- only, with no DB-label fallback wired up (checked: lead-actions-panel.ts, lead-summary-panel.ts,
-- lead-detail-page.presenter.ts, lead-activity-dialogs.ts, report-lead-block.ts,
-- v2-status-dialog.ts, v2-lead-timeline.ts, v2-current-status-card.ts). A code without a static
-- closeReason.<code> key in messages.{uk,pl,en}.ts renders raw and untranslated in both v1 and
-- v2 today. The 5 already-enabled codes have that key (W5); the 3 new ones do not yet. Flip
-- is_v2 to true for a code once its CRM fallback label ships — no API/migration change needed.
alter table public.loss_reasons add column if not exists is_v2 boolean not null default false;

update public.loss_reasons set is_v2 = true
  where code in ('bought_elsewhere', 'out_of_budget', 'not_relevant', 'cant_reach_client', 'other');

-- Lost.dc.html / Lost-errors.dc.html REASONS, in board order, minus the 5 codes above already in
-- the table. uk/pl copy is a first agent draft (see roadmap G6, "review uk/pl copy"); review
-- before enabling.
insert into public.loss_reasons (code, label_uk, label_pl, label_en, is_v2) values
  ('price_too_high', 'Занадто дорого', 'Zbyt drogo', 'Price too high', false),
  ('project_postponed', 'Проєкт відкладено', 'Projekt odłożony', 'Project postponed', false),
  ('quote_only', 'Хотів лише дізнатися ціну', 'Chciał tylko wyceny', 'Only wanted a price', false)
on conflict (code) do nothing;
