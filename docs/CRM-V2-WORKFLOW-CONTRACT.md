# CRM v2 workflow — contract (W1)

Status: **approved by the user 2026-09-24** (answers in §6). Nothing here is implemented yet;
`api/openapi.yaml` stays at 2.19.0 until the W tasks land. Implementation tasks: W2–W6 in the CRM v2 roadmap
(`web/.agents/skills/how-to-dev-kolss/references/crm-v2-roadmap.md`).

Sources: lead card v1.3 design (popups, JS state model), leads list design, the KOLSS CRM design
system README, decisions D1–D6 in the roadmap, and the current code (`internal/crmapi/activities.go`,
`leads.go`, migrations up to `20260909120000_lead_questions`).

## 1. Principles

1. **Rule zero: v1 keeps working.** Additive migrations only (nullable or defaulted new columns,
   new rows, wider CHECKs on columns v1 already tolerates). Existing request fields, validation and
   responses stay unchanged. Contract bump **2.19.0 → 2.20.0** (minor).
2. **One v2 action = one timeline event.** v2 status changes write the same `call_status_changed` /
   `client_status_changed` events v1 already renders, with v2 details added to `new_value`. No
   duplicate v1 + v2 events.
3. **Mirror 1:1 both ways.** The v2 handler also sets `call_status` / `client_status` /
   `callback_due_at` / `loss_reason` / the showroom appointment, so v1 screens stay correct for leads
   worked in v2. The v1 activity handler also sets `v2_status` through the same map in the same
   transaction (user, 2026-09-24; see §3.6), so v2 never shows a stale status.
4. **v2 has its own activity type.** v1 rules differ from the design (v1 requires a comment for
   `reached` and `closed_lost`, forbids a date for `no_answer`, and has another loss reason set). One new
   `v2_status` activity with its own validation keeps the v1 activities unchanged.

## 2. Database (one additive migration per W task)

```sql
-- W5 + W4: v2 lead status (D1a) and no-answer attempt counter
alter table public.leads
  add column if not exists v2_status text,
  add column if not exists v2_status_changed_at timestamptz,
  add column if not exists no_answer_attempts smallint not null default 0,
  add constraint leads_v2_status_check check (
    v2_status is null or v2_status in
      ('new', 'later', 'noanswer', 'success', 'thinking', 'invited', 'lost', 'project')
  );
-- Backfill v2_status from v1 fields (same rule as CRM deriveV2LeadStatus):
-- client_status beyond new_lead wins (showroom_invited→invited, thinking→thinking,
-- closed_lost→lost; measurement_scheduled / calculation_in_progress / postponed /
-- contract_signed stay NULL = read-only legacy, D1c), else call_status
-- (callback_requested→later, no_answer→noanswer, reached→success), else 'new'.

-- W2: rating (D3), separate from lead_quality
alter table public.leads
  add column if not exists rating text,
  add constraint leads_rating_check check (rating is null or rating in ('cold', 'medium', 'hot'));

-- W3: channel; v1 source_system / source_channel stay untouched
alter table public.leads
  add column if not exists channel text,
  add constraint leads_channel_check check (
    channel is null or channel in
      ('referral', 'phone', 'office', 'website', 'meta_ads', 'google_ads', 'other')
  );
-- Backfill: meta_lead_ads / facebook → meta_ads; site_form / website → website;
-- manual → office; source_channel 'other' (Google Sheet import) → other.

-- Budget as free input: a number or a range, e.g. "20 000" or "20 000 – 25 000" (user, 2026-09-24).
-- The currency stays in the existing estimated_budget_currency (UAH, USD, EUR, PLN).
alter table public.leads
  add column if not exists estimated_budget_text text,
  add constraint leads_estimated_budget_text_check check (
    estimated_budget_text is null or char_length(estimated_budget_text) <= 60
  );

-- C2 "Lead info" (design popup "Lead info")
alter table public.leads
  add column if not exists products text[] not null default '{}',
  add column if not exists material_fronts text,
  add column if not exists material_worktop text,
  add column if not exists material_appliances text,
  add column if not exists expected_lead_time text,
  add column if not exists preferred_measurement_at timestamptz,
  add constraint leads_products_check check (
    products <@ array['kitchen', 'wardrobe', 'furniture', 'bathroom', 'hallway', 'other']
  );

-- W5: new loss reasons for the v2 design list; existing codes and the v1 list stay as they are
-- (user, 2026-09-24). "Other" reuses the existing `other` code. Labels: uk/pl to confirm on review.
alter table public.loss_reasons add column if not exists label_en text;
insert into public.loss_reasons (code, label_uk, label_pl, label_en) values
  ('bought_elsewhere', 'Купив деінде', 'Kupił gdzie indziej', 'Bought elsewhere'),
  ('out_of_budget', 'Поза бюджетом', 'Poza budżetem', 'Out of budget'),
  ('not_relevant', 'Вже неактуально', 'Już nieaktualne', 'Not relevant anymore'),
  ('cant_reach_client', 'Не вдається зв''язатися', 'Brak kontaktu z klientem', 'Can''t reach client')
on conflict (code) do nothing;
update public.loss_reasons set label_en = 'Other' where code = 'other' and label_en is null;

-- Rating and v2 events use the existing event categories; no CHECK change on lead_events.
```

