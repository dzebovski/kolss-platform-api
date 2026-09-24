package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	activityCallStatus    = "call_status"
	activityClientStatus  = "client_status"
	activityComment       = "comment"
	activityClearReminder = "clear_reminder"
	activityReopen        = "reopen"
	activityQuestion      = "question"
	// activityRating sets the CRM v2 lead rating (Cold / Medium / Hot).
	activityRating = "rating"

	reminderKindCallback    = "callback"
	reminderKindThinking    = "thinking"
	reminderKindComment     = "comment"
	reminderKindShowroom    = "showroom"
	reminderKindMeasurement = "measurement"
)

type leadActivityRequest struct {
	Type           string            `json:"type"`
	Status         string            `json:"status"`
	Kind           string            `json:"kind"`
	Comment        string            `json:"comment"`
	DueAt          *time.Time        `json:"dueAt"`
	AssignedTo     *uuid.UUID        `json:"assignedTo"`
	AssigneeIDs    []uuid.UUID       `json:"assigneeIds"`
	Translations   map[string]string `json:"translations"`
	Reason         string            `json:"reason"`
	ContractNumber string            `json:"contractNumber"`
	Amount         *float64          `json:"amount"`
	Currency       string            `json:"currency"`
	Rating         string            `json:"rating"`
	// v2_status (Successful call answers); rejected by the other activity types.
	EstimatedBudgetText     *string  `json:"estimatedBudgetText"`
	EstimatedBudgetCurrency string   `json:"estimatedBudgetCurrency"`
	CityRegion              *string  `json:"cityRegion"`
	Products                []string `json:"products"`
	NextAction              string   `json:"nextAction"`
}

type questionAssigneeSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// commentAssigneeExistsQuery verifies a comment task assignee is an active,
// non–super_admin profile that belongs to the lead's office.
const commentAssigneeExistsQuery = `
	select exists(
	  select 1
	  from public.profiles p
	  join public.user_office_memberships m on m.user_id = p.id
	  where p.id = $1
	    and p.is_active = true
	    and p.role <> 'super_admin'
	    and m.office_id = $2
	)
`

var errCommentAssigneeInvalid = errors.New("comment assignee invalid")

func validateCommentAssignee(r *http.Request, tx pgx.Tx, assigneeID, officeID uuid.UUID) error {
	var valid bool
	if err := tx.QueryRow(r.Context(), commentAssigneeExistsQuery, assigneeID, officeID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errCommentAssigneeInvalid
	}
	return nil
}

// loadQuestionAssignees deliberately permits only office_member profiles. A
// question is not a lead assignment and must never make an admin/curator into
// a manager task owner.
func loadQuestionAssignees(r *http.Request, tx pgx.Tx, assigneeIDs []uuid.UUID, officeID uuid.UUID) ([]questionAssigneeSnapshot, error) {
	seen := make(map[uuid.UUID]struct{}, len(assigneeIDs))
	items := make([]questionAssigneeSnapshot, 0, len(assigneeIDs))
	for _, id := range assigneeIDs {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		var name *string
		err := tx.QueryRow(r.Context(), `
			select p.display_name
			from public.profiles p
			join public.user_office_memberships m on m.user_id=p.id
			where p.id=$1 and p.is_active=true and p.role='office_member' and m.office_id=$2
		`, id, officeID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errCommentAssigneeInvalid
		}
		if err != nil {
			return nil, err
		}
		items = append(items, questionAssigneeSnapshot{ID: id.String(), Name: strings.TrimSpace(derefString(name))})
	}
	return items, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func normalizeQuestionTranslations(translations map[string]string) (map[string]string, map[string]string) {
	if len(translations) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(translations))
	fields := map[string]string{}
	for language, text := range translations {
		lang := strings.ToUpper(strings.TrimSpace(language))
		if lang != "UK" && lang != "PL" && lang != "EN" {
			fields["translations"] = "Languages must be UK, PL, or EN"
			continue
		}
		if value := strings.TrimSpace(text); value == "" {
			fields["translations"] = "Translations cannot be empty"
		} else {
			result[lang] = value
		}
	}
	if len(fields) > 0 {
		return nil, fields
	}
	return result, nil
}

