package crmapi

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

type sheetImportRequest struct {
	SourceID uuid.UUID        `json:"source_id"`
	Mode     string           `json:"mode"`
	Rows     []map[string]any `json:"rows"`
}

type importSource struct {
	ID            uuid.UUID
	OfficeID      uuid.UUID
	OfficeCode    string
	SpreadsheetID string
	SheetName     string
}

type mappedSheetLead struct {
	ExternalID              string
	Name                    *string
	Phone                   *string
	Email                   *string
	ProductInterest         *string
	ProjectStage            *string
	CommunicationPreference *string
	CityRegion              *string
	SourceNote              *string
	SourceCreatedAt         *time.Time
	AdID                    *string
	AdName                  *string
	CampaignID              *string
	CampaignName            *string
	FormID                  *string
	FormName                *string
	Platform                *string
	IsOrganic               *string
	Raw                     map[string]any
}

func (s *Server) handleSheetImport(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "))
	officeCode := s.importOffice(secret)
	if officeCode == "" {
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Invalid import secret", nil)
		return
	}
	var req sheetImportRequest
	if err := decodeJSON(w, r, s.importBodyLimit, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid import batch", nil)
		return
	}
	if req.Mode == "" {
		req.Mode = "incremental"
	}
	if req.SourceID == uuid.Nil || (req.Mode != "incremental" && req.Mode != "reconcile") || len(req.Rows) > 100 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid import source, mode, or row count", nil)
		return
	}
	var source importSource
	err := s.pool.QueryRow(r.Context(), `
		select src.id,src.office_id,o.code,src.spreadsheet_id,src.sheet_name
		from public.lead_import_sources src join public.offices o on o.id=src.office_id
		where src.id=$1 and src.is_enabled=true
	`, req.SourceID).Scan(&source.ID, &source.OfficeID, &source.OfficeCode, &source.SpreadsheetID, &source.SheetName)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "source_not_found", "Import source not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "source_load_failed", "Could not load import source", nil)
		return
	}
	if source.OfficeCode != officeCode {
		s.writeError(w, r, http.StatusForbidden, "source_forbidden", "Import secret does not match source office", nil)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not start import", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var runID uuid.UUID
	if err := tx.QueryRow(r.Context(), `insert into public.lead_import_runs (source_id,status) values ($1,'running') returning id`, source.ID).Scan(&runID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not start import", nil)
		return
	}

	created, updated, skipped := 0, 0, 0
	for _, rawRow := range req.Rows {
		lead, mapErr := mapSheetRow(source, rawRow)
		if mapErr != nil || lead.Phone == nil {
			skipped++
			continue
		}
		var leadID uuid.UUID
		var inserted bool
		err = tx.QueryRow(r.Context(), `
			insert into public.leads (
				office_id,source_system,external_lead_id,source_channel,
				name,phone,email,product_interest,project_stage_source,city_region,source_note,
				source_created_at,ad_id,ad_name,campaign_id,campaign_name,form_id,form_name,platform,is_organic,raw_payload
			) values ($1,'meta_lead_ads',$2,'facebook',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			on conflict (source_system,external_lead_id) do update set
				raw_payload=excluded.raw_payload,
				name=coalesce(excluded.name,public.leads.name),
				phone=coalesce(excluded.phone,public.leads.phone),
				email=coalesce(excluded.email,public.leads.email),
				product_interest=coalesce(excluded.product_interest,public.leads.product_interest),
				project_stage_source=coalesce(excluded.project_stage_source,public.leads.project_stage_source),
				city_region=coalesce(excluded.city_region,public.leads.city_region),
				source_note=coalesce(excluded.source_note,public.leads.source_note),
				updated_at=now()
			returning id,(xmax=0)
		`, source.OfficeID, lead.ExternalID, lead.Name, lead.Phone, lead.Email, lead.ProductInterest, lead.ProjectStage, lead.CityRegion, lead.SourceNote, lead.SourceCreatedAt, lead.AdID, lead.AdName, lead.CampaignID, lead.CampaignName, lead.FormID, lead.FormName, lead.Platform, lead.IsOrganic, lead.Raw).Scan(&leadID, &inserted)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not store import row", nil)
			return
		}
		if inserted {
			created++
			if req.Mode == "incremental" {
				if err := s.notifier.Enqueue(r.Context(), tx, notifications.LeadInfo{
					ID:                      leadID,
					Name:                    lead.Name,
					Phone:                   lead.Phone,
					Email:                   lead.Email,
					ClientInfo:              lead.SourceNote,
					ProductInterest:         lead.ProductInterest,
					ProjectStage:            lead.ProjectStage,
					CommunicationPreference: lead.CommunicationPreference,
					CreatedAt:               lead.SourceCreatedAt,
					OfficeCode:              source.OfficeCode,
					SourceSystem:            "meta_lead_ads",
				}); err != nil {
					s.writeError(w, r, http.StatusInternalServerError, "notification_enqueue_failed", "Could not enqueue lead notification", nil)
					return
				}
			}
		} else {
			updated++
		}
	}
	if _, err := tx.Exec(r.Context(), `
		update public.lead_import_runs set status='success',rows_processed=$2,rows_created=$3,
		rows_updated=$4,rows_skipped=$5,finished_at=now() where id=$1
	`, runID, len(req.Rows), created, updated, skipped); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not finish import", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `update public.lead_import_sources set last_imported_at=now() where id=$1`, source.ID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not finish import", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "import_failed", "Could not finish import", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runId": runID, "mode": req.Mode, "processed": len(req.Rows), "created": created, "updated": updated, "skipped": skipped})
}

