# CRM v2 workflow — contract draft (W1)

Status: **draft for user review**, 2026-09-24. Nothing here is implemented; `api/openapi.yaml`
stays at 2.19.0 until this draft is approved. Implementation tasks: W2–W6 in the CRM v2 roadmap
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
3. **Mirror 1:1 into v1 fields** (decision D1b): the v2 handler also sets `call_status` /
   `client_status` / `callback_due_at` / `loss_reason` / the showroom appointment, so v1 screens stay
   correct for leads worked in v2.
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

-- C2 "Lead info" (design popup "Lead info") — needs user confirmation, see Q3/Q4
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

-- W5: loss reasons for v2 (design list) + English labels for the en UI
alter table public.loss_reasons add column if not exists label_en text;
insert into public.loss_reasons (code, label_uk, label_pl, label_en)
values ('not_relevant', 'Вже неактуально', 'Już nieaktualne', 'Not relevant anymore')
on conflict (code) do nothing;
-- + fill label_en for expensive, no_contact, lost_client, other.

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
    estimatedBudget: { type: [number, 'null'], minimum: 0 }
    estimatedBudgetCurrency: { type: string, enum: [UAH, USD, EUR, PLN] }
    cityRegion: { type: string }
    products: { type: array, items: { $ref: '#/components/schemas/LeadProduct' } }
    nextAction: { type: string, maxLength: 500 }
    # invited only
    designerId: { type: string, format: uuid, description: Any active user (D4). Required for invited. }
    # lost only
    lossReason:
      type: string
      enum: [lost_client, expensive, not_relevant, no_contact, other]
      description: Required for lost.

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
| `noanswer` | `v2_status`, `no_answer_attempts + 1` | `call_status = no_answer`, `callback_due_at = dueAt` (see Q5) | `call_status_changed` / `call_status` / `no_answer` |
| `thinking` | `v2_status` | `client_status = thinking`, `callback_due_at = dueAt` | `client_status_changed` / `client_status` / `thinking` |
| `invited` | `v2_status` | `client_status = showroom_invited`; creates a `lead_showroom_visits` row, `kind = showroom`, `responsible_manager_id = designerId`, 60 min | `client_status_changed` / `client_status` / `showroom_invited` |
| `lost` | `v2_status` | `client_status = closed_lost`, `loss_reason = lossReason`, reminders cleared, scheduled visits cancelled (as in v1) | `client_status_changed` / `client_status` / `closed_lost` |

`new_value` of the event gets the v2 details: `v2_status`, `due_at`, `attempt` (noanswer),
`next_action`, `products`, `budget`, `city_region`, `designer_id`, `appointment_id`, `loss_reason`.
The lead card uses them for the Current status card and the timeline detail rows.

`rating` writes `leads.rating` and one event `rating_changed` / category `system` /
`new_value = { from, to }`.

### 3.3 Contact and lead info

- `PATCH /v1/leads/{leadId}` (`UpdateLeadRequest`): add an optional `channel` (`LeadChannel`).
  Existing required fields stay required; the v2 contact popup sends the current values for fields
  it does not show.
- New `PATCH /v1/leads/{leadId}/info` (`If-Match` required), partial update for the "Lead info" popup:
  `estimatedBudget`, `estimatedBudgetCurrency`, `cityRegion`, `products`, `materialFronts`,
  `materialWorktop`, `materialAppliances`, `expectedLeadTime`, `preferredMeasurementAt`. Only the sent
  fields change. It writes one `lead_updated` event with the changed fields (the same audit shape v1
  already renders). Response `{ version }`.

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

`LossReason` gets an optional `label_en`. `GET /v1/loss-reasons` is unchanged otherwise.

## 4. v1 fallbacks that must ship first (Rule zero)

1. CRM v1: i18n titles for event `rating_changed`. v1 otherwise shows the raw key, because unknown
   event types fall back to a comment with the raw type as the title.
2. CRM v1: `closeReason.not_relevant` label (uk/pl/en). v1 shows unknown loss reason codes as the raw
   code.
3. Verify that v1 reminders render `callback_due_at` with `call_status = no_answer` / `reached`
   correctly (today v1 never produces that combination).

## 5. Rollout per task

- **W2** rating: migration → API (`rating` activity, list filter, facets) → CRM.
- **W3** channel: migration + backfill → API (`channel` read, PATCH field) → CRM.
- **W4 + W5** `v2_status` activity, attempts, loss reasons, invite appointment: v1 fallbacks (§4) →
  migration + backfill → API → CRM.
- **W6** schema-only documentation of the existing `Lead` fields (no behaviour change).
- Every step: OpenAPI 2.20.x bump, Go tests, regenerated CRM client, `check-api-boundary.mjs` pins (X1).

## 6. Questions for the user

- **Q1 — v1 changes and `v2_status`.** D1b says no two-way sync. Should v1 activities still set
  `v2_status` through the same 1:1 map in the same transaction? It is cheap and avoids stale v2
  statuses on leads worked in v1. Recommended: yes.
- **Q2 — Loss reasons.** Design → DB mapping: Bought elsewhere → `lost_client`, Out of budget →
  `expensive`, Not relevant anymore → new `not_relevant`, Can't reach client → `no_contact`,
  Other → `other`. Is that right?
- **Q3 — Budget.** The design uses free text ("20 000 – 25 000 zł"), the DB a number + currency
  (reports use it). Recommended: keep number + currency (optionally a max value later). Free text would
  need a new column.
- **Q4 — Products.** The design uses chips (Kitchen, Wardrobe, …), v1 free text `product_interest`
  (from Meta forms). Recommended: a new `products` list; `product_interest` stays as it is.
- **Q5 — No answer date in v1.** v2 requires a next attempt date and stores it in `callback_due_at`,
  so it also appears in v1 reminders. OK?
- **Q6 — Showroom choice.** The invite popup carries the lead's office (`f.office`), and each office
  has one showroom (Kyiv, Legionowo). Recommended: the showroom = the lead's office, no picker; changing
  the office would change the client code prefix and access.
- **Q7 — Name.** The contact popup has First name + Last name; the DB has one `name`. Recommended: keep
  one column; the UI joins the two fields with a space.

## 7. Design gaps found (lead card v1.3, 2026-09-24)

- The popups Call later, No answer, Client thinking, Invited to showroom and Lost declare required fields
  in the JS (date, designer, loss reason chips, 1-hour calendar note), but their markup renders only the
  Comment box. As drawn, Save stays disabled. This contract follows the JS model.
- Successful call declares an optional "Follow up on" date and a budget-confirmed checkbox in the JS,
  but neither is rendered.
- Add comment still has "Assign to"; D5 removed it for the first release. Its "Remind on" date is
  declared but not rendered.
- "Handover details" is computed but has no markup and no trigger. Documents upload has no API (Later).
