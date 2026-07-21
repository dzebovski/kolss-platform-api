package metaleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

type claimedEvent struct {
	ID             uuid.UUID
	LeadgenID      string
	PageID         string
	FormID         string
	AdID           string
	EventCreatedAt *time.Time
	Payload        []byte
	Attempts       int
	ClaimToken     uuid.UUID
}

type connectionInfo struct {
	ID          uuid.UUID
	OfficeID    uuid.UUID
	OfficeCode  string
	IngestAfter time.Time
}

func (i *Integration) processAvailable(ctx context.Context, includeRetries bool) {
	for processed := 0; processed < batchSize && ctx.Err() == nil; processed++ {
		event, err := i.claimEvent(ctx, includeRetries)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			i.log().Error("Meta lead event claim failed", "error", err)
			return
		}
		if err := i.processEvent(ctx, event); err != nil {
			i.log().Warn("Meta lead event processing failed", "leadgen_id", event.LeadgenID, "page_id", event.PageID, "error", err)
			i.failEvent(ctx, event, err)
		}
	}
}

func (i *Integration) claimEvent(ctx context.Context, includeRetries bool) (claimedEvent, error) {
	if i.Pool == nil {
		return claimedEvent{}, errors.New("Meta lead worker database is not configured")
	}
	tx, err := i.Pool.Begin(ctx)
	if err != nil {
		return claimedEvent{}, err
	}
	defer tx.Rollback(ctx)

	var event claimedEvent
	event.ClaimToken = uuid.New()
	err = tx.QueryRow(ctx, `
		select id,leadgen_id,page_id,coalesce(form_id,''),coalesce(ad_id,''),event_created_at,
		       webhook_payload,attempts
		from public.meta_lead_events
		where attempts < $1
		  and next_attempt_at <= now()
		  and (
		    status='pending'
		    or ($2 and status='retry')
		    or (status='processing' and claimed_at < now() - interval '5 minutes')
		  )
		order by received_at asc
		limit 1
		for update skip locked
	`, maxAttempts, includeRetries).Scan(
		&event.ID,
		&event.LeadgenID,
		&event.PageID,
		&event.FormID,
		&event.AdID,
		&event.EventCreatedAt,
		&event.Payload,
		&event.Attempts,
	)
	if err != nil {
		return claimedEvent{}, err
	}
	if _, err := tx.Exec(ctx, `
		update public.meta_lead_events
		set status='processing',claimed_at=now(),claim_token=$2,last_error=null
		where id=$1
	`, event.ID, event.ClaimToken); err != nil {
		return claimedEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return claimedEvent{}, err
	}
	return event, nil
}

