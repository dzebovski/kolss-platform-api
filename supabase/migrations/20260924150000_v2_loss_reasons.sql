-- CRM v2 (W5): loss reasons of the v2 Lost popup (contract §2, §3.5). Existing codes and the v1
-- list (expensive / invalid / no_contact / other, validated in the API) stay as they are; v1 only
-- needs labels for the new codes (CRM v1 closeReason.* keys, shipped before this migration).
alter table public.loss_reasons add column if not exists label_en text;

insert into public.loss_reasons (code, label_uk, label_pl, label_en) values
  ('bought_elsewhere', 'Купив деінде', 'Kupił gdzie indziej', 'Bought elsewhere'),
  ('out_of_budget', 'Поза бюджетом', 'Poza budżetem', 'Out of budget'),
  ('not_relevant', 'Вже неактуально', 'Już nieaktualne', 'Not relevant anymore'),
  ('cant_reach_client', 'Не вдається зв''язатися', 'Brak kontaktu z klientem', 'Can''t reach client')
on conflict (code) do nothing;

update public.loss_reasons set label_en = 'Other' where code = 'other' and label_en is null;
