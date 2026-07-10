-- CRM Angular close-reason codes (FK target for leads.loss_reason).

insert into public.loss_reasons (code, label_uk, label_pl) values
  ('no_contact', 'Немає контакту', 'Brak kontaktu'),
  ('location_mismatch', 'Не підходить місцеположення', 'Nieodpowiednia lokalizacja'),
  ('expensive', 'Дорого', 'Za drogo'),
  ('lost_client', 'Втрачений клієнт', 'Utracony klient')
on conflict (code) do update
set
  label_uk = excluded.label_uk,
  label_pl = excluded.label_pl;