// applyCommentActivityValues sets the comment-specific keys on the lead_events
// new_value payload: an optional reminder date and an optional task assignee.
func applyCommentActivityValues(req leadActivityRequest, newValue map[string]any) {
	if req.DueAt != nil {
		newValue["callback_due_at"] = req.DueAt
	}
	if req.AssignedTo != nil {
		newValue["assigned_to"] = req.AssignedTo
	}
}

type activityLead struct {
	OfficeID       uuid.UUID
	AssignedTo     *uuid.UUID
	CallStatus     *string
	ClientStatus   string
	CallbackDueAt  *time.Time
	ArchivedAt     *time.Time
	CurrentVersion int64
	Rating         *string
	// CRM v2 workflow
	V2Status         *string
	NoAnswerAttempts int
	Budget           *float64
	BudgetCurrency   string
	OfficeCode       string
}

func (s *Server) handleDeprecatedLeadAction(w http.ResponseWriter, r *http.Request) {
	s.writeError(
		w,
		r,
		http.StatusGone,
		"workflow_retired",
		"Workflow actions are retired; use lead activities",
		nil,
	)
}

func (s *Server) handleLeadActivity(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid activity body", nil)
		return
	}
	var req leadActivityRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid activity body", nil)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid activity body", nil)
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	req.Status = strings.TrimSpace(req.Status)
	req.Kind = strings.TrimSpace(req.Kind)
	req.Comment = strings.TrimSpace(req.Comment)
	req.Reason = strings.TrimSpace(req.Reason)
	req.ContractNumber = strings.TrimSpace(req.ContractNumber)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	req.Rating = strings.TrimSpace(req.Rating)
	req.EstimatedBudgetCurrency = strings.ToUpper(strings.TrimSpace(req.EstimatedBudgetCurrency))
	req.NextAction = strings.TrimSpace(req.NextAction)
	if fields := validateLeadActivity(req, actor.IsSuperAdmin()); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Activity validation failed", fields)
		return
	}
	if req.Type == activityQuestion {
		if _, fields := normalizeQuestionTranslations(req.Translations); len(fields) > 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Activity validation failed", fields)
			return
		}
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	defer tx.Rollback(r.Context())

	hashBytes := sha256.Sum256(append([]byte(leadID.String()+"|"), body...))
	operation := "lead.activity"
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, operation, key, hex.EncodeToString(hashBytes[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}

	var lead activityLead
	err = tx.QueryRow(r.Context(), `
		select l.office_id, l.assigned_to, l.call_status, l.client_status, l.callback_due_at, l.archived_at,
		  l.version, l.rating, l.v2_status, l.no_answer_attempts, l.estimated_budget,
		  l.estimated_budget_currency, o.code
		from public.leads l join public.offices o on o.id = l.office_id
		where l.id=$1 for update of l
	`, leadID).Scan(
		&lead.OfficeID,
		&lead.AssignedTo,
		&lead.CallStatus,
		&lead.ClientStatus,
		&lead.CallbackDueAt,
		&lead.ArchivedAt,
		&lead.CurrentVersion,
		&lead.Rating,
		&lead.V2Status,
		&lead.NoAnswerAttempts,
		&lead.Budget,
		&lead.BudgetCurrency,
		&lead.OfficeCode,
	)
	if errors.Is(err, pgx.ErrNoRows) || lead.ArchivedAt != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	if !actor.CanAccessOffice(lead.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	if req.Type == activityComment && req.AssignedTo != nil {
		if err := validateCommentAssignee(r, tx, *req.AssignedTo, lead.OfficeID); err != nil {
			if errors.Is(err, errCommentAssigneeInvalid) {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Activity validation failed", map[string]string{"assignedTo": "Manager must be an active member of the lead's office"})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
			return
		}
	}
	var questionAssignees []questionAssigneeSnapshot
	if req.Type == activityQuestion {
		if !actor.CanAskLeadQuestions(lead.OfficeID) {
			s.writeError(w, r, http.StatusForbidden, "forbidden", "Only office admins, curators, and super admins can ask questions", nil)
			return
		}
		questionAssignees, err = loadQuestionAssignees(r, tx, req.AssigneeIDs, lead.OfficeID)
		if err != nil {
			if errors.Is(err, errCommentAssigneeInvalid) {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Activity validation failed", map[string]string{"assigneeIds": "All assigned managers must be active members of the lead's office"})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
			return
		}
	}
	terminal := isTerminalClientStatus(lead.ClientStatus)
	if req.Type == activityReopen {
		if !terminal {
			s.writeError(w, r, http.StatusConflict, "invalid_transition", "Only terminal leads can be reopened", nil)
			return
		}
	} else if terminal && req.Type != activityQuestion {
		s.writeError(w, r, http.StatusConflict, "lead_terminal", "Terminal leads must be reopened before another activity", nil)
		return
	}
	if req.Type == activityRating && lead.Rating != nil && *lead.Rating == req.Rating {
		s.writeError(w, r, http.StatusConflict, "rating_unchanged", "Rating is already selected", nil)
		return
	}
	if clientStatusUnchanged(lead.ClientStatus, req) {
		s.writeError(w, r, http.StatusConflict, "status_unchanged", "Client status is already selected", nil)
		return
	}

	if err := s.applyLeadActivity(r, tx, actor, leadID, lead, req, questionAssignees); err != nil {
		if errors.Is(err, errLossReasonNotFound) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_loss_reason", "Unknown loss reason", map[string]string{"reason": "Unknown loss reason"})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}

	response := map[string]any{"ok": true, "version": lead.CurrentVersion + 1}
	rawResponse, _ := json.Marshal(response)
	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, operation, key, http.StatusOK, rawResponse); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func clientStatusUnchanged(current string, req leadActivityRequest) bool {
	return req.Type == activityClientStatus && req.Status == current && req.Status != "showroom_invited"
}

// isLeadRating reports whether value is a CRM v2 lead rating (leads_rating_check).
func isLeadRating(value string) bool {
	switch value {
	case "cold", "medium", "hot":
		return true
	default:
		return false
	}
}

func validateLeadActivity(req leadActivityRequest, isSuperAdmin bool) map[string]string {
	fields := map[string]string{}
	reject := func(name, value string) {
		if value != "" {
			fields[name] = "Not allowed for this activity type"
		}
	}
	rejectDueAt := func() {
		if req.DueAt != nil {
			fields["dueAt"] = "Not allowed for this status"
		}
	}
	switch req.Type {
	case activityCallStatus:
		reject("kind", req.Kind)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		switch req.Status {
		case "reached":
			rejectDueAt()
			if req.Comment == "" && !isSuperAdmin {
				fields["comment"] = "Required for a successful call"
			}
		case "no_answer":
			rejectDueAt()
		case "callback_requested":
			if req.DueAt == nil {
				fields["dueAt"] = "Required for callback"
			}
		default:
			fields["status"] = "Unknown call status"
		}
	case activityClientStatus:
		reject("kind", req.Kind)
		switch req.Status {
		case "showroom_invited":
			if req.DueAt == nil {
				fields["dueAt"] = "Required for showroom appointment"
			}
			reject("reason", req.Reason)
			reject("comment", req.Comment)
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
		case "calculation_in_progress":
			rejectDueAt()
			reject("reason", req.Reason)
			reject("comment", req.Comment)
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
		case "thinking":
			// comment and dueAt are both optional (indefinite pause is allowed).
			reject("reason", req.Reason)
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
		case "postponed":
			// dueAt is optional, like thinking, but comment is required: postponed
			// leads must record why/what the client said (a concrete future
			// timeframe), not just that they were parked.
			reject("reason", req.Reason)
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
			if req.Comment == "" {
				fields["comment"] = "Required"
			}
		case "closed_lost":
			rejectDueAt()
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
			switch req.Reason {
			case "expensive", "invalid", "other", "no_contact":
			default:
				fields["reason"] = "Must be expensive, invalid, no_contact, or other"
			}
			if req.Comment == "" {
				fields["comment"] = "Required"
			}
		case "contract_signed":
			rejectDueAt()
			reject("reason", req.Reason)
			reject("comment", req.Comment)
			if req.ContractNumber == "" {
				fields["contractNumber"] = "Required"
			}
			if req.Amount == nil || *req.Amount <= 0 {
				fields["amount"] = "Must be greater than zero"
			}
			switch req.Currency {
			case "UAH", "USD", "EUR", "PLN":
			default:
				fields["currency"] = "Must be UAH, USD, EUR, or PLN"
			}
		default:
			fields["status"] = "Unknown client status"
		}
	case activityComment:
		reject("status", req.Status)
		reject("kind", req.Kind)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		if req.Comment == "" {
			fields["comment"] = "Required"
		}
		if req.AssignedTo != nil && req.DueAt == nil {
			fields["dueAt"] = "Required when a manager is assigned"
		}
	case activityClearReminder:
		rejectDueAt()
		reject("status", req.Status)
		reject("comment", req.Comment)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		switch req.Kind {
		case reminderKindCallback,
			reminderKindThinking,
			reminderKindComment,
			reminderKindShowroom,
			reminderKindMeasurement:
		default:
			fields["kind"] = "Must be callback, thinking, comment, showroom, or measurement"
		}
	case activityReopen:
		rejectDueAt()
		reject("status", req.Status)
		reject("kind", req.Kind)
		reject("comment", req.Comment)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
	case activityQuestion:
		rejectDueAt()
		reject("status", req.Status)
		reject("kind", req.Kind)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		if strings.TrimSpace(req.Comment) == "" {
			fields["comment"] = "Required"
		}
	case activityRating:
		rejectDueAt()
		reject("status", req.Status)
		reject("kind", req.Kind)
		reject("comment", req.Comment)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		if !isLeadRating(req.Rating) {
			fields["rating"] = "Must be cold, medium, or hot"
		}
	case activityV2Status:
		reject("kind", req.Kind)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		validateV2StatusActivity(req, fields)
	default:
		fields["type"] = "Unknown activity type"
	}
	if req.Type != activityRating {
		reject("rating", req.Rating)
	}
	if req.Type != activityV2Status {
		for _, name := range v2ActivityFieldsSent(req) {
			fields[name] = "Not allowed for this activity type"
		}
	}
	if req.Type != activityComment && req.AssignedTo != nil {
		fields["assignedTo"] = "Not allowed for this activity type"
	}
	if req.Type != activityQuestion && len(req.AssigneeIDs) > 0 {
		fields["assigneeIds"] = "Not allowed for this activity type"
	}
	if req.Type != activityQuestion && len(req.Translations) > 0 {
		fields["translations"] = "Not allowed for this activity type"
	}
	return fields
}

func (s *Server) applyLeadActivity(r *http.Request, tx pgx.Tx, actor Actor, leadID uuid.UUID, lead activityLead, req leadActivityRequest, questionAssignees []questionAssigneeSnapshot) error {
	now := time.Now().UTC()
	assignedTo := lead.AssignedTo
	if req.Type != activityReopen && req.Type != activityClearReminder && req.Type != activityQuestion && req.Type != activityRating && assignedTo == nil {
		assignedTo = &actor.ID
	}

	eventType := ""
	eventCategory := ""
	statusCode := (*string)(nil)
	comment := clean(req.Comment)
	oldValue := map[string]any{}
	newValue := map[string]any{}
	callStatus := lead.CallStatus
	clientStatus := lead.ClientStatus
	callbackDueAt := lead.CallbackDueAt
	changeCall := false
	changeClient := false
	lossReason := (*string)(nil)
	rating := lead.Rating
	v2Status := lead.V2Status
	noAnswerAttempts := lead.NoAnswerAttempts

	switch req.Type {
	case activityCallStatus:
		eventType = "call_status_changed"
		eventCategory = activityCallStatus
		statusCode = &req.Status
		oldValue["call_status"] = lead.CallStatus
		newValue["call_status"] = req.Status
		callStatus = &req.Status
		changeCall = true
		if req.Status == "callback_requested" {
			callbackDueAt = req.DueAt
			newValue["callback_due_at"] = req.DueAt
		} else if lead.ClientStatus != "thinking" && lead.ClientStatus != "postponed" {
			callbackDueAt = nil
		}
	case activityComment:
		eventType = "comment_added"
		eventCategory = activityComment
		if req.DueAt != nil {
			callbackDueAt = req.DueAt
		}
		applyCommentActivityValues(req, newValue)
	case activityClearReminder:
		eventType = "reminder_cleared"
		newValue["kind"] = req.Kind
		oldValue["kind"] = req.Kind
		if lead.CallbackDueAt != nil {
			oldValue["callback_due_at"] = lead.CallbackDueAt
		}
		switch req.Kind {
		case reminderKindCallback, reminderKindThinking:
			eventCategory = "system"
			callbackDueAt = nil
			newValue["callback_due_at"] = nil
		case reminderKindComment:
			// Latest comment-category event drives comment_reminder_*; a clear
			// event without callback_due_at / assigned_to hides the reminder.
			eventCategory = activityComment
			commentDueAt, err := latestCommentReminderDueAt(r, tx, leadID)
			if err != nil {
				return err
			}
			if commentDueAt != nil {
				oldValue["comment_reminder_due_at"] = commentDueAt
			}
			if shouldClearLeadDueForCommentReminder(lead.CallbackDueAt, commentDueAt) {
				callbackDueAt = nil
				newValue["callback_due_at"] = nil
			}
		case reminderKindShowroom, reminderKindMeasurement:
			// Cancels the scheduled visit of that kind; client_status stays unchanged.
			eventCategory = "system"
			visitKind := appointmentKindShowroom
			valueKey := "showroom_due_at"
			if req.Kind == reminderKindMeasurement {
				visitKind = appointmentKindMeasurement
				valueKey = "measurement_due_at"
			}
			dueAt, err := latestScheduledVisitDueAt(r, tx, leadID, visitKind)
			if err != nil {
				return err
			}
			if dueAt != nil {
				oldValue[valueKey] = dueAt
			}
			newValue[valueKey] = nil
		}
	case activityClientStatus:
		eventType = "client_status_changed"
		eventCategory = activityClientStatus
		statusCode = &req.Status
		oldValue["client_status"] = lead.ClientStatus
		newValue["client_status"] = req.Status
		clientStatus = req.Status
		changeClient = true
		if req.Status == "thinking" || req.Status == "postponed" {
			newValue["callback_due_at"] = req.DueAt
		}
		if req.Status == "showroom_invited" {
			managerID := actor.ID
			if assignedTo != nil {
				managerID = *assignedTo
			}
			appointmentID, actualManagerID, err := scheduleLegacyAppointment(
				r,
				tx,
				leadID,
				lead.OfficeID,
				actor.ID,
				managerID,
				*req.DueAt,
			)
			if err != nil {
				return err
			}
			assignedTo = &actualManagerID
			newValue["appointment_id"] = appointmentID
			newValue["starts_at"] = req.DueAt.UTC()
			newValue["ends_at"] = req.DueAt.UTC().Add(time.Hour)
			newValue["responsible_manager_id"] = actualManagerID
		}
		callbackDueAt = nextClientStatusCallbackDue(
			lead.CallStatus,
			callbackDueAt,
			req.Status,
			req.DueAt,
		)
		if req.Status == "closed_lost" {
			var exists bool
			if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.loss_reasons where code=$1)`, req.Reason).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return errLossReasonNotFound
			}
			lossReason = &req.Reason
			newValue["reason"] = req.Reason
		}
		if req.Status == "contract_signed" {
			rates, err := loadCurrencyRateSetAt(r.Context(), tx, now)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				insert into public.lead_contracts
				  (lead_id,signed_at,status,contract_number,amount,currency,currency_rate_set_id,created_by)
				values ($1,$2,'signed',$3,$4,$5,$6,$7)
			`, leadID, now, req.ContractNumber, req.Amount, req.Currency, rates.ID, actor.ID); err != nil {
				return err
			}
			newValue["contract_number"] = req.ContractNumber
			newValue["amount"] = req.Amount
			newValue["currency"] = req.Currency
			newValue["signed_at"] = now
		}
	case activityReopen:
		eventType = "lead_reopened"
		eventCategory = "system"
		status := "new_lead"
		statusCode = &status
		oldValue["client_status"] = lead.ClientStatus
		oldValue["call_status"] = lead.CallStatus
		newValue["client_status"] = status
		newValue["call_status"] = nil
		clientStatus = status
		callStatus = nil
		changeCall = true
		changeClient = true
		callbackDueAt = nil
	case activityQuestion:
		eventType = "question"
		eventCategory = "question"
		status := "status-question"
		statusCode = &status
		comment = &req.Comment
		translations, _ := normalizeQuestionTranslations(req.Translations)
		if translations == nil {
			translations = map[string]string{}
		}
		newValue["question"] = map[string]any{
			"assignees":    questionAssignees,
			"translations": translations,
			"status":       "pending",
		}
	case activityV2Status:
		// One v2 action = one v1 event: call results write call_status_changed like v1, with the
		// v2 details in new_value (contract §3.2).
		callCode := v2CallResultStatuses[req.Status]
		eventType = "call_status_changed"
		eventCategory = activityCallStatus
		statusCode = &callCode
		oldValue["call_status"] = lead.CallStatus
		oldValue["v2_status"] = lead.V2Status
		newValue["call_status"] = callCode
		newValue["v2_status"] = req.Status
		newValue["due_at"] = req.DueAt
		newValue["callback_due_at"] = req.DueAt
		callStatus = &callCode
		changeCall = true
		callbackDueAt = req.DueAt
		v2Status = &req.Status
		switch req.Status {
		case v2StatusNoAnswer:
			noAnswerAttempts++
			newValue["attempt"] = noAnswerAttempts
		case v2StatusSuccess:
			noAnswerAttempts = 0
			if err := applySuccessfulCallInfo(r.Context(), tx, leadID, lead, req, now, newValue); err != nil {
				return err
			}
		}
	case activityRating:
		eventType = "rating_changed"
		eventCategory = "system"
		oldValue["rating"] = lead.Rating
		newValue["from"] = lead.Rating
		newValue["to"] = req.Rating
		rating = &req.Rating
	}
	// v1 status changes keep v2_status in step (contract §3.6); reopen starts over as new.
	switch req.Type {
	case activityCallStatus, activityClientStatus:
		v2Status = deriveV2LeadStatus(clientStatus, callStatus)
	case activityReopen:
		status := v2StatusNew
		v2Status = &status
	}
	v2Changed := !equalStringPtr(v2Status, lead.V2Status) || req.Type == activityV2Status

	_, err := tx.Exec(r.Context(), `
		update public.leads set
		  call_status=$2,
		  call_status_changed_at=case when $3 then $6 else call_status_changed_at end,
		  client_status=$4,
		  client_status_changed_at=case when $5 then $6 else client_status_changed_at end,
		  assigned_to=$7,
		  loss_reason=case when $8::text is null then loss_reason else $8 end,
		  callback_due_at=$9,
		  rating=$10,
		  v2_status=$11,
		  v2_status_changed_at=case when $12 then $6 else v2_status_changed_at end,
		  no_answer_attempts=$13,
		  updated_at=$6,
		  version=version+1
		where id=$1
	`, leadID, callStatus, changeCall, clientStatus, changeClient, now, assignedTo, lossReason, callbackDueAt, rating, v2Status, v2Changed, noAnswerAttempts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(r.Context(), `
		insert into public.lead_events
		  (lead_id,actor_id,event_type,event_category,status_code,comment,old_value,new_value)
		values ($1,$2,$3,$4,$5,$6,$7,$8)
	`, leadID, actor.ID, eventType, eventCategory, statusCode, comment, oldValue, newValue)
	if err != nil {
		return err
	}
	if req.Type == activityClientStatus && isTerminalClientStatus(req.Status) {
		return cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, leadID)
	}
	if req.Type == activityClearReminder && req.Kind == reminderKindShowroom {
		return cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, leadID, appointmentKindShowroom)
	}
	if req.Type == activityClearReminder && req.Kind == reminderKindMeasurement {
		return cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, leadID, appointmentKindMeasurement)
	}
	return nil
}

