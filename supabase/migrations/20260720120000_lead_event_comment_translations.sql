-- Persist on-demand English translations for timeline comments.

begin;

alter table public.lead_events
  add column if not exists comment_translation_en text,
  add column if not exists comment_translation_source_lang text,
  add column if not exists comment_translated_at timestamptz;

alter table public.lead_events
  drop constraint if exists lead_events_comment_translation_source_lang_check,
  add constraint lead_events_comment_translation_source_lang_check check (
    comment_translation_source_lang is null
    or comment_translation_source_lang in ('UK', 'PL')
  ),
  drop constraint if exists lead_events_comment_translation_complete_check,
  add constraint lead_events_comment_translation_complete_check check (
    (
      comment_translation_en is null
      and comment_translation_source_lang is null
      and comment_translated_at is null
    )
    or (
      nullif(btrim(comment_translation_en), '') is not null
      and comment_translation_source_lang is not null
      and comment_translated_at is not null
    )
  );

commit;
