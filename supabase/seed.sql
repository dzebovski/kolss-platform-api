-- Local-only development seed. Applied by `supabase db reset --local` / first
-- `supabase start`; never pushed to the hosted project (`supabase db push`
-- does not run seeds). All people, phones and emails below are fictional.
--
-- Login (local Auth at http://127.0.0.1:54321 only):
--   admin@kolss.local    super_admin
--   kyiv@kolss.local     office_admin, Kyiv
--   warsaw@kolss.local   office_member, Warsaw (HR: fixed schedule)
--   designer@kolss.local office_member, Warsaw (HR: flexible schedule)
--   consultant@kolss.local office_member, Warsaw (HR: not tracked yet)
-- Password for all: kolss-local-dev

do $$
declare
  u record;
begin
  for u in
    select * from (values
      ('00000000-0000-4000-a000-000000000001'::uuid, 'admin@kolss.local',  'super_admin'::public.user_role,   'Local Admin'),
      ('00000000-0000-4000-a000-000000000002'::uuid, 'kyiv@kolss.local',   'office_admin'::public.user_role,  'Олена Київ'),
      ('00000000-0000-4000-a000-000000000003'::uuid, 'warsaw@kolss.local', 'office_member'::public.user_role, 'Marek Warszawa'),
      ('00000000-0000-4000-a000-000000000004'::uuid, 'designer@kolss.local', 'office_member'::public.user_role, 'Ola Projektantka'),
      ('00000000-0000-4000-a000-000000000005'::uuid, 'consultant@kolss.local', 'office_member'::public.user_role, 'Jan Konsultant')
    ) as t(id, email, role, display_name)
  loop
    insert into auth.users (
      instance_id, id, aud, role, email, encrypted_password, email_confirmed_at,
      raw_app_meta_data, raw_user_meta_data, created_at, updated_at,
      confirmation_token, recovery_token, email_change, email_change_token_new
    ) values (
      '00000000-0000-0000-0000-000000000000', u.id, 'authenticated', 'authenticated', u.email,
      extensions.crypt('kolss-local-dev', extensions.gen_salt('bf')), now(),
      '{"provider":"email","providers":["email"]}', '{}', now(), now(), '', '', '', ''
    );
    insert into auth.identities (id, user_id, provider_id, provider, identity_data, last_sign_in_at, created_at, updated_at)
    values (gen_random_uuid(), u.id, u.id::text, 'email',
      jsonb_build_object('sub', u.id::text, 'email', u.email, 'email_verified', true), now(), now(), now());
    -- A trigger on auth.users already created the profile; its role guard
    -- blocks role changes outside a super_admin session, so bypass triggers.
    set local session_replication_role = replica;
    insert into public.profiles (id, role, display_name) values (u.id, u.role, u.display_name)
    on conflict (id) do update set role = excluded.role, display_name = excluded.display_name;
    set local session_replication_role = origin;
  end loop;
end
$$;

insert into public.user_office_memberships (user_id, office_id)
select '00000000-0000-4000-a000-000000000002'::uuid, id from public.offices where code = 'kyiv'
union all
select '00000000-0000-4000-a000-000000000003'::uuid, id from public.offices where code = 'warsaw'
union all
select '00000000-0000-4000-a000-000000000004'::uuid, id from public.offices where code = 'warsaw'
union all
select '00000000-0000-4000-a000-000000000005'::uuid, id from public.offices where code = 'warsaw';

insert into public.leads (
  office_id, source_system, external_lead_id, source_channel, channel,
  name, phone, email, city_region, product_interest, products,
  estimated_budget, estimated_budget_currency, order_comment,
  rating, v2_status, v2_status_changed_at, client_status, call_status,
  assigned_to, no_answer_attempts, callback_due_at, created_at, source_created_at
)
select
  o.id, 'manual', 'seed-' || l.n, l.channel, l.channel,
  l.name, l.phone, l.email, l.city, l.interest, l.products,
  l.budget, l.currency, l.comment,
  l.rating, l.v2_status, now() - l.age, coalesce(l.client_status, 'new_lead'), l.call_status,
  case when o.code = 'kyiv' then '00000000-0000-4000-a000-000000000002'::uuid
       when l.n % 2 = 0 then '00000000-0000-4000-a000-000000000003'::uuid end,
  l.attempts, l.callback, now() - l.age, now() - l.age
from (values
  (1,  'kyiv',   'meta_ads',   'Ірина Коваленко',   '+380501110001', 'iryna@example.com',  'Київ, Оболонь',    'Кухня кутова',        array['kitchen'],             8500,  'USD', 'Хоче білі фасади, стільниця під камінь', 'hot',    'new',      null,                     null,          0, null::timestamptz,          interval '20 minutes'),
  (2,  'kyiv',   'website',    'Андрій Шевчук',      '+380501110002', null,                 'Київ, Позняки',    'Шафа-купе',           array['wardrobe'],            2200,  'USD', null,                                     'medium', 'noanswer', null,                     'callback_requested', 2, now() + interval '3 hours', interval '1 day'),
  (3,  'kyiv',   'referral',   'Марія Бондар',       '+380501110003', 'maria@example.com',  'Ірпінь',           'Кухня + гардероб',    array['kitchen','wardrobe'],  14000, 'USD', 'Рекомендація від клієнта 2025 року',      'hot',    'invited',  'showroom_invited',       'reached',     0, null,                      interval '3 days'),
  (4,  'kyiv',   'phone',      'Олег Мельник',       '+380501110004', null,                 'Київ, Троєщина',   'Ванна кімната',       array['bathroom'],            1800,  'EUR', null,                                     'cold',   'thinking', 'thinking',               'reached',     0, null,                      interval '6 days'),
  (5,  'kyiv',   'meta_ads',   'Світлана Ткаченко',  '+380501110005', 'svitlana@example.com','Бровари',         'Кухня пряма',         array['kitchen'],             null,  'UAH', 'Бюджет не назвала',                      null,     'lost',     'closed_lost',            'reached',     0, null,                      interval '12 days'),
  (6,  'kyiv',   'office',     'Дмитро Лисенко',     '+380501110006', null,                 'Київ, Печерськ',   'Кухня преміум',       array['kitchen'],             22000, 'USD', 'Підписали договір, монтаж у листопаді',  'hot',    'success',  'contract_signed',        'reached',     0, null,                      interval '20 days'),
  (7,  'kyiv',   'google_ads', 'Наталія Савчук',     '+380501110007', 'nata@example.com',   'Вишневе',          'Передпокій',          array['hallway'],             1200,  'USD', null,                                     'medium', 'later',    'postponed',              'reached',     0, now() + interval '14 days', interval '9 days'),
  (8,  'warsaw', 'meta_ads',   'Katarzyna Nowak',    '+48501110008',  'kasia@example.com',  'Warszawa, Mokotów','Kuchnia z wyspą',     array['kitchen'],             60000, 'PLN', 'Chce spotkanie w salonie w sobotę',      'hot',    'new',      null,                     null,          0, null,                      interval '5 minutes'),
  (9,  'warsaw', 'meta_ads',   'Piotr Wiśniewski',   '+48501110009',  null,                 'Warszawa, Wola',   'Szafa wnękowa',       array['wardrobe'],            9000,  'PLN', null,                                     'medium', 'new',      null,                     null,          0, null,                      interval '2 hours'),
  (10, 'warsaw', 'website',    'Anna Wójcik',        '+48501110010',  'anna@example.com',   'Piaseczno',        'Kuchnia + łazienka',  array['kitchen','bathroom'],  85000, 'PLN', 'Dom w budowie, odbiór w marcu',          'hot',    'project',  'calculation_in_progress','reached',     0, null,                      interval '4 days'),
  (11, 'warsaw', 'phone',      'Tomasz Kamiński',    '+48501110011',  null,                 'Warszawa, Ursynów','Garderoba',           array['wardrobe'],            15000, 'PLN', null,                                     'cold',   'noanswer', null,                     'no_answer',   3, now() - interval '1 hour',  interval '2 days'),
  (12, 'warsaw', 'referral',   'Magdalena Lewandowska','+48501110012','magda@example.com',  'Konstancin',       'Meble na wymiar',     array['furniture'],           40000, 'PLN', 'Polecenie od architekta',                'medium', 'invited',  'measurement_scheduled',  'reached',     0, null,                      interval '5 days'),
  (13, 'warsaw', 'meta_ads',   'Michał Zieliński',   '+48501110013',  null,                 'Warszawa, Bemowo', 'Kuchnia',             array['kitchen'],             null,  'PLN', null,                                     null,     'lost',     'closed_lost',            'reached',     0, null,                      interval '15 days'),
  (14, 'warsaw', 'office',     'Joanna Szymańska',   '+48501110014',  'joanna@example.com', 'Warszawa, Żoliborz','Kuchnia + szafy',    array['kitchen','wardrobe'],  120000,'PLN', 'Umowa podpisana',                        'hot',    'success',  'contract_signed',        'reached',     0, null,                      interval '25 days'),
  (15, 'warsaw', 'google_ads', 'Paweł Dąbrowski',    '+48501110015',  null,                 'Pruszków',         'Łazienka',            array['bathroom'],            12000, 'PLN', null,                                     'medium', 'thinking', 'thinking',               'reached',     0, null,                      interval '7 days')
) as l(n, office, channel, name, phone, email, city, interest, products, budget, currency, comment,
       rating, v2_status, client_status, call_status, attempts, callback, age)
join public.offices o on o.code = l.office;

insert into public.lead_events (lead_id, event_type, new_value, created_at)
select id, 'created', jsonb_build_object('source', 'manual', 'source_system', 'manual'), created_at
from public.leads
where external_lead_id like 'seed-%';

-- HR portal (fictional). Dates are relative to the reset day so the calendar is never empty.
insert into public.hr_employees (id, profile_id, office_id, position, started_on)
select e.id, e.profile_id, o.id, e.position, date '2026-01-01'
from (values
  ('00000000-0000-4000-b000-000000000001'::uuid, '00000000-0000-4000-a000-000000000003'::uuid, 'warsaw', 'Doradca klienta'),
  ('00000000-0000-4000-b000-000000000002'::uuid, '00000000-0000-4000-a000-000000000004'::uuid, 'warsaw', 'Projektant'),
  ('00000000-0000-4000-b000-000000000003'::uuid, '00000000-0000-4000-a000-000000000002'::uuid, 'kyiv',   'Менеджерка салону')
) as e(id, profile_id, office, position)
join public.offices o on o.code = e.office;

insert into public.hr_work_patterns (employee_id, valid_from, kind, weekdays, start_time, end_time, days_per_week, hours_per_day)
values
  ('00000000-0000-4000-b000-000000000001', date '2026-01-01', 'fixed',    '{1,2,3,4,5}', '10:00', '18:00', 5, 8),
  ('00000000-0000-4000-b000-000000000002', date '2026-01-01', 'flexible', null,          null,    null,    3, 6),
  ('00000000-0000-4000-b000-000000000003', date '2026-01-01', 'fixed',    '{1,2,3,4,5}', '10:00', '18:00', 5, 8);

insert into public.hr_leave_limits (employee_id, year, days)
values ('00000000-0000-4000-b000-000000000001', extract(year from current_date)::int, 20),
       ('00000000-0000-4000-b000-000000000002', extract(year from current_date)::int, 26);

insert into public.hr_calendar_days (office_id, day, kind, name)
select o.id, d.day, d.kind, d.name
from (values
  ('warsaw', date '2026-11-11', 'public_holiday', 'Narodowe Święto Niepodległości'),
  ('warsaw', date '2026-12-24', 'public_holiday', 'Wigilia'),
  ('warsaw', date '2026-12-25', 'public_holiday', 'Boże Narodzenie'),
  ('warsaw', date '2026-12-31', 'office_closed',  'Inwentaryzacja'),
  ('kyiv',   date '2026-12-25', 'public_holiday', 'Різдво')
) as d(office, day, kind, name)
join public.offices o on o.code = d.office;

-- Week starts (Monday) relative to today.
insert into public.hr_absences (employee_id, type, date_from, date_to, part, comment, created_by, updated_by)
select e.id, a.type, a.date_from, a.date_to, a.part, a.comment, e.profile_id, e.profile_id
from (values
  ('00000000-0000-4000-b000-000000000001'::uuid, 'vacation',       date_trunc('week', current_date)::date + 7,  date_trunc('week', current_date)::date + 9,  'full_day',   null),
  ('00000000-0000-4000-b000-000000000001'::uuid, 'sick',           date_trunc('week', current_date)::date - 5,  date_trunc('week', current_date)::date - 5,  'full_day',   'Przeziębienie'),
  ('00000000-0000-4000-b000-000000000001'::uuid, 'day_off_makeup', date_trunc('week', current_date)::date + 18, date_trunc('week', current_date)::date + 18, 'first_half', null),
  ('00000000-0000-4000-b000-000000000002'::uuid, 'remote',         date_trunc('week', current_date)::date + 3,  date_trunc('week', current_date)::date + 3,  'full_day',   null),
  ('00000000-0000-4000-b000-000000000003'::uuid, 'vacation',       date_trunc('week', current_date)::date + 14, date_trunc('week', current_date)::date + 18, 'full_day',   null)
) as a(employee_id, type, date_from, date_to, part, comment)
join public.hr_employees e on e.id = a.employee_id;

insert into public.hr_shifts (employee_id, work_date, start_time, end_time, kind, created_by, updated_by)
select '00000000-0000-4000-b000-000000000002', date_trunc('week', current_date)::date + s.offset_days, s.start_time, s.end_time, 'planned',
       '00000000-0000-4000-a000-000000000004', '00000000-0000-4000-a000-000000000004'
from (values (0, time '10:00', time '16:00'), (2, time '12:00', time '18:00'), (3, time '10:00', time '16:00')) as s(offset_days, start_time, end_time);