`v2_status` stays `NULL` for leads whose v1 status is one of the D1c legacy statuses; the API then
returns `v2_status: null` and the CRM shows the v1 status read-only.

## 3. API changes (OpenAPI 2.20.0)

### 3.1 `Lead` — new read fields (the schema already allows extra properties)

```yaml
v2_status:
  oneOf: [{ type: 'null' }, { $ref: '#/components/schemas/V2LeadStatus' }]
v2_status_changed_at: { type: [string, 'null'], format: date-time }
no_answer_attempts: { type: integer, minimum: 0 }
rating:
  oneOf: [{ type: 'null' }, { $ref: '#/components/schemas/LeadRating' }]
channel:
  oneOf: [{ type: 'null' }, { $ref: '#/components/schemas/LeadChannel' }]
# C2 lead info
estimated_budget_text: { type: [string, 'null'], maxLength: 60 }
products: { type: array, items: { $ref: '#/components/schemas/LeadProduct' } }
material_fronts: { type: [string, 'null'] }
material_worktop: { type: [string, 'null'] }
material_appliances: { type: [string, 'null'] }
expected_lead_time: { type: [string, 'null'] }
preferred_measurement_at: { type: [string, 'null'], format: date-time }
# W6: document fields the API already returns but the schema does not declare
name: { type: string }
phone: { type: string }
email: { type: [string, 'null'] }
assigned_to: { type: [string, 'null'], format: uuid }
city_region: { type: [string, 'null'] }
product_interest: { type: [string, 'null'] }
order_comment: { type: [string, 'null'] }   # the client's first message (fallback source_note)
source_note: { type: [string, 'null'] }
source_created_at: { type: string, format: date-time }
loss_reason: { type: [string, 'null'] }
```

New enums: `V2LeadStatus` (`new, later, noanswer, success, thinking, invited, lost, project`),
`LeadRating` (`cold, medium, hot`), `LeadChannel` (`referral, phone, office, website, meta_ads,
google_ads, other`), `LeadProduct` (`kitchen, wardrobe, furniture, bathroom, hallway, other`).

### 3.2 `POST /v1/leads/{leadId}/activities` — two new union members

```yaml
LeadActivityRequest:
  oneOf:
    # ...existing six members unchanged...
    - $ref: '#/components/schemas/V2StatusActivityRequest'
    - $ref: '#/components/schemas/RatingActivityRequest'

V2StatusActivityRequest:
  type: object
  additionalProperties: false
  required: [type, status]
  properties:
    type: { type: string, const: v2_status }
    status: { type: string, enum: [success, later, noanswer, thinking, invited, lost] }
    comment: { type: string }
    dueAt:
      type: string
      format: date-time
      description: >
        later: call back on (required). noanswer: next attempt (required).
        thinking: follow-up on (required). invited: visit start (required).
        success: follow-up date (optional). lost: not allowed.
    # success only (all optional)
    estimatedBudgetText: { type: string, maxLength: 60, description: A number or a range, see §3.7. }
    estimatedBudgetCurrency:
      type: string
      enum: [UAH, USD, EUR, PLN]
      description: Defaults to the office currency (Warsaw PLN, Kyiv UAH) when the lead has none.
    cityRegion: { type: string }
    products: { type: array, items: { $ref: '#/components/schemas/LeadProduct' } }
    nextAction: { type: string, maxLength: 500 }
    # invited only
    designerId: { type: string, format: uuid, description: Any active user (D4). Required for invited. }
    # lost only
    lossReason:
      type: string
      enum: [bought_elsewhere, out_of_budget, not_relevant, cant_reach_client, other]
      description: Required for lost. The v2 list only; v1 keeps its own list and validation.

RatingActivityRequest:
  type: object
  additionalProperties: false
  required: [type, rating]
  properties:
    type: { type: string, const: rating }
    rating: { $ref: '#/components/schemas/LeadRating' }
```

