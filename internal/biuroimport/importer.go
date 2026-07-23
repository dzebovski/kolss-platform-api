package biuroimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBlocked = errors.New("import is blocked by validation errors")

type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
)

type RunOptions struct {
	Mode           Mode
	ExpectedSHA256 string
}

type Collision struct {
	ExternalID          string   `json:"externalId"`
	SourceRows          []int    `json:"sourceRows"`
	MaskedPhone         string   `json:"maskedPhone"`
	ExistingLeadSources []string `json:"existingLeadSources"`
}

type Readback struct {
	Leads               int `json:"leads"`
	ContactAttempts     int `json:"contactAttempts"`
	TextComments        int `json:"textComments"`
	MeasurementComments int `json:"measurementComments"`
	SourceCreatedEvents int `json:"sourceCreatedEvents"`
	FirstCallEvents     int `json:"firstCallEvents"`
	Notifications       int `json:"notifications"`
}

type Report struct {
	Mode                     Mode        `json:"mode"`
	Applied                  bool        `json:"applied"`
	SourceSHA256             string      `json:"sourceSha256"`
	SourceRows               int         `json:"sourceRows"`
	LogicalLeads             int         `json:"logicalLeads"`
	DuplicateGroups          int         `json:"duplicateGroups"`
	WouldCreate              int         `json:"wouldCreate"`
	WouldContactAttempts     int         `json:"wouldContactAttempts"`
	WouldTextComments        int         `json:"wouldTextComments"`
	WouldMeasurementComments int         `json:"wouldMeasurementComments"`
	Created                  int         `json:"created"`
	AlreadyImported          int         `json:"alreadyImported"`
	SkippedCollisions        int         `json:"skippedCollisions"`
	ContactAttempts          int         `json:"contactAttempts"`
	TextComments             int         `json:"textComments"`
	MeasurementComments      int         `json:"measurementComments"`
	Warnings                 []Warning   `json:"warnings,omitempty"`
	BlockingErrors           []Warning   `json:"blockingErrors,omitempty"`
	ManagerCandidates        []string    `json:"managerCandidates,omitempty"`
	Collisions               []Collision `json:"collisions,omitempty"`
	Readback                 *Readback   `json:"readback,omitempty"`
}

type dbQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type analysis struct {
	OfficeID   uuid.UUID
	Managers   map[string]uuid.UUID
	OwnLeadIDs map[string]uuid.UUID
	Skipped    map[string]bool
}

