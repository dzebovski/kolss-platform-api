package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	appointmentStatusScheduled = "scheduled"
	appointmentLocalLayout     = "2006-01-02T15:04"
	appointmentDateLayout      = "2006-01-02"
)

type appointmentMutationRequest struct {
	LeadID               uuid.UUID  `json:"leadId"`
	StartsAtLocal        *string    `json:"startsAtLocal"`
	DurationMinutes      *int       `json:"durationMinutes"`
	ResponsibleManagerID *uuid.UUID `json:"responsibleManagerId"`
	Comment              *string    `json:"comment"`
	Status               *string    `json:"status"`
}

type appointmentLeadSummary struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Phone string    `json:"phone"`
}

type appointmentOfficeSummary struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	TimezoneName string    `json:"timezoneName"`
}

type appointmentManagerSummary struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"displayName"`
}

type appointment struct {
	ID                    uuid.UUID                  `json:"id"`
	Lead                  appointmentLeadSummary     `json:"lead"`
	Office                appointmentOfficeSummary   `json:"office"`
	ResponsibleManager    *appointmentManagerSummary `json:"responsibleManager"`
	StartsAt              time.Time                  `json:"startsAt"`
	EndsAt                time.Time                  `json:"endsAt"`
	Status                string                     `json:"status"`
	Comment               *string                    `json:"comment"`
	Version               int64                      `json:"version"`
	HasConflict           bool                       `json:"hasConflict"`
	IsOutsideWorkingHours bool                       `json:"isOutsideWorkingHours"`
	Warnings              []string                   `json:"warnings"`
	CreatedAt             time.Time                  `json:"createdAt"`
	UpdatedAt             time.Time                  `json:"updatedAt"`
}

type appointmentOffice struct {
	ID           uuid.UUID
	Code         string
	Name         string
	TimezoneName string
	Location     *time.Location
}

type appointmentRowScanner interface {
	Scan(dest ...any) error
}

const appointmentSelect = `
	select
	  v.id,
	  l.id,
	  coalesce(l.name, ''),
	  coalesce(l.phone, ''),
	  o.id,
	  o.code,
	  coalesce(o.name_uk, o.code),
	  o.timezone_name,
	  v.responsible_manager_id,
	  coalesce(p.display_name, ''),
	  v.scheduled_at,
	  v.ends_at,
	  v.status,
	  v.comment,
	  v.version,
	  v.created_at,
	  v.updated_at
	from public.lead_showroom_visits v
	join public.leads l on l.id = v.lead_id
	join public.offices o on o.id = l.office_id
	left join public.profiles p on p.id = v.responsible_manager_id
`

const appointmentScheduledEventInsert = `
	insert into public.lead_events (
	  lead_id,
	  actor_id,
	  event_type,
	  event_category,
	  status_code,
	  comment,
	  new_value
	)
	values (
	  $1,$2,'appointment_scheduled','client_status','showroom_invited',$3,
	  jsonb_build_object(
	    'appointment_id',$4::uuid,
	    'starts_at',$5::timestamptz,
	    'ends_at',$6::timestamptz,
	    'responsible_manager_id',$7::uuid
	  )
	)
`

const appointmentChangedEventInsert = `
	insert into public.lead_events (
	  lead_id,
	  actor_id,
	  event_type,
	  event_category,
	  status_code,
	  comment,
	  old_value,
	  new_value
	)
	values (
	  $1,$2,$3,'system',$4,$5,
	  jsonb_build_object(
	    'appointment_id',$6::uuid,
	    'starts_at',$7::timestamptz,
	    'ends_at',$8::timestamptz,
	    'responsible_manager_id',$9::uuid,
	    'status',$10::text,
	    'comment',$11::text
	  ),
	  jsonb_build_object(
	    'appointment_id',$6::uuid,
	    'starts_at',$12::timestamptz,
	    'ends_at',$13::timestamptz,
	    'responsible_manager_id',$14::uuid,
	    'status',$4::text,
	    'comment',$5::text
	  )
	)