Response stays `{ ok, version }`. `Idempotency-Key` is required as for the other activities.

**What `v2_status` writes** (one transaction, one `lead_events` row):

| v2 status | v2 fields | v1 mirror (D1b) | Event (`event_type` / category / `status_code`) |
|---|---|---|---|
| `success` | `v2_status`, `no_answer_attempts = 0`; budget / city / products when sent | `call_status = reached`; `callback_due_at = dueAt` (or cleared) | `call_status_changed` / `call_status` / `reached` |
| `later` | `v2_status` | `call_status = callback_requested`, `callback_due_at = dueAt` | `call_status_changed` / `call_status` / `callback_requested` |
| `noanswer` | `v2_status`, `no_answer_attempts + 1` | `call_status = no_answer`, `callback_due_at = dueAt` (also shows in v1 reminders; user OK) | `call_status_changed` / `call_status` / `no_answer` |
| `thinking` | `v2_status` | `client_status = thinking`, `callback_due_at = dueAt` | `client_status_changed` / `client_status` / `thinking` |
| `invited` | `v2_status` | `client_status = showroom_invited`; creates a `lead_showroom_visits` row, `kind = showroom`, `responsible_manager_id = designerId`, 60 min, at the showroom of the lead's office (no picker) | `client_status_changed` / `client_status` / `showroom_invited` |
| `lost` | `v2_status` | `client_status = closed_lost`, `loss_reason = lossReason`, reminders cleared, scheduled visits cancelled (as in v1) | `client_status_changed` / `client_status` / `closed_lost` |

`new_value` of the event gets the v2 details: `v2_status`, `due_at`, `attempt` (noanswer),
`next_action`, `products`, `budget`, `city_region`, `designer_id`, `appointment_id`, `loss_reason`.
The lead card uses them for the Current status card and the timeline detail rows.

`rating` writes `leads.rating` and one event `rating_changed` / category `system` /
`new_value = { from, to }`.

### 3.3 Contact and lead info

- The contact popup has First name + Last name; they are joined with one space into the existing
  `name` (user, 2026-09-24). No new name columns.
- `PATCH /v1/leads/{leadId}` (`UpdateLeadRequest`): add an optional `channel` (`LeadChannel`).
  Existing required fields stay required; the v2 contact popup sends the current values for fields
  it does not show.
- New `PATCH /v1/leads/{leadId}/info` (`If-Match` required), partial update for the "Lead info" popup:
  `estimatedBudgetText`, `estimatedBudgetCurrency`, `cityRegion`, `products`, `materialFronts`,
  `materialWorktop`, `materialAppliances`, `expectedLeadTime`, `preferredMeasurementAt`. Only the sent
  fields change. It writes one `lead_edited` event (the type `PATCH /v1/leads/{leadId}` already
  writes; the CRM maps it to `lead_updated`) with the changed field keys in `fields` (`budget`,
  `cityRegion`, `product`, `materials`, `expectedLeadTime`, `preferredMeasurement`) and the new values
  in `info`. Response `{ version }`. Implemented in W7 (OpenAPI 2.25.0, migration
  `20260924160000_lead_info`): texts ≤ 200 (materials) / 60 (lead time) characters, empty string
  clears, `null` clears the date; the CRM needs v1 labels for the three new audit keys first.

### 3.3a Create lead (task W8)

`POST /v1/leads` (`CreateLeadRequest`) gains optional fields for the v2 Create lead popup;
`officeId` remains the showroom (no separate field) and `sourceCreatedAtLocal` remains the
created date/time. All new fields are omittable, so v1 create requests are unchanged:

- `channel` (`LeadChannel`): rejects `other` here (400 `validation_error`), since that value only
  marks the legacy Google Sheet import (§2, §6.11). Omitted keeps the source-derived default
  (`leads_default_channel` trigger), same as before W8.
- `referredBy` (≤ 200 chars), `aboutClient` (≤ 2000 chars): stored in the new `referred_by` /
  `about_client` columns (migration `20260925120000_lead_create_v2_fields`), named for reuse by
  `PATCH /v1/leads/{leadId}/info` in W9.