func Run(ctx context.Context, pool *pgxpool.Pool, source ParsedSource, options RunOptions) (Report, error) {
	report := Report{
		Mode:            options.Mode,
		SourceSHA256:    source.SHA256,
		SourceRows:      source.SourceRows,
		LogicalLeads:    len(source.LogicalLeads),
		DuplicateGroups: source.DuplicateGroups,
		Warnings:        append([]Warning(nil), source.Warnings...),
	}
	if options.Mode != ModeDryRun && options.Mode != ModeApply {
		return report, fmt.Errorf("unsupported mode %q", options.Mode)
	}
	if options.Mode == ModeApply {
		expected := strings.ToLower(strings.TrimSpace(options.ExpectedSHA256))
		if expected == "" {
			return report, errors.New("apply requires --expected-sha256")
		}
		if expected != source.SHA256 {
			return report, fmt.Errorf("source SHA-256 mismatch: got %s, expected %s", source.SHA256, expected)
		}
	}

	txOptions := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	if options.Mode == ModeApply {
		txOptions = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite}
	}
	tx, err := pool.BeginTx(ctx, txOptions)
	if err != nil {
		return report, fmt.Errorf("begin import transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	state, err := analyze(ctx, tx, source, &report)
	if err != nil {
		return report, err
	}
	if len(report.BlockingErrors) > 0 {
		return report, ErrBlocked
	}
	if options.Mode == ModeDryRun {
		if err := tx.Commit(ctx); err != nil {
			return report, fmt.Errorf("finish dry-run: %w", err)
		}
		return report, nil
	}

	for _, lead := range source.LogicalLeads {
		if state.Skipped[lead.ExternalID] {
			continue
		}
		created, counts, err := applyLead(ctx, tx, source.Config, state, lead)
		if err != nil {
			return report, fmt.Errorf("apply %s: %w", lead.ExternalID, err)
		}
		if created {
			report.Created++
		}
		report.ContactAttempts += counts.ContactAttempts
		report.TextComments += counts.TextComments
		report.MeasurementComments += counts.MeasurementComments
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("commit import: %w", err)
	}
	report.Applied = true

	readback, err := verifyReadback(ctx, pool, source.Config)
	if err != nil {
		return report, fmt.Errorf("readback verification: %w", err)
	}
	report.Readback = &readback
	return report, nil
}

func analyze(ctx context.Context, db dbQuerier, source ParsedSource, report *Report) (analysis, error) {
	state := analysis{
		Managers:   map[string]uuid.UUID{},
		OwnLeadIDs: map[string]uuid.UUID{},
		Skipped:    map[string]bool{},
	}
	err := db.QueryRow(ctx, `select id from public.offices where code=$1 and is_active=true`, source.Config.OfficeCode).Scan(&state.OfficeID)
	if errors.Is(err, pgx.ErrNoRows) {
		report.BlockingErrors = append(report.BlockingErrors, Warning{
			Code: "office_not_found", Message: "active Warsaw office was not found",
		})
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("load active Warsaw office: %w", err)
	}

	managerCounts := map[string]int{}
	managerCandidates := make([]string, 0)
	rows, err := db.Query(ctx, `
		select p.id,coalesce(p.display_name,'')
		from public.profiles p
		join public.user_office_memberships m on m.user_id=p.id
		where m.office_id=$1 and p.is_active=true and p.role<>'super_admin'
	`, state.OfficeID)
	if err != nil {
		return state, fmt.Errorf("load managers: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var displayName string
		if err := rows.Scan(&id, &displayName); err != nil {
			rows.Close()
			return state, fmt.Errorf("scan manager: %w", err)
		}
		managerCandidates = append(managerCandidates, displayName)
		name := canonicalProfileManager(displayName)
		if name != "" {
			managerCounts[name]++
			state.Managers[name] = id
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return state, fmt.Errorf("iterate managers: %w", err)
	}
	rows.Close()
	for _, name := range []string{"Sławek", "Andrii", "Danuta"} {
		switch managerCounts[name] {
		case 1:
		default:
			report.BlockingErrors = append(report.BlockingErrors, Warning{
				Code:    "manager_resolution_failed",
				Message: fmt.Sprintf("expected exactly one active Warsaw manager %s, found %d", name, managerCounts[name]),
			})
		}
	}
	if len(report.BlockingErrors) > 0 {
		sort.Strings(managerCandidates)
		report.ManagerCandidates = managerCandidates
		return state, nil
	}

	externalIDs := make([]string, 0, len(source.LogicalLeads))
	phones := make([]string, 0, len(source.LogicalLeads))
	for _, lead := range source.LogicalLeads {
		externalIDs = append(externalIDs, lead.ExternalID)
		if lead.NormalizedPhone != "" {
			phones = append(phones, strings.TrimPrefix(lead.NormalizedPhone, "+"))
		}
		if lead.FirstConversation != nil && lead.FirstConversation.ManagerName == "" {
			report.Warnings = append(report.Warnings, Warning{
				Code:    "first_conversation_manager_unmapped",
				Message: "first conversation has no recognized manager and will not be imported",
			})
		}
	}

	rows, err = db.Query(ctx, `
		select id,external_lead_id
		from public.leads
		where office_id=$1 and source_system=$2 and external_lead_id=any($3::text[])
	`, state.OfficeID, SourceSystem, externalIDs)
	if err != nil {
		return state, fmt.Errorf("load existing imported leads: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var externalID string
		if err := rows.Scan(&id, &externalID); err != nil {
			rows.Close()
			return state, fmt.Errorf("scan existing imported lead: %w", err)
		}
		state.OwnLeadIDs[externalID] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return state, fmt.Errorf("iterate imported leads: %w", err)
	}
	rows.Close()

	type existingLead struct {
		ID           uuid.UUID
		SourceSystem string
		ExternalID   string
	}
	existingByPhone := map[string][]existingLead{}
	if len(phones) > 0 {
		rows, err = db.Query(ctx, `
			with candidates as (
				select id,source_system,external_lead_id,
					regexp_replace(coalesce(phone,''),'[^0-9]','','g') as raw_digits
				from public.leads
				where office_id=$1
			),
			normalized as (
				select id,source_system,external_lead_id,
					case
						when length(raw_digits)=9 then '48'||raw_digits
						when length(raw_digits)=13 and raw_digits like '0048%' then substr(raw_digits,3)
						else raw_digits
					end as phone_digits
				from candidates
			)
			select id,source_system,external_lead_id,phone_digits
			from normalized
			where phone_digits=any($2::text[])
		`, state.OfficeID, uniqueStrings(phones))
		if err != nil {
			return state, fmt.Errorf("load phone collisions: %w", err)
		}
		for rows.Next() {
			var item existingLead
			var digits string
			if err := rows.Scan(&item.ID, &item.SourceSystem, &item.ExternalID, &digits); err != nil {
				rows.Close()
				return state, fmt.Errorf("scan phone collision: %w", err)
			}
			existingByPhone[digits] = append(existingByPhone[digits], item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return state, fmt.Errorf("iterate phone collisions: %w", err)
		}
		rows.Close()
	}

	for _, lead := range source.LogicalLeads {
		if _, exists := state.OwnLeadIDs[lead.ExternalID]; exists {
			report.AlreadyImported++
			continue
		}
		digits := strings.TrimPrefix(lead.NormalizedPhone, "+")
		matches := existingByPhone[digits]
		foreignSources := make([]string, 0, len(matches))
		for _, match := range matches {
			if match.SourceSystem == SourceSystem && match.ExternalID == lead.ExternalID {
				continue
			}
			foreignSources = append(foreignSources, match.SourceSystem)
		}
		if len(foreignSources) > 0 {
			state.Skipped[lead.ExternalID] = true
			report.SkippedCollisions++
			report.Collisions = append(report.Collisions, Collision{
				ExternalID:          lead.ExternalID,
				SourceRows:          sourceRows(lead.Rows),
				MaskedPhone:         maskPhone(lead.NormalizedPhone),
				ExistingLeadSources: uniqueStrings(foreignSources),
			})
			continue
		}
		report.WouldCreate++
		if lead.FirstConversation != nil && lead.FirstConversation.ManagerName != "" {
			report.WouldContactAttempts++
		}
		for _, row := range lead.Rows {
			for _, event := range row.commentEvents(source.Config) {
				if event.Kind == "measurement" {
					report.WouldMeasurementComments++
				} else {
					report.WouldTextComments++
				}
			}
		}
	}
	sort.Slice(report.Collisions, func(i, j int) bool {
		return report.Collisions[i].ExternalID < report.Collisions[j].ExternalID
	})
	return state, nil
}

type applyCounts struct {
	ContactAttempts     int
	TextComments        int
	MeasurementComments int
}

func applyLead(
	ctx context.Context,
	tx pgx.Tx,
	config SourceConfig,
	state analysis,
	lead LogicalLead,
) (bool, applyCounts, error) {
	var assignedTo *uuid.UUID
	if id, exists := state.Managers[lead.ManagerName]; exists {
		value := id
		assignedTo = &value
	}
	raw, err := lead.RawPayload(config)
	if err != nil {
		return false, applyCounts{}, fmt.Errorf("marshal provenance: %w", err)
	}

	var leadID uuid.UUID
	created := false
	err = tx.QueryRow(ctx, `
		insert into public.leads (
			office_id,source_system,source_channel,external_lead_id,assigned_to,
			name,phone,email,city_region,source_created_at,raw_payload,last_comment,last_comment_at
		) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13)
		on conflict (source_system,external_lead_id) do nothing
		returning id
	`, state.OfficeID, SourceSystem, SourceChannel, lead.ExternalID, assignedTo,
		nilIfEmpty(lead.Name), nilIfEmpty(lead.Phone), nilIfEmpty(lead.Email), nilIfEmpty(lead.CityRegion),
		lead.SourceCreatedAt, string(raw), nilIfEmpty(lead.LastComment), lead.LastCommentAt,
	).Scan(&leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			select id from public.leads
			where office_id=$1 and source_system=$2 and external_lead_id=$3
		`, state.OfficeID, SourceSystem, lead.ExternalID).Scan(&leadID)
	} else if err == nil {
		created = true
	}
	if err != nil {
		return false, applyCounts{}, fmt.Errorf("store lead: %w", err)
	}

	createdValue, _ := json.Marshal(map[string]any{
		"import_source": ImportSource,
		"import_kind":   "source_created",
		"source_system": SourceSystem,
	})
	if _, err := tx.Exec(ctx, `
		insert into public.lead_events
			(id,lead_id,actor_id,event_type,event_category,new_value,created_at)
		values ($1,$2,null,'created','system',$3::jsonb,$4)
		on conflict (id) do nothing
	`, sourceCreatedEventID(config, lead.ExternalID), leadID, string(createdValue), lead.SourceCreatedAt); err != nil {
		return false, applyCounts{}, fmt.Errorf("store source-created event: %w", err)
	}

	counts := applyCounts{}
	if lead.FirstConversation != nil {
		managerID, exists := state.Managers[lead.FirstConversation.ManagerName]
		if exists {
			tag, err := tx.Exec(ctx, `
				insert into public.lead_contact_attempts
					(id,lead_id,manager_id,result,comment,created_at)
				values ($1,$2,$3,'reached',$4,$5)
				on conflict (id) do nothing
			`, firstConversationAttemptID(config, lead.FirstConversation.SourceKey), leadID, managerID,
				firstConversationComment, lead.FirstConversation.At)
			if err != nil {
				return false, applyCounts{}, fmt.Errorf("store first conversation attempt: %w", err)
			}
			counts.ContactAttempts += int(tag.RowsAffected())

			newValue, _ := json.Marshal(map[string]any{
				"import_source": ImportSource,
				"import_kind":   "first_conversation",
				"call_status":   "reached",
			})
			if _, err := tx.Exec(ctx, `
				insert into public.lead_events
					(id,lead_id,actor_id,event_type,event_category,status_code,comment,new_value,created_at)
				values ($1,$2,$3,'call_status_changed','call_status','reached',$4,$5::jsonb,$6)
				on conflict (id) do nothing
			`, firstConversationEventID(config, lead.FirstConversation.SourceKey), leadID, managerID,
				firstConversationComment, string(newValue), lead.FirstConversation.At); err != nil {
				return false, applyCounts{}, fmt.Errorf("store first conversation event: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				update public.leads
				set call_status='reached',call_status_changed_at=$2,updated_at=now(),version=version+1
				where id=$1 and (call_status is distinct from 'reached' or call_status_changed_at is distinct from $2)
			`, leadID, lead.FirstConversation.At); err != nil {
				return false, applyCounts{}, fmt.Errorf("update first conversation snapshot: %w", err)
			}
		}
	}

	for _, row := range lead.Rows {
		for _, event := range row.commentEvents(config) {
			var actorID *uuid.UUID
			if id, exists := state.Managers[event.ManagerName]; exists {
				value := id
				actorID = &value
			}
			newValue, _ := json.Marshal(map[string]any{
				"import_source": ImportSource,
				"import_kind":   event.Kind,
				"source_key":    event.SourceKey,
			})
			tag, err := tx.Exec(ctx, `
				insert into public.lead_events
					(id,lead_id,actor_id,event_type,event_category,comment,new_value,created_at)
				values ($1,$2,$3,'comment_added','comment',$4,$5::jsonb,$6)
				on conflict (id) do nothing
			`, event.ID, leadID, actorID, event.Body, string(newValue), event.At)
			if err != nil {
				return false, applyCounts{}, fmt.Errorf("store %s comment: %w", event.Kind, err)
			}
			if event.Kind == "measurement" {
				counts.MeasurementComments += int(tag.RowsAffected())
			} else {
				counts.TextComments += int(tag.RowsAffected())
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		update public.leads
		set last_comment=$2,last_comment_at=$3
		where id=$1
		  and (last_comment is distinct from $2 or last_comment_at is distinct from $3)
	`, leadID, nilIfEmpty(lead.LastComment), lead.LastCommentAt); err != nil {
		return false, applyCounts{}, fmt.Errorf("restore imported last comment: %w", err)
	}
	return created, counts, nil
}

func verifyReadback(ctx context.Context, pool *pgxpool.Pool, config SourceConfig) (Readback, error) {
	prefix := fmt.Sprintf("sheet:%s:%d:", config.SpreadsheetID, config.SheetID)
	var result Readback
	err := pool.QueryRow(ctx, `
		with imported as (
			select id from public.leads
			where source_system=$1 and external_lead_id like $2
		)
		select
			(select count(*) from imported),
			(select count(*) from public.lead_contact_attempts a join imported i on i.id=a.lead_id),
			(select count(*) from public.lead_events e join imported i on i.id=e.lead_id
			 where e.new_value->>'import_source'=$3
			   and e.new_value->>'import_kind' in ('information','info_slawek','status')),
			(select count(*) from public.lead_events e join imported i on i.id=e.lead_id
			 where e.new_value->>'import_source'=$3 and e.new_value->>'import_kind'='measurement'),
			(select count(*) from public.lead_events e join imported i on i.id=e.lead_id
			 where e.new_value->>'import_source'=$3 and e.new_value->>'import_kind'='source_created'),
			(select count(*) from public.lead_events e join imported i on i.id=e.lead_id
			 where e.new_value->>'import_source'=$3 and e.new_value->>'import_kind'='first_conversation'),
			(select count(*) from public.lead_notifications n join imported i on i.id=n.lead_id)
	`, SourceSystem, prefix+"%", ImportSource).Scan(
		&result.Leads,
		&result.ContactAttempts,
		&result.TextComments,
		&result.MeasurementComments,
		&result.SourceCreatedEvents,
		&result.FirstCallEvents,
		&result.Notifications,
	)
	return result, err
}

func nilIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sourceRows(rows []SourceRow) []int {
	result := make([]int, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.SheetRow)
	}
	sort.Ints(result)
	return result
}

func maskPhone(phone string) string {
	if len(phone) < 7 {
		return "***"
	}
	return phone[:5] + "***" + phone[len(phone)-3:]
}