func mapSheetRow(source importSource, raw map[string]any) (mappedSheetLead, error) {
	rowNumber := stringValue(raw["_sheet_row"])
	metaID := strings.TrimSpace(stringValue(raw["id"]))
	externalID := ""
	if metaID != "" {
		if strings.HasPrefix(metaID, "l:") {
			externalID = metaID
		} else {
			externalID = "l:" + metaID
		}
	} else if rowNumber != "" {
		externalID = fmt.Sprintf("google_sheet:%s:%s:%s", source.SpreadsheetID, source.SheetName, rowNumber)
	}
	if externalID == "" {
		return mappedSheetLead{}, errors.New("missing lead identity")
	}
	createdAt := parseSheetTime(stringValue(raw["created_time"]))
	lead := mappedSheetLead{
		ExternalID:      externalID,
		Name:            clean(stringValue(raw["full_name"])),
		Phone:           clean(normalizePhone(firstNonEmptyString(raw, "phone", "phone_number"))),
		Email:           clean(strings.ToLower(stringValue(raw["email"]))),
		SourceCreatedAt: createdAt,
		AdID:            clean(stringValue(raw["ad_id"])),
		AdName:          clean(stringValue(raw["ad_name"])),
		CampaignID:      clean(stringValue(raw["campaign_id"])),
		CampaignName:    clean(stringValue(raw["campaign_name"])),
		FormID:          clean(stringValue(raw["form_id"])),
		FormName:        clean(stringValue(raw["form_name"])),
		Platform:        clean(stringValue(raw["platform"])),
		IsOrganic:       clean(stringValue(raw["is_organic"])),
		Raw:             raw,
	}
	if source.OfficeCode == "warsaw" {
		lead.ProductInterest = clean(stringValue(raw["jakiej_kuchni_szukasz?"]))
		lead.ProjectStage = clean(stringValue(raw["na_jakim_etapie_jesteś?"]))
		lead.CityRegion = clean(stringValue(raw["city"]))
		lead.SourceNote = clean(stringValue(raw["kiedy_planujesz_realizację?"]))
	} else {
		productInterest := firstNonEmptyString(raw,
			"які_меблі_вам_потрібно_виготовити?",
			"що_ви_хочете_замовити?",
		)
		projectStage := firstNonEmptyString(raw,
			"на_якому_етапі_перебуває_ваш_проєкт?",
			"на_якому_етапі_ваш_проєкт?",
		)
		communicationPreference := firstNonEmptyString(raw, "як_вам_зручно_спілкуватися?")
		lead.ProductInterest = clean(productInterest)
		lead.ProjectStage = clean(projectStage)
		lead.CommunicationPreference = clean(communicationPreference)
		lead.SourceNote = sourceNoteFromAnswers(productInterest, projectStage, communicationPreference)
	}
	return lead, nil
}

func firstNonEmptyString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(raw[key]); value != "" {
			return value
		}
	}
	return ""
}

func sourceNoteFromAnswers(productInterest, projectStage, communicationPreference string) *string {
	entries := []struct {
		label string
		value string
	}{
		{"Які меблі вам потрібно виготовити?", productInterest},
		{"На якому етапі перебуває ваш проєкт?", projectStage},
		{"Як вам зручно спілкуватися?", communicationPreference},
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if value := strings.TrimSpace(entry.value); value != "" {
			lines = append(lines, entry.label+": "+value)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	value := strings.Join(lines, "\n")
	return &value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

var nonPhoneCharacters = regexp.MustCompile(`[^0-9+]`)

func normalizePhone(value string) string {
	value = nonPhoneCharacters.ReplaceAllString(strings.TrimSpace(value), "")
	if strings.HasPrefix(value, "00") {
		value = "+" + strings.TrimPrefix(value, "00")
	}
	return value
}

func parseSheetTime(value string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "01/02/2006 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}