- `products` (`LeadProduct[]`): reuses the W4/W7 `products` column.
- `estimatedBudgetText` (a number or a range, §3.7): reuses the W7 `estimated_budget_text` column;
  its lower bound overrides `estimatedBudget`. `estimatedBudgetCurrency` still defaults to EUR when
  omitted and no `estimatedBudgetText` is sent (unchanged v1 behaviour); when `estimatedBudgetText`
  is sent and `estimatedBudgetCurrency` is omitted, the default is the office currency instead
  (§3.7), matching `PATCH /v1/leads/{leadId}/info`.

The client's request is the existing `initialMessage` (stored as `order_comment`, shown as the
first timeline entry); showroom is the existing `officeId`. `Lead` (returned by create, get and
list, via `to_jsonb(leads)`) gains `referred_by` and `about_client` for free since the read model
already reflects every lead column.

### 3.3b Manager can be changed by any office user (task G4, decision D9)

`PATCH /v1/leads/{leadId}` (`UpdateLeadRequest.assignedToId`) is no longer super-admin-only.
Every actor who passes `actor.CanEditLead(officeID)` may set it. Super admin keeps its exact
previous behaviour (no server-side check on the assignee at all; omitted, `null` and `""` all
clear). For every other actor, `assignedToId` omitted or `null` **keeps the lead's current
manager** — before this task they could not touch `assigned_to` at all, so this preserves that
(Rule zero; Go's plain `*string` field can't tell an absent JSON key from an explicit `null`, so
both decode to `nil` and both mean "keep" for a non-super-admin actor, see
`resolveAssignedToID`); an explicit `""` clears it, with no check, same as super admin; a uuid
reassigns it and, when that differs from the lead's current `assigned_to` (the v1 Edit lead
dialog always echoes back the unchanged current value, so that request path is unaffected), is
checked with the same rule `GET /v1/managers` and the comment "Assign to" (D5,
`commentAssigneeExistsQuery`) already use: the new assignee must be an active, non-`super_admin`
profile that belongs to the lead's office, else `400 validation_error` on `assignedToId`.

`/v1/me` gains `permissions.canChangeLeadManager` (same office scope as `canEditLeadFields`) for
the v2 UI. v1's own "Assign manager" dialog (`lead-detail-page.ts canAssignManager`) does not
read `/v1/me` permissions for this feature at all — it gates itself on the client-side role check
`isSuperAdminRole(profile.role)`, so v1 keeps showing that control to super admin only and its
behaviour is unchanged by this task.

### 3.4 Leads list

`GET /v1/leads`, new optional query parameters (comma-separated, as the existing filters):

```yaml
- { name: v2Status, in: query, style: form, explode: false,
    schema: { type: array, items: { $ref: '#/components/schemas/V2LeadStatus' } } }
- { name: rating, in: query, style: form, explode: false,
    schema: { type: array, items: { $ref: '#/components/schemas/LeadRating' } } }
- { name: createdFrom, in: query, schema: { type: string, format: date } }   # "Custom" period
- { name: createdTo, in: query, schema: { type: string, format: date } }
```

New `GET /v1/leads/facets` takes the same filters and returns the chip counts:

```yaml
LeadFacetsResponse:
  type: object
  required: [total, v2Status, rating]
  properties:
    total: { type: integer }
    v2Status: { type: object, additionalProperties: { type: integer } }  # ignores the v2Status filter
    rating: { type: object, additionalProperties: { type: integer } }    # ignores the rating filter
```

This matches the list design: status chip counts respect the rating filter but not their own, and
the other way round for rating.

### 3.5 Loss reasons

`LossReason` gets an optional `label_en`. `GET /v1/loss-reasons` is unchanged otherwise. v2 offers
only the five codes of the design list. v1 keeps offering and accepting its own four.

### 3.6 v1 activities also set `v2_status`

`call_status` / `client_status` / `reopen` activities from v1 update `v2_status` and
`v2_status_changed_at` with the D1b map:
- `callback_requested` → later, `no_answer` → noanswer, `reached` → success
- `thinking` → thinking, `showroom_invited` → invited, `closed_lost` → lost, `reopen` → new
- `measurement_scheduled` / `calculation_in_progress` / `postponed` / `contract_signed` → NULL (legacy)

A v1 call status on a lead whose client status is beyond `new_lead` keeps the client-status mapping,
the same precedence as the backfill. `no_answer_attempts` changes only through v2.

### 3.7 Budget input