// latestCommentReminderDueAt returns the due date on the latest comment-category
// event, if that event stored an explicit callback_due_at string.
func latestCommentReminderDueAt(r *http.Request, tx pgx.Tx, leadID uuid.UUID) (*time.Time, error) {
	var raw *string
	err := tx.QueryRow(r.Context(), `
		select case
			when jsonb_typeof(e.new_value->'callback_due_at') = 'string'
				then e.new_value->>'callback_due_at'
			else null
		end
		from public.lead_events e
		where e.lead_id = $1
			and e.event_category = 'comment'
		order by e.created_at desc
		limit 1
	`, leadID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, *raw)
		if err != nil {
			return nil, nil
		}
	}
	utc := parsed.UTC()
	return &utc, nil
}

// latestScheduledVisitDueAt returns the latest scheduled visit start time of the
// given kind for the lead, matching the lead list showroom_due_at /
// measurement_due_at derivation.
func latestScheduledVisitDueAt(
	r *http.Request,
	tx pgx.Tx,
	leadID uuid.UUID,
	kind string,
) (*time.Time, error) {
	var due *time.Time
	err := tx.QueryRow(r.Context(), `
		select v.scheduled_at
		from public.lead_showroom_visits v
		where v.lead_id = $1
			and v.kind = $2
			and v.status = 'scheduled'
		order by v.scheduled_at desc, v.created_at desc
		limit 1
	`, leadID, kind).Scan(&due)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return due, nil
}

// shouldClearLeadDueForCommentReminder is true when the lead's shared due date
// came from the active comment reminder (same instant).
func shouldClearLeadDueForCommentReminder(leadDue, commentDue *time.Time) bool {
	return leadDue != nil && commentDue != nil && leadDue.Equal(*commentDue)
}

// isTerminalClientStatus reports whether the lead is closed — no further work,
// and therefore no reminders, belong to it.
func isTerminalClientStatus(status string) bool {
	return status == "closed_lost" || status == "contract_signed"
}

func nextClientStatusCallbackDue(
	callStatus *string,
	current *time.Time,
	clientStatus string,
	requested *time.Time,
) *time.Time {
	if isTerminalClientStatus(clientStatus) {
		return nil
	}
	if clientStatus == "thinking" || clientStatus == "postponed" {
		return requested
	}
	if callStatus != nil && *callStatus == "callback_requested" {
		return current
	}
	return nil
}
