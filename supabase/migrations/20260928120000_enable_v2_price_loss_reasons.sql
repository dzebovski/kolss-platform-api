-- Enable the three CRM v2 loss reasons after their fallback labels shipped.
update public.loss_reasons
set is_v2 = true
where code in ('price_too_high', 'project_postponed', 'quote_only');
