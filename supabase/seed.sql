-- Local-only development seed. Applied by `supabase db reset --local` / first
-- `supabase start`; never pushed to the hosted project (`supabase db push`
-- does not run seeds). All people, phones and emails below are fictional.
--
-- Login (local Auth at http://127.0.0.1:54321 only):
--   admin@kolss.local    super_admin
--   kyiv@kolss.local     office_admin, Kyiv
--   warsaw@kolss.local   office_member, Warsaw
-- Password for all: kolss-local-dev

do $$
declare
  u record;
begin
  for u in
    select * from (values
      ('00000000-0000-4000-a000-000000000001'::uuid, 'admin@kolss.local',  'super_admin'::public.user_role,   'Local Admin'),
      ('00000000-0000-4000-a000-000000000002'::uuid, 'kyiv@kolss.local',   'office_admin'::public.user_role,  'Олена Київ'),
      ('00000000-0000-4000-a000-000000000003'::uuid, 'warsaw@kolss.local', 'office_member'::public.user_role, 'Marek Warszawa')
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
select '00000000-0000-4000-a000-000000000003'::uuid, id from public.offices where code = 'warsaw';

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
