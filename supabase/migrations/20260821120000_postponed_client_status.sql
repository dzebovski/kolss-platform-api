-- Add the "postponed" client_status.
--
-- Nurture/parking status for leads whose client committed to a concrete
-- future timeframe. Non-terminal, like "thinking" — it does not close the
-- lead and does not skip the closed_lost / contract_signed transitions.

alter table public.leads
  drop constraint if exists leads_client_status_check,
  add constraint leads_client_status_check check (
    client_status in (
      'new_lead',
      'showroom_invited',
      'measurement_scheduled',
      'calculation_in_progress',
      'thinking',
      'postponed',
      'closed_lost',
      'contract_signed'
    )
  );