`

func (s *Server) handleListAppointments(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("officeId")))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid office", map[string]string{"officeId": "Required"})
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	office, err := s.loadAppointmentOffice(r, officeID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "office_not_found", "Office not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointments_load_failed", "Could not load appointments", nil)
		return
	}

	fromLocal, errFrom := time.ParseInLocation(appointmentDateLayout, r.URL.Query().Get("from"), office.Location)
	toLocal, errTo := time.ParseInLocation(appointmentDateLayout, r.URL.Query().Get("to"), office.Location)
	if errFrom != nil || errTo != nil || !toLocal.After(fromLocal) || toLocal.Sub(fromLocal) > 63*24*time.Hour {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment date range", map[string]string{
			"from": "Use YYYY-MM-DD",
			"to":   "Use an exclusive YYYY-MM-DD date within 63 days",
		})
		return
	}

	var managerID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("managerId")); raw != "" {
		value, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid manager", map[string]string{"managerId": "Invalid id"})
			return
		}
		managerID = &value
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !isAppointmentStatus(status) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment status", map[string]string{"status": "Unknown status"})
		return
	}

	rows, err := s.pool.Query(r.Context(), appointmentSelect+`
		where l.office_id = $1
		  and v.scheduled_at < $3
		  and v.ends_at > $2
		  and ($4::uuid is null or v.responsible_manager_id = $4)
		  and ($5::text = '' or v.status = $5)
		order by v.scheduled_at, v.created_at, v.id
	`, officeID, fromLocal.UTC(), toLocal.UTC(), managerID, status)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointments_load_failed", "Could not load appointments", nil)
		return
	}
	defer rows.Close()

	items := make([]appointment, 0)
	for rows.Next() {
		item, scanErr := scanAppointment(rows)
		if scanErr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "appointments_load_failed", "Could not load appointments", nil)
			return
		}
		item.IsOutsideWorkingHours = appointmentOutsideWorkingHours(item.StartsAt, item.EndsAt, office.Location)
		item.HasConflict, err = s.appointmentHasConflict(r, item.ID, item.ResponsibleManager, item.StartsAt, item.EndsAt)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "appointments_load_failed", "Could not load appointments", nil)
			return
		}
		item.Warnings = appointmentWarnings(item.HasConflict, item.IsOutsideWorkingHours)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointments_load_failed", "Could not load appointments", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"timezone": office.TimezoneName,
		"from":     r.URL.Query().Get("from"),
		"to":       r.URL.Query().Get("to"),
	})
}

func (s *Server) handleCreateAppointment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment body", nil)
		return
	}
	var req appointmentMutationRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment body", nil)
		return
	}
	if fields := validateCreateAppointment(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Appointment validation failed", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointment_create_failed", "Could not create appointment", nil)
		return
	}
	defer tx.Rollback(r.Context())

	hashBytes := sha256.Sum256(body)
	cached, status, cachedBody, err := claimIdempotency(
		r,
		tx,
		actor.ID,
		"appointment.create",
		key,
		hex.EncodeToString(hashBytes[:]),
	)
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "appointment_create_failed", "Could not create appointment", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}

	item, createErr := s.createAppointment(r, tx, actor, req)
	if createErr != nil {
		s.writeAppointmentMutationError(w, r, createErr, "appointment_create_failed")
		return
	}
	response := map[string]any{"appointment": item, "warnings": item.Warnings}
	rawResponse, _ := json.Marshal(response)
	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, "appointment.create", key, http.StatusCreated, rawResponse); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointment_create_failed", "Could not create appointment", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointment_create_failed", "Could not create appointment", nil)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleUpdateAppointment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	appointmentID, err := uuid.Parse(r.PathValue("appointmentId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment id", nil)
		return
	}
	version, ok := parseIfMatch(r)
	if !ok {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match appointment version is required", nil)
		return
	}
	var req appointmentMutationRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid appointment body", nil)
		return
	}
	if fields := validateUpdateAppointment(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Appointment validation failed", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointment_update_failed", "Could not update appointment", nil)
		return
	}
	defer tx.Rollback(r.Context())
	item, updateErr := s.updateAppointment(r, tx, actor, appointmentID, version, req)
	if updateErr != nil {
		s.writeAppointmentMutationError(w, r, updateErr, "appointment_update_failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "appointment_update_failed", "Could not update appointment", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"appointment": item, "warnings": item.Warnings})
}

var (
	errAppointmentNotFound       = errors.New("appointment not found")
	errAppointmentLeadNotFound   = errors.New("appointment lead not found")
	errAppointmentOfficeDenied   = errors.New("appointment office denied")
	errAppointmentManagerInvalid = errors.New("appointment manager invalid")
	errAppointmentAlreadyActive  = errors.New("appointment already active")
	errAppointmentTerminal       = errors.New("appointment terminal")
	errAppointmentVersion        = errors.New("appointment version conflict")
)

func (s *Server) createAppointment(
	r *http.Request,
	tx pgx.Tx,
	actor Actor,
	req appointmentMutationRequest,
) (appointment, error) {
	var officeID uuid.UUID
	var assignedTo *uuid.UUID
	var clientStatus string
	var archivedAt *time.Time
	err := tx.QueryRow(r.Context(), `
		select office_id, assigned_to, client_status, archived_at
		from public.leads
		where id=$1
		for update
	`, req.LeadID).Scan(&officeID, &assignedTo, &clientStatus, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || archivedAt != nil {
		return appointment{}, errAppointmentLeadNotFound
	}
	if err != nil {
		return appointment{}, err
	}
	if !actor.CanAccessOffice(officeID) {
		return appointment{}, errAppointmentOfficeDenied
	}
	if clientStatus == "closed_lost" || clientStatus == "contract_signed" {
		return appointment{}, errAppointmentLeadNotFound
	}
	office, err := loadAppointmentOfficeTx(r, tx, officeID)
	if err != nil {
		return appointment{}, err
	}
	startsAt, err := parseAppointmentLocal(*req.StartsAtLocal, office.Location)
	if err != nil {
		return appointment{}, fmt.Errorf("starts_at_local: %w", err)
	}
	endsAt := startsAt.Add(time.Duration(*req.DurationMinutes) * time.Minute)
	if err := validateAppointmentManager(r, tx, *req.ResponsibleManagerID, officeID); err != nil {
		return appointment{}, err
	}
	var activeExists bool
	if err := tx.QueryRow(r.Context(), `
		select exists(
		  select 1 from public.lead_showroom_visits
		  where lead_id=$1 and status='scheduled'
		)
	`, req.LeadID).Scan(&activeExists); err != nil {
		return appointment{}, err
	}
	if activeExists {
		return appointment{}, errAppointmentAlreadyActive
	}

	var appointmentID uuid.UUID
	err = tx.QueryRow(r.Context(), `
		insert into public.lead_showroom_visits (
		  lead_id,
		  scheduled_at,
		  ends_at,
		  status,
		  comment,
		  responsible_manager_id,
		  created_by,
		  updated_by
		)
		values ($1,$2,$3,'scheduled',$4,$5,$6,$6)
		returning id
	`, req.LeadID, startsAt, endsAt, cleanOptional(req.Comment), *req.ResponsibleManagerID, actor.ID).Scan(&appointmentID)
	if err != nil {
		return appointment{}, err
	}
	nextAssignee := assignedTo
	if nextAssignee == nil {
		nextAssignee = req.ResponsibleManagerID
	}
	if _, err := tx.Exec(r.Context(), `
		update public.leads
		set
		  client_status='showroom_invited',
		  client_status_changed_at=now(),
		  assigned_to=$2,
		  callback_due_at=case
		    when call_status='callback_requested' then callback_due_at
		    else null
		  end,
		  updated_at=now(),
		  version=version+1
		where id=$1
	`, req.LeadID, nextAssignee); err != nil {
		return appointment{}, err
	}
	if _, err := tx.Exec(
		r.Context(),
		appointmentScheduledEventInsert,
		req.LeadID,
		actor.ID,
		cleanOptional(req.Comment),
		appointmentID,
		startsAt,
		endsAt,
		*req.ResponsibleManagerID,
	); err != nil {
		return appointment{}, err
	}
	return loadAppointmentTx(r, tx, appointmentID)
}

func (s *Server) updateAppointment(
	r *http.Request,
	tx pgx.Tx,
	actor Actor,
	appointmentID uuid.UUID,
	version int64,
	req appointmentMutationRequest,
) (appointment, error) {
	var leadID, officeID uuid.UUID
	var currentStart, currentEnd time.Time
	var currentManager *uuid.UUID
	var currentComment *string
	var currentStatus string
	var currentVersion int64
	err := tx.QueryRow(r.Context(), `
		select
		  v.lead_id,
		  l.office_id,
		  v.scheduled_at,
		  v.ends_at,
		  v.responsible_manager_id,
		  v.comment,
		  v.status,
		  v.version
		from public.lead_showroom_visits v
		join public.leads l on l.id=v.lead_id
		where v.id=$1
		for update of v, l
	`, appointmentID).Scan(
		&leadID,
		&officeID,
		&currentStart,
		&currentEnd,
		&currentManager,
		&currentComment,
		&currentStatus,
		&currentVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return appointment{}, errAppointmentNotFound
	}
	if err != nil {
		return appointment{}, err
	}
	if !actor.CanAccessOffice(officeID) {
		return appointment{}, errAppointmentOfficeDenied
	}
	if currentVersion != version {
		return appointment{}, errAppointmentVersion
	}
	if currentStatus != appointmentStatusScheduled {
		return appointment{}, errAppointmentTerminal
	}

	nextStart := currentStart
	nextEnd := currentEnd
	nextManager := currentManager
	nextComment := currentComment
	nextStatus := currentStatus
	if req.StartsAtLocal != nil {
		office, loadErr := loadAppointmentOfficeTx(r, tx, officeID)
		if loadErr != nil {
			return appointment{}, loadErr
		}
		nextStart, err = parseAppointmentLocal(*req.StartsAtLocal, office.Location)
		if err != nil {
			return appointment{}, fmt.Errorf("starts_at_local: %w", err)
		}
		duration := int(currentEnd.Sub(currentStart) / time.Minute)
		if req.DurationMinutes != nil {
			duration = *req.DurationMinutes
		}
		nextEnd = nextStart.Add(time.Duration(duration) * time.Minute)
	} else if req.DurationMinutes != nil {
		nextEnd = nextStart.Add(time.Duration(*req.DurationMinutes) * time.Minute)
	}
	if req.ResponsibleManagerID != nil {
		if err := validateAppointmentManager(r, tx, *req.ResponsibleManagerID, officeID); err != nil {
			return appointment{}, err
		}
		nextManager = req.ResponsibleManagerID
	}
	if req.Comment != nil {
		nextComment = cleanOptional(req.Comment)
	}
	if req.Status != nil {
		nextStatus = *req.Status
	}

	result, err := tx.Exec(r.Context(), `
		update public.lead_showroom_visits
		set
		  scheduled_at=$2,
		  ends_at=$3,
		  responsible_manager_id=$4,
		  comment=$5,
		  status=$6,
		  updated_by=$7,
		  updated_at=now(),
		  version=version+1
		where id=$1 and version=$8
	`, appointmentID, nextStart, nextEnd, nextManager, nextComment, nextStatus, actor.ID, version)
	if err != nil {
		return appointment{}, err
	}
	if result.RowsAffected() != 1 {
		return appointment{}, errAppointmentVersion
	}

	eventType := "appointment_updated"
	if nextStatus != currentStatus {
		eventType = "appointment_status_changed"
	} else if !nextStart.Equal(currentStart) || !nextEnd.Equal(currentEnd) || !sameUUID(nextManager, currentManager) {
		eventType = "appointment_rescheduled"
	}
	if _, err := tx.Exec(r.Context(), appointmentChangedEventInsert,
		leadID, actor.ID, eventType, nextStatus, nextComment, appointmentID,
		currentStart, currentEnd, currentManager, currentStatus, currentComment,
		nextStart, nextEnd, nextManager,
	); err != nil {
		return appointment{}, err
	}
	return loadAppointmentTx(r, tx, appointmentID)
}

func (s *Server) writeAppointmentMutationError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	fallbackCode string,
) {
	switch {
	case errors.Is(err, errAppointmentNotFound):
		s.writeError(w, r, http.StatusNotFound, "appointment_not_found", "Appointment not found", nil)
	case errors.Is(err, errAppointmentLeadNotFound):
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Active lead not found", nil)
	case errors.Is(err, errAppointmentOfficeDenied):
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
	case errors.Is(err, errAppointmentManagerInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid responsible manager", map[string]string{"responsibleManagerId": "Manager must be active in this office"})
	case errors.Is(err, errAppointmentAlreadyActive):
		s.writeError(w, r, http.StatusConflict, "active_appointment_exists", "Lead already has a scheduled appointment", nil)
	case errors.Is(err, errAppointmentTerminal):
		s.writeError(w, r, http.StatusConflict, "appointment_terminal", "Completed appointment cannot be edited", nil)
	case errors.Is(err, errAppointmentVersion):
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Appointment was changed by another user", nil)
	case strings.HasPrefix(err.Error(), "starts_at_local:"):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid local appointment time", map[string]string{"startsAtLocal": "Time does not exist in the office timezone"})
	default:
		s.logger.Error(
			"appointment mutation failed",
			"error", err,
			"code", fallbackCode,
			"request_id", requestID(r.Context()),
		)
		s.writeError(w, r, http.StatusInternalServerError, fallbackCode, "Could not save appointment", nil)
	}
}

func validateCreateAppointment(req appointmentMutationRequest) map[string]string {
	fields := map[string]string{}
	if req.LeadID == uuid.Nil {
		fields["leadId"] = "Required"
	}
	if req.StartsAtLocal == nil || strings.TrimSpace(*req.StartsAtLocal) == "" {
		fields["startsAtLocal"] = "Required"
	}
	if req.DurationMinutes == nil || !validAppointmentDuration(*req.DurationMinutes) {
		fields["durationMinutes"] = "Use 15–480 minutes in 15-minute increments"
	}
	if req.ResponsibleManagerID == nil || *req.ResponsibleManagerID == uuid.Nil {
		fields["responsibleManagerId"] = "Required"
	}
	if req.Status != nil {
		fields["status"] = "Status is assigned automatically"
	}
	return fields
}

func validateUpdateAppointment(req appointmentMutationRequest) map[string]string {
	fields := map[string]string{}
	if req.LeadID != uuid.Nil {
		fields["leadId"] = "Lead cannot be changed"
	}
	if req.StartsAtLocal != nil && strings.TrimSpace(*req.StartsAtLocal) == "" {
		fields["startsAtLocal"] = "Cannot be blank"
	}
	if req.DurationMinutes != nil && !validAppointmentDuration(*req.DurationMinutes) {
		fields["durationMinutes"] = "Use 15–480 minutes in 15-minute increments"
	}
	if req.ResponsibleManagerID != nil && *req.ResponsibleManagerID == uuid.Nil {
		fields["responsibleManagerId"] = "Invalid manager"
	}
	if req.Status != nil && !isAppointmentTerminalStatus(*req.Status) {
		fields["status"] = "Use visited, no_show, or canceled"
	}
	if req.StartsAtLocal == nil && req.DurationMinutes == nil && req.ResponsibleManagerID == nil && req.Comment == nil && req.Status == nil {
		fields["appointment"] = "At least one change is required"
	}
	return fields
}

func validAppointmentDuration(minutes int) bool {
	return minutes >= 15 && minutes <= 480 && minutes%15 == 0
}

func isAppointmentStatus(status string) bool {
	switch status {
	case "scheduled", "visited", "no_show", "canceled", "rescheduled":
		return true
	default:
		return false
	}
}

func isAppointmentTerminalStatus(status string) bool {
	return status == "visited" || status == "no_show" || status == "canceled"
}

func parseAppointmentLocal(value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	parsed, err := time.ParseInLocation(appointmentLocalLayout, value, location)
	if err != nil {
		return time.Time{}, err
	}
	// ParseInLocation normalizes a time inside a DST gap. Reject that silent
	// normalization so the UI can ask the manager to choose a real local time.
	if parsed.In(location).Format(appointmentLocalLayout) != value {
		return time.Time{}, errors.New("local time is inside a timezone transition")
	}
	return parsed.UTC(), nil
}

func appointmentOutsideWorkingHours(startsAt, endsAt time.Time, location *time.Location) bool {
	start := startsAt.In(location)
	end := endsAt.In(location)
	if start.Weekday() == time.Sunday || end.Weekday() == time.Sunday || start.YearDay() != end.YearDay() {
		return true
	}
	open := time.Date(start.Year(), start.Month(), start.Day(), 9, 0, 0, 0, location)
	closeAt := time.Date(start.Year(), start.Month(), start.Day(), 19, 0, 0, 0, location)
	return start.Before(open) || end.After(closeAt)
}

func appointmentWarnings(conflict, outside bool) []string {
	warnings := make([]string, 0, 2)
	if conflict {
		warnings = append(warnings, "manager_overlap")
	}
	if outside {
		warnings = append(warnings, "outside_working_hours")
	}
	return warnings
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	return clean(*value)
}

func sameUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (s *Server) loadAppointmentOffice(r *http.Request, officeID uuid.UUID) (appointmentOffice, error) {
	var office appointmentOffice
	err := s.pool.QueryRow(r.Context(), `
		select id,code,coalesce(name_uk,code),timezone_name
		from public.offices
		where id=$1 and is_active=true
	`, officeID).Scan(&office.ID, &office.Code, &office.Name, &office.TimezoneName)
	if err != nil {
		return appointmentOffice{}, err
	}
	office.Location, err = time.LoadLocation(office.TimezoneName)
	return office, err
}

func loadAppointmentOfficeTx(r *http.Request, tx pgx.Tx, officeID uuid.UUID) (appointmentOffice, error) {
	var office appointmentOffice
	err := tx.QueryRow(r.Context(), `
		select id,code,coalesce(name_uk,code),timezone_name
		from public.offices
		where id=$1 and is_active=true
	`, officeID).Scan(&office.ID, &office.Code, &office.Name, &office.TimezoneName)
	if err != nil {
		return appointmentOffice{}, err
	}
	office.Location, err = time.LoadLocation(office.TimezoneName)
	return office, err
}

func validateAppointmentManager(
	r *http.Request,
	tx pgx.Tx,
	managerID uuid.UUID,
	officeID uuid.UUID,
) error {
	var valid bool
	err := tx.QueryRow(r.Context(), `
		select exists(
		  select 1
		  from public.profiles p
		  join public.user_office_memberships m on m.user_id=p.id
		  where p.id=$1 and p.is_active=true and m.office_id=$2
		)
	`, managerID, officeID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errAppointmentManagerInvalid
	}
	return nil
}

// scheduleLegacyAppointment keeps the temporary showroom_invited activity
// compatible while routing its date through the same canonical appointment
// row used by the calendar. New clients should use /v1/appointments.
func scheduleLegacyAppointment(
	r *http.Request,
	tx pgx.Tx,
	leadID uuid.UUID,
	officeID uuid.UUID,
	actorID uuid.UUID,
	preferredManagerID uuid.UUID,
	startsAt time.Time,
) (uuid.UUID, uuid.UUID, error) {
	managerID := preferredManagerID
	if err := validateAppointmentManager(r, tx, managerID, officeID); err != nil {
		managerID = actorID
		if err := validateAppointmentManager(r, tx, managerID, officeID); err != nil {
			return uuid.Nil, uuid.Nil, err
		}
	}
	startsAt = startsAt.UTC()
	endsAt := startsAt.Add(time.Hour)
	var appointmentID uuid.UUID
	err := tx.QueryRow(r.Context(), `
		select id
		from public.lead_showroom_visits
		where lead_id=$1 and status='scheduled'
		for update
	`, leadID).Scan(&appointmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(r.Context(), `
			insert into public.lead_showroom_visits (
			  lead_id,
			  scheduled_at,
			  ends_at,
			  status,
			  responsible_manager_id,
			  created_by,
			  updated_by
			)
			values ($1,$2,$3,'scheduled',$4,$5,$5)
			returning id
		`, leadID, startsAt, endsAt, managerID, actorID).Scan(&appointmentID)
		return appointmentID, managerID, err
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	_, err = tx.Exec(r.Context(), `
		update public.lead_showroom_visits
		set
		  scheduled_at=$2,
		  ends_at=$3,
		  responsible_manager_id=$4,
		  updated_by=$5,
		  updated_at=now(),
		  version=version+1
		where id=$1
	`, appointmentID, startsAt, endsAt, managerID, actorID)
	return appointmentID, managerID, err
}

func scanAppointment(row appointmentRowScanner) (appointment, error) {
	var item appointment
	var managerID *uuid.UUID
	var managerName string
	err := row.Scan(
		&item.ID,
		&item.Lead.ID,
		&item.Lead.Name,
		&item.Lead.Phone,
		&item.Office.ID,
		&item.Office.Code,
		&item.Office.Name,
		&item.Office.TimezoneName,
		&managerID,
		&managerName,
		&item.StartsAt,
		&item.EndsAt,
		&item.Status,
		&item.Comment,
		&item.Version,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if err != nil {
		return appointment{}, err
	}
	if managerID != nil {
		item.ResponsibleManager = &appointmentManagerSummary{ID: *managerID, DisplayName: managerName}
	}
	return item, nil
}

func loadAppointmentTx(r *http.Request, tx pgx.Tx, appointmentID uuid.UUID) (appointment, error) {
	item, err := scanAppointment(tx.QueryRow(r.Context(), appointmentSelect+` where v.id=$1`, appointmentID))
	if err != nil {
		return appointment{}, err
	}
	location, err := time.LoadLocation(item.Office.TimezoneName)
	if err != nil {
		return appointment{}, err
	}
	item.IsOutsideWorkingHours = appointmentOutsideWorkingHours(item.StartsAt, item.EndsAt, location)
	if item.ResponsibleManager != nil {
		err = tx.QueryRow(r.Context(), `
			select exists(
			  select 1
			  from public.lead_showroom_visits other
			  where other.id <> $1
			    and other.status='scheduled'
			    and other.responsible_manager_id=$2
			    and other.scheduled_at < $4
			    and other.ends_at > $3
			)
		`, item.ID, item.ResponsibleManager.ID, item.StartsAt, item.EndsAt).Scan(&item.HasConflict)
		if err != nil {
			return appointment{}, err
		}
	}
	item.Warnings = appointmentWarnings(item.HasConflict, item.IsOutsideWorkingHours)
	return item, nil
}

func (s *Server) appointmentHasConflict(
	r *http.Request,
	appointmentID uuid.UUID,
	manager *appointmentManagerSummary,
	startsAt time.Time,
	endsAt time.Time,
) (bool, error) {
	if manager == nil {
		return false, nil
	}
	var conflict bool
	err := s.pool.QueryRow(r.Context(), `
		select exists(
		  select 1
		  from public.lead_showroom_visits other
		  where other.id <> $1
		    and other.status='scheduled'
		    and other.responsible_manager_id=$2
		    and other.scheduled_at < $4
		    and other.ends_at > $3
		)
	`, appointmentID, manager.ID, startsAt, endsAt).Scan(&conflict)
	return conflict, err
}
