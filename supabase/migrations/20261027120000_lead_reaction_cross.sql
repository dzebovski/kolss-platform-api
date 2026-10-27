-- Adds the negative "cross" reaction to the per-user lead reactions.

begin;

alter table public.lead_reactions drop constraint if exists lead_reactions_kind_check;
alter table public.lead_reactions
  add constraint lead_reactions_kind_check check (kind in ('like', 'star', 'question', 'cross'));

commit;