The UI sends the text as typed: one number (`20000`, `20 000`) or a range (`20 000 – 25 000`, a hyphen
or en dash). The API stores it in `estimated_budget_text` after trimming. When the text holds one
number, that number also goes to v1 `estimated_budget`; for a range, the lower bound goes there
(default choice, change on review), so v1 and the reports keep a numeric value. Text the API cannot
parse returns `400 invalid_budget`. Currency: `estimated_budget_currency` (PLN shown as `zł`); the
default comes from the office (Warsaw PLN, Kyiv UAH).

## 4. v1 fallbacks that must ship first (Rule zero)

1. CRM v1: i18n titles for event `rating_changed`. v1 otherwise shows the raw key, because unknown
   event types fall back to a comment with the raw type as the title.
2. CRM v1: `closeReason.*` labels (uk/pl/en) for `bought_elsewhere`, `out_of_budget`,
   `not_relevant`, `cant_reach_client`. v1 shows unknown loss reason codes as the raw code.
3. v1 reminders for `callback_due_at` with `call_status = no_answer` / `reached` (v1 never produced
   that combination, so the date was invisible). Decided 2026-09-24 (user, option A): both are
   ordinary v1 callback reminders. API `leadcohorts` treats a callback_due_at whose latest call event
   is no_answer / reached as a `callback` candidate (manager tasks, digest "due today" / "overdue"),
   and a no_answer lead with its own next attempt date leaves the "no answer or undated callback"
   group. CRM v1 shows the same reminder, calendar entry and due-date badge as for a callback.

## 5. Rollout per task

- **W2** rating: v1 fallback (`rating_changed` title) → migration → API (`rating` activity, list filter) → CRM.
- **W3** channel: migration + backfill → API (`channel` read, PATCH field) → CRM.
- **W4 + W5** `v2_status` activity, attempts, loss reasons, invite appointment: v1 fallbacks (§4) →
  migration + backfill → API → CRM.
- **W6** schema-only documentation of the existing `Lead` fields (no behaviour change).
- **W7** lead info fields + `PATCH /v1/leads/{leadId}/info`.
- Every step: OpenAPI minor bump, Go tests, regenerated CRM client, `check-api-boundary.mjs` pins (X1).

## 6. User decisions (2026-09-24)

1. v1 activities also set `v2_status` (§3.6).
2. Add new loss reasons for the v2 design list; the choice lives in the new design; the old list and
   v1 are not touched (§2, §3.5).
3. Budget is free input (a number or a range). The currency is selectable among PLN (`zł`), EUR,
   USD, UAH. The default is PLN for Warsaw and UAH for Kyiv (§3.7).
4. Products: a new `products` list; `product_interest` stays as it is.
5. The No answer next attempt date goes to `callback_due_at` and shows in v1 reminders.
6. The showroom is the lead's office showroom, with no picker.
7. First name + Last name are joined into `name`.

Implementation decisions (user, 2026-09-24, before W2):

8. The uk/pl labels of the new loss reasons stay as in §2; a budget range writes its lower bound to v1
   `estimated_budget` (§3.7).
9. The v1 fallbacks (§4) ship as a separate CRM v1 change deployed before the API that writes the new
   values (`rating_changed` before W2, the loss reason labels before W5).
10. `GET /v1/leads/facets` (§3.4) ships with W5, because the status counts need `v2_status`. W2 adds only
    the `rating` list filter.
11. W3: the API sets `channel` when a lead is created, from its source (Meta Lead Ads → `meta_ads`, site
    form → `website`, manual create → `office`). `referral`, `phone` and `google_ads` are set by hand.
12. The "Lead info" fields (§2 "C2 Lead info") and `PATCH /v1/leads/{leadId}/info` (§3.3) are task W7.
13. Every W task bumps the OpenAPI minor version (2.20.0, 2.21.0, …).
14. Invited: the designer must be an active member of the lead's office (the showroom's office);
    otherwise 400 on `designerId` (user, 2026-09-24). This narrows D4 "any active user".
15. The v1 reminder rule for dated no_answer / reached calls is option A in §4.3 (user, 2026-09-24).

## 7. Design gaps found (lead card v1.3, 2026-09-24)

- The popups Call later, No answer, Client thinking, Invited to showroom and Lost declare required fields
  in the JS (date, designer, loss reason chips, 1-hour calendar note), but their markup renders only the
  Comment box. As drawn, Save stays disabled. This contract follows the JS model.
- Successful call declares an optional "Follow up on" date and a budget-confirmed checkbox in the JS,
  but neither is rendered.
- Add comment still has "Assign to"; D5 removed it for the first release. Its "Remind on" date is
  declared but not rendered.
- "Handover details" is computed but has no markup and no trigger. Documents upload has no API (Later).
