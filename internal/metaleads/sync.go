package metaleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (i *Integration) ensureConnections(ctx context.Context) error {
	for _, page := range i.Config.Pages {
		var connectionID uuid.UUID
		err := i.Pool.QueryRow(ctx, `
			insert into public.meta_page_connections (office_id,page_id,ingest_after)
			select id,$2,$3 from public.offices where code=$1 and is_active=true
			on conflict (office_id) do update set
			  page_id=excluded.page_id,
			  updated_at=now()
			returning id
		`, page.OfficeCode, page.PageID, i.Config.IngestAfter).Scan(&connectionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("active office %q not found for Meta page", page.OfficeCode)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Integration) syncConfiguredPages(ctx context.Context, full bool) {
	if err := i.ensureConnections(ctx); err != nil {
		i.log().Error("Meta connection reconciliation failed", "error", err)
		return
	}
	for _, page := range i.Config.Pages {
		if ctx.Err() != nil {
			return
		}
		if err := i.syncPageMetadata(ctx, page); err != nil {
			i.log().Warn("Meta Page metadata sync failed", "page_id", page.PageID, "error", err)
			i.recordSyncFailure(ctx, page.PageID, graphErrorKind(err, "page"), err)
			continue
		}
		if _, err := i.syncForms(ctx, page); err != nil {
			i.log().Warn("Meta form discovery failed", "page_id", page.PageID, "error", err)
			i.recordSyncFailure(ctx, page.PageID, graphErrorKind(err, "forms"), err)
			continue
		}
		if err := i.reconcilePage(ctx, page, full); err != nil {
			i.log().Warn("Meta lead reconciliation failed", "page_id", page.PageID, "full", full, "error", err)
			i.recordSyncFailure(ctx, page.PageID, graphErrorKind(err, "reconcile"), err)
			continue
		}
		i.markConnectionHealthy(ctx, page.PageID)
	}
}

func (i *Integration) syncPageMetadata(ctx context.Context, page Page) error {
	metadata, err := i.Client.FetchPage(ctx, page.PageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(metadata.ID) != "" && strings.TrimSpace(metadata.ID) != page.PageID {
		return fmt.Errorf("Meta returned Page %q for configured Page %q", metadata.ID, page.PageID)
	}
	_, err = i.Pool.Exec(ctx, `
		update public.meta_page_connections
		set page_name=nullif($2,''),token_status='valid',token_checked_at=now(),updated_at=now()
		where page_id=$1
	`, page.PageID, strings.TrimSpace(metadata.Name))
	return err
}

func (i *Integration) syncForms(ctx context.Context, page Page) (int, error) {
	connection, err := i.connection(ctx, page.PageID)
	if err != nil {
		return 0, err
	}
	runID, err := i.startSyncRun(ctx, connection.ID, "forms", nil)
	if err != nil {
		return 0, err
	}
	processed := 0
	after := ""
	seenCursors := map[string]struct{}{}
	for {
		forms, next, err := i.Client.ListForms(ctx, page.PageID, after)
		if err != nil {
			i.finishSyncRun(ctx, runID, "failed", processed, 0, 0, err)
			return processed, err
		}
		for _, form := range forms {
			if strings.TrimSpace(form.ID) == "" {
				continue
			}
			questions := form.Questions
			if len(questions) == 0 || string(questions) == "null" {
				questions = json.RawMessage(`[]`)
			}
			status := strings.ToUpper(strings.TrimSpace(form.Status))
			if status == "" {
				status = "UNKNOWN"
			}
			if _, err := i.Pool.Exec(ctx, `
				insert into public.meta_forms (form_id,connection_id,name,status,locale,questions)
				values ($1,$2,nullif($3,''),$4,nullif($5,''),$6::jsonb)
				on conflict (form_id) do update set
				  connection_id=excluded.connection_id,
				  name=coalesce(excluded.name,public.meta_forms.name),
				  status=excluded.status,
				  locale=coalesce(excluded.locale,public.meta_forms.locale),
				  questions=excluded.questions,
				  last_seen_at=now()
			`, form.ID, connection.ID, form.Name, status, form.Locale, questions); err != nil {
				i.finishSyncRun(ctx, runID, "failed", processed, 0, 0, err)
				return processed, err
			}
			processed++
		}
		if next == "" {
			break
		}
		if _, duplicate := seenCursors[next]; duplicate {
			err := errors.New("Meta form pagination cursor repeated")
			i.finishSyncRun(ctx, runID, "failed", processed, 0, 0, err)
			return processed, err
		}
		seenCursors[next] = struct{}{}
		after = next
	}
	i.finishSyncRun(ctx, runID, "success", processed, 0, 0, nil)
	return processed, nil
}

func (i *Integration) reconcilePage(ctx context.Context, page Page, full bool) error {
	connection, err := i.connection(ctx, page.PageID)
	if err != nil {
		return err
	}
	lookback := i.Config.ReconciliationLookback
	syncType := "active_reconcile"
	if full {
		lookback = 7 * 24 * time.Hour
		syncType = "full_reconcile"
	}
	cutoff := time.Now().UTC().Add(-lookback)
	if connection.IngestAfter.After(cutoff) {
		cutoff = connection.IngestAfter
	}
	runID, err := i.startSyncRun(ctx, connection.ID, syncType, &cutoff)
	if err != nil {
		return err
	}
	query := `
		select form_id
		from public.meta_forms
		where connection_id=$1 and status='ACTIVE'
		order by first_seen_at asc
	`
	if full {
		query = `
			select form_id
			from public.meta_forms
			where connection_id=$1 and status in ('ACTIVE','ARCHIVED')
			order by first_seen_at asc
		`
	}
	rows, err := i.Pool.Query(ctx, query, connection.ID)
	if err != nil {
		i.finishSyncRun(ctx, runID, "failed", 0, 0, 0, err)
		return err
	}
	var formIDs []string
	for rows.Next() {
		var formID string
		if err := rows.Scan(&formID); err != nil {
			rows.Close()
			i.finishSyncRun(ctx, runID, "failed", len(formIDs), 0, 0, err)
			return err
		}
		formIDs = append(formIDs, formID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		i.finishSyncRun(ctx, runID, "failed", len(formIDs), 0, 0, err)
		return err
	}

	leadsSeen, eventsCreated := 0, 0
	for _, formID := range formIDs {
		seen, created, err := i.reconcileForm(ctx, page.PageID, formID, cutoff)
		leadsSeen += seen
		eventsCreated += created
		if err != nil {
			i.finishSyncRun(ctx, runID, "failed", len(formIDs), leadsSeen, eventsCreated, err)
			return err
		}
		_, _ = i.Pool.Exec(ctx, `update public.meta_forms set last_reconciled_at=now() where form_id=$1`, formID)
	}
	i.finishSyncRun(ctx, runID, "success", len(formIDs), leadsSeen, eventsCreated, nil)
	if eventsCreated > 0 {
		i.Wake()
	}
	return nil
}

func (i *Integration) reconcileForm(ctx context.Context, pageID, formID string, cutoff time.Time) (leadsSeen int, eventsCreated int, err error) {
	after := ""
	seenCursors := map[string]struct{}{}
	for {
		leads, next, err := i.Client.ListLeads(ctx, pageID, formID, after)
		if err != nil {
			return leadsSeen, eventsCreated, err
		}
		olderOnPage := 0
		for _, lead := range leads {
			createdAt := parseMetaTime(lead.CreatedTime)
			if createdAt == nil {
				i.log().Warn("Meta reconciliation skipped lead without created_time", "leadgen_id", lead.ID, "form_id", formID)
				continue
			}
			if createdAt.Before(cutoff) {
				olderOnPage++
				continue
			}
			leadsSeen++
			if strings.TrimSpace(lead.ID) == "" {
				continue
			}
			if strings.TrimSpace(lead.FormID) == "" {
				lead.FormID = formID
			}
			rawPayload := lead.RawPayload
			if len(rawPayload) == 0 || !json.Valid(rawPayload) {
				rawPayload, err = json.Marshal(lead)
				if err != nil {
					return leadsSeen, eventsCreated, err
				}
			}
			payload, marshalErr := json.Marshal(map[string]any{
				"source":      "reconciliation",
				"lead":        lead,
				"raw_payload": rawPayload,
			})
			if marshalErr != nil {
				return leadsSeen, eventsCreated, marshalErr
			}
			var inserted bool
			insertErr := i.Pool.QueryRow(ctx, `
				insert into public.meta_lead_events (
				  leadgen_id,page_id,form_id,ad_id,event_created_at,status,webhook_payload
				) values ($1,$2,$3,nullif($4,''),$5,'pending',$6::jsonb)
				on conflict (leadgen_id) do nothing
				returning true
			`, lead.ID, pageID, formID, lead.AdID, createdAt, payload).Scan(&inserted)
			if errors.Is(insertErr, pgx.ErrNoRows) {
				continue
			}
			if insertErr != nil {
				return leadsSeen, eventsCreated, insertErr
			}
			if inserted {
				eventsCreated++
			}
		}
		if next == "" || (len(leads) > 0 && olderOnPage == len(leads)) {
			break
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return leadsSeen, eventsCreated, errors.New("Meta lead pagination cursor repeated")
		}
		seenCursors[next] = struct{}{}
		after = next
	}
	return leadsSeen, eventsCreated, nil
}

func (i *Integration) startSyncRun(ctx context.Context, connectionID uuid.UUID, syncType string, cutoff *time.Time) (uuid.UUID, error) {
	var runID uuid.UUID
	err := i.Pool.QueryRow(ctx, `
		insert into public.meta_sync_runs (connection_id,sync_type,status,range_from,range_to)
		values ($1,$2,'running',$3,case when $3::timestamptz is null then null else now() end)
		returning id
	`, connectionID, syncType, cutoff).Scan(&runID)
	return runID, err
}

func (i *Integration) finishSyncRun(ctx context.Context, runID uuid.UUID, status string, forms, leads, events int, syncErr error) {
	var message *string
	if syncErr != nil {
		value := truncateError(syncErr.Error(), 2000)
		message = &value
	}
	_, err := i.Pool.Exec(ctx, `
		update public.meta_sync_runs
		set status=$2,forms_processed=$3,leads_seen=$4,events_created=$5,error_message=$6,finished_at=now()
		where id=$1
	`, runID, status, forms, leads, events, message)
	if err != nil && ctx.Err() == nil {
		i.log().Warn("Meta sync run completion failed", "run_id", runID, "error", err)
	}
}

func graphErrorKind(err error, fallback string) string {
	var graphErr *GraphError
	if errors.As(err, &graphErr) && graphErr.OAuth() {
		return "oauth"
	}
	return fallback
}
