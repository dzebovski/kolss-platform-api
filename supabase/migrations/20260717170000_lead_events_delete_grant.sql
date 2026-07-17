-- Allow the API role to delete lead timeline events (super-admin timeline management).

begin;

grant delete on public.lead_events to kolss_api;

commit;