func (i *Integration) processEvent(ctx context.Context, event claimedEvent) error {
	connection, err := i.connection(ctx, event.PageID)
	if err != nil {
		return err
	}
	lead, ok := reconciledLead(event.Payload)
	if !ok {
		lead, err = i.Client.FetchLead(ctx, event.PageID, event.LeadgenID)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(lead.ID) == "" {
		lead.ID = event.LeadgenID
	}
	if strings.TrimSpace(lead.FormID) == "" {
		lead.FormID = event.FormID
	}
	if strings.TrimSpace(lead.AdID) == "" {
		lead.AdID = event.AdID
	}
	if strings.TrimSpace(lead.FormName) == "" && strings.TrimSpace(lead.FormID) != "" {
		var formName *string
		err = i.Pool.QueryRow(ctx, `select name from public.meta_forms where form_id=$1`, lead.FormID).Scan(&formName)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load Meta form attribution: %w", err)
		}
		if formName != nil {
			lead.FormName = strings.TrimSpace(*formName)
		}
	}
	mapped := MapLead(lead)
	createdAt := mapped.SourceCreatedAt
	if createdAt == nil {
		createdAt = event.EventCreatedAt
	}
	if createdAt != nil && createdAt.Before(connection.IngestAfter) {
		_, err := i.Pool.Exec(ctx, `
			update public.meta_lead_events
			set status='ignored',processed_at=now(),last_error='lead predates META_INGEST_AFTER',
			    claimed_at=null,claim_token=null
			where id=$1 and claim_token=$2
		`, event.ID, event.ClaimToken)
		return err
	}
	mapped.SourceCreatedAt = createdAt
	rawLead := lead.RawPayload
	if len(rawLead) == 0 || !json.Valid(rawLead) {
		rawLead, err = json.Marshal(lead)
		if err != nil {
			return fmt.Errorf("encode Meta lead: %w", err)
		}
	}

	tx, err := i.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var leadID uuid.UUID
	var inserted bool
	err = tx.QueryRow(ctx, `
		insert into public.leads (
		  office_id,source_system,external_lead_id,source_channel,
		  name,phone,email,product_interest,project_stage_source,city_region,source_note,
		  source_created_at,ad_id,ad_name,campaign_id,campaign_name,form_id,form_name,platform,is_organic,raw_payload
		) values ($1,'meta_lead_ads',$2,'facebook',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb)
		on conflict (source_system,external_lead_id) do update set
		  raw_payload=excluded.raw_payload,
		  name=case when public.leads.name is null or btrim(public.leads.name)='' then excluded.name else public.leads.name end,
		  phone=case when public.leads.phone is null or btrim(public.leads.phone)='' then excluded.phone else public.leads.phone end,
		  email=case when public.leads.email is null or btrim(public.leads.email)='' then excluded.email else public.leads.email end,
		  product_interest=case when public.leads.product_interest is null or btrim(public.leads.product_interest)='' then excluded.product_interest else public.leads.product_interest end,
		  project_stage_source=case when public.leads.project_stage_source is null or btrim(public.leads.project_stage_source)='' then excluded.project_stage_source else public.leads.project_stage_source end,
		  city_region=case when public.leads.city_region is null or btrim(public.leads.city_region)='' then excluded.city_region else public.leads.city_region end,
		  source_note=case when public.leads.source_note is null or btrim(public.leads.source_note)='' then excluded.source_note else public.leads.source_note end,
		  source_created_at=coalesce(public.leads.source_created_at,excluded.source_created_at),
		  ad_id=case when public.leads.ad_id is null or btrim(public.leads.ad_id)='' then excluded.ad_id else public.leads.ad_id end,
		  ad_name=case when public.leads.ad_name is null or btrim(public.leads.ad_name)='' then excluded.ad_name else public.leads.ad_name end,
		  campaign_id=case when public.leads.campaign_id is null or btrim(public.leads.campaign_id)='' then excluded.campaign_id else public.leads.campaign_id end,
		  campaign_name=case when public.leads.campaign_name is null or btrim(public.leads.campaign_name)='' then excluded.campaign_name else public.leads.campaign_name end,
		  form_id=case when public.leads.form_id is null or btrim(public.leads.form_id)='' then excluded.form_id else public.leads.form_id end,
		  form_name=case when public.leads.form_name is null or btrim(public.leads.form_name)='' then excluded.form_name else public.leads.form_name end,
		  platform=case when public.leads.platform is null or btrim(public.leads.platform)='' then excluded.platform else public.leads.platform end,
		  is_organic=case when public.leads.is_organic is null or btrim(public.leads.is_organic)='' then excluded.is_organic else public.leads.is_organic end,
		  updated_at=now()
		returning id,(xmax=0)
	`,
		connection.OfficeID,
		mapped.ExternalID,
		mapped.Name,
		mapped.Phone,
		mapped.Email,
		mapped.ProductInterest,
		mapped.ProjectStage,
		mapped.CityRegion,
		mapped.SourceNote,
		mapped.SourceCreatedAt,
		mapped.AdID,
		mapped.AdName,
		mapped.CampaignID,
		mapped.CampaignName,
		mapped.FormID,
		mapped.FormName,
		mapped.Platform,
		mapped.IsOrganic,
		rawLead,
	).Scan(&leadID, &inserted)
	if err != nil {
		return err
	}
	if inserted {
		if err := i.Outbox.Enqueue(ctx, tx, notifications.LeadInfo{
			ID:                      leadID,
			Name:                    mapped.Name,
			Phone:                   mapped.Phone,
			Email:                   mapped.Email,
			ClientInfo:              mapped.SourceNote,
			ProductInterest:         mapped.ProductInterest,
			ProjectStage:            mapped.ProjectStage,
			CommunicationPreference: mapped.CommunicationPreference,
			CreatedAt:               mapped.SourceCreatedAt,
			OfficeCode:              connection.OfficeCode,
			SourceSystem:            "meta_lead_ads",
		}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		update public.meta_lead_events
		set status='processed',lead_id=$3,processed_at=now(),attempts=$4,last_error=null,
		    claimed_at=null,claim_token=null
		where id=$1 and claim_token=$2
	`, event.ID, event.ClaimToken, leadID, event.Attempts+1); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if inserted && i.NotificationWaker != nil {
		i.NotificationWaker.Wake()
	}
	return nil
}

func (i *Integration) connection(ctx context.Context, pageID string) (connectionInfo, error) {
	var connection connectionInfo
	err := i.Pool.QueryRow(ctx, `
		select c.id,o.id,o.code,c.ingest_after
		from public.meta_page_connections c
		join public.offices o on o.id=c.office_id
		where c.page_id=$1
	`, pageID).Scan(&connection.ID, &connection.OfficeID, &connection.OfficeCode, &connection.IngestAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectionInfo{}, fmt.Errorf("Meta page %q has no database connection", pageID)
	}
	return connection, err
}

func reconciledLead(payload []byte) (Lead, bool) {
	var envelope struct {
		Source     string          `json:"source"`
		Lead       *Lead           `json:"lead"`
		RawPayload json.RawMessage `json:"raw_payload"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil || envelope.Source != "reconciliation" || envelope.Lead == nil {
		return Lead{}, false
	}
	lead := *envelope.Lead
	if len(envelope.RawPayload) > 0 && json.Valid(envelope.RawPayload) {
		lead.RawPayload = append(json.RawMessage(nil), envelope.RawPayload...)
	}
	return lead, true
}

func (i *Integration) failEvent(ctx context.Context, event claimedEvent, processErr error) {
	attempts := event.Attempts + 1
	status := "retry"
	if attempts >= maxAttempts || !retryableError(processErr) {
		status = "dead_letter"
	}
	nextAttempt := time.Now().UTC().Add(retryDelay(attempts))
	message := truncateError(processErr.Error(), 2000)
	_, updateErr := i.Pool.Exec(ctx, `
		update public.meta_lead_events
		set status=$3,attempts=$4,next_attempt_at=$5,last_error=$6,
		    claimed_at=null,claim_token=null,processed_at=case when $3='dead_letter' then now() else processed_at end
		where id=$1 and claim_token=$2
	`, event.ID, event.ClaimToken, status, attempts, nextAttempt, message)
	if updateErr != nil {
		i.log().Error("Meta lead event failure persistence failed", "leadgen_id", event.LeadgenID, "error", updateErr)
		return
	}
	var graphErr *GraphError
	if status == "dead_letter" || attempts == 3 || (errors.As(processErr, &graphErr) && graphErr.OAuth()) {
		kind := "event_retry"
		if status == "dead_letter" {
			kind = "dead_letter"
		} else if graphErr != nil && graphErr.OAuth() {
			kind = "oauth"
		}
		i.alertConnectionFailure(ctx, event.PageID, kind, "Meta lead processing failure: "+message)
	}
}

func retryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	var graphErr *GraphError
	if !errors.As(err, &graphErr) {
		// Database and local transactional errors are retryable unless proven permanent.
		return true
	}
	if graphErr.OAuth() || graphErr.IsTransient || graphErr.HTTPStatus == 408 || graphErr.HTTPStatus == 409 ||
		graphErr.HTTPStatus == 425 || graphErr.HTTPStatus == 429 || graphErr.HTTPStatus >= 500 {
		return true
	}
	switch graphErr.Code {
	case 1, 2, 4, 17, 32, 341, 368:
		return true
	case 100:
		message := strings.ToLower(graphErr.Message)
		return strings.Contains(message, "does not exist") ||
			strings.Contains(message, "not available") ||
			strings.Contains(message, "temporarily")
	default:
		return false
	}
}

func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 15 * time.Second
	case 2:
		return time.Minute
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	case 5:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

func truncateError(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
