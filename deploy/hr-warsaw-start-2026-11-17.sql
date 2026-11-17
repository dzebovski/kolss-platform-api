-- Warsaw HR tracking was created on 2026-11-17 with the old default start (next Monday,
-- 2026-11-23). Absences and days before 23 Nov were hidden. Move the start (and the first
-- work pattern) of the people still tracked to 2026-11-17. Andrey Gumeniuk (tracking stopped)
-- is left as is.
begin;

update public.hr_work_patterns p
set valid_from = date '2026-11-17'
from public.hr_employees e
join public.offices o on o.id = e.office_id
where p.employee_id = e.id
  and o.code = 'warsaw'
  and e.started_on = date '2026-11-23'
  and e.tracked_until is null
  and p.valid_from = date '2026-11-23';

update public.hr_employees e
set started_on = date '2026-11-17', updated_at = now()
from public.offices o
where o.id = e.office_id
  and o.code = 'warsaw'
  and e.started_on = date '2026-11-23'
  and e.tracked_until is null
returning e.id, e.started_on;

commit;
