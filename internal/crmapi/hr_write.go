package crmapi

// HR portal — writes by the person (or the super admin for them): absences and planned hours
// (HB4). No approvals (D3): every write is saved at once and queued for Slack.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// hrBeginTx owns the request's Idempotency-Key (same contract as the rest of the API).
func (s *Server) hrBeginTx(w http.ResponseWriter, r *http.Request, actor Actor, operation string, hashParts ...string) (pgx.Tx, string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return nil, "", false
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.hrFail(w, r, err)
		return nil, "", false
	}
	hash := sha256.Sum256([]byte(strings.Join(hashParts, "|")))
	cached, status, body, err := claimIdempotency(r, tx, actor.ID, operation, key, hex.EncodeToString(hash[:]))
	if err != nil {
		_ = tx.Rollback(r.Context())
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return nil, "", false
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.hrFail(w, r, err)
			return nil, "", false
		}
		writeJSON(w, status, body)
		return nil, "", false
	}
	return tx, key, true
}

func (s *Server) hrFinishTx(r *http.Request, tx pgx.Tx, actor Actor, operation, key string, status int, value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, s.finishIdempotentTx(r, tx, actor, operation, key, status, raw)
}

// hrRuleError writes a 400 for edit-window / shape rules with the field that failed.
func (s *Server) hrRuleError(w http.ResponseWriter, r *http.Request, code string, fields map[string]string) {
	status := http.StatusBadRequest
	switch code {
	case hrErrForbidden, hrErrNotTracked:
		status = http.StatusForbidden
	case hrErrOverlap:
		status = http.StatusConflict
	}
	s.writeError(w, r, status, code, hrRuleMessage(code), fields)
}

func hrRuleMessage(code string) string {
	switch code {
	case hrErrPastDate:
		return "This date can no longer be changed"
	case hrErrClosedMonth:
		return "The month is closed; only the super admin can change it"
	case hrErrOutsideHours:
		return "Outside office hours"
	case hrErrEndBefore:
		return "End is before start"
	case hrErrCommentNeeded:
		return "A comment is required"
	case hrErrOverlap:
		return "Another absence already covers these dates"
	case hrErrForbidden:
		return "Not allowed"
	case hrErrNotTracked:
		return "Not tracked in HR"
	}
	return "Invalid request"
}

type hrMutationResponse struct {
	Entry    *hrEntryJSON   `json:"entry"`
	Entries  []hrEntryJSON  `json:"entries,omitempty"`
	Balances hrBalancesJSON `json:"balances"`
}

func (s *Server) hrActorName(ctx context.Context, q hrQuerier, actorID uuid.UUID) *string {
	var name *string
	_ = q.QueryRow(ctx, `select display_name from public.profiles where id = $1`, actorID).Scan(&name)
	return name
}

// ---- absences -------------------------------------------------------------------------------

func (s *Server) hrOverlapExists(ctx context.Context, tx pgx.Tx, employeeID uuid.UUID, from, to time.Time, except *uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		select exists(select 1 from public.hr_absences
		  where employee_id = $1 and deleted_at is null and date_from <= $3 and date_to >= $2
		    and ($4::uuid is null or id <> $4))
	`, employeeID, from, to, except).Scan(&exists)
	return exists, err
}

func (s *Server) handleHRCreateAbsence(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	var req hrAbsenceRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid absence", nil)
		return
	}
	target, ok := s.hrTargetEmployee(w, r, v, req.EmployeeID)
	if !ok {
		return
	}
	in, code, fields := hrValidateAbsence(req)
	if code != "" {
		s.hrRuleError(w, r, code, fields)
		return
	}
	today := hrToday(target.TimeZone)
	scope := hrScopeAbsences
	if in.Type == hrTypeMakeupDay {
		scope = hrScopeHours // a make-up day is planned hours (D15, D17)
	}
	if code := hrEditWindowError(v.isAdmin(), scope, today, in.DateFrom, in.DateTo); code != "" {
		s.hrRuleError(w, r, code, map[string]string{"dateFrom": code})
		return
	}
	comment := ""
	if in.Comment != nil {
		comment = *in.Comment
	}
	const operation = "hr.absence.create"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, target.ID.String(), in.Type, hrFormatDate(in.DateFrom), hrFormatDate(in.DateTo), in.Part, comment)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()
	if _, err := hrEmployeeByID(ctx, tx, target.ID, true); err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp := hrMutationResponse{}
	var slack hrSlackMessage
	if in.Type == hrTypeMakeupDay {
		_, code, err := s.hrCreateMakeupShifts(ctx, tx, v.actor, *target, in)
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
		if code != "" {
			s.hrRuleError(w, r, code, map[string]string{"dateFrom": code})
			return
		}
		slack = hrSlackMessage{Icon: "🗓️", Action: "Новий день відпрацювання", Person: target.DisplayName,
			Detail: hrSlackAbsence(hrTypeMakeupDay, in.DateFrom, in.DateTo, in.Part)}
	} else {
		overlap, err := s.hrOverlapExists(ctx, tx, target.ID, in.DateFrom, in.DateTo, nil)
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
		if overlap {
			s.hrRuleError(w, r, hrErrOverlap, map[string]string{"dateFrom": hrErrOverlap})
			return
		}
		if _, err := tx.Exec(ctx, `
			insert into public.hr_absences (employee_id, type, date_from, date_to, part, comment, created_by, updated_by)
			values ($1, $2, $3, $4, $5, $6, $7, $7)
		`, target.ID, in.Type, in.DateFrom, in.DateTo, in.Part, in.Comment, v.actor.ID); err != nil {
			s.hrFail(w, r, err)
			return
		}
		slack = hrSlackMessage{Icon: "🗓️", Action: "Нова відсутність", Person: target.DisplayName,
			Detail: hrSlackAbsence(in.Type, in.DateFrom, in.DateTo, in.Part)}
	}
	slack.Actor = s.hrActorName(ctx, tx, v.actor.ID)
	queued, err := s.hrNotify(ctx, tx, slack)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if err := s.hrFillMutation(ctx, tx, v, *target, in.DateFrom, in.DateTo, &resp); err != nil {
		s.hrFail(w, r, err)
		return
	}
	raw, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusCreated, resp)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusCreated, raw)
}

// hrFillMutation returns the entries touching the changed dates and the new balances.
func (s *Server) hrFillMutation(ctx context.Context, tx pgx.Tx, v hrViewer, e hrEmployee, from, to time.Time, resp *hrMutationResponse) error {
	balances, err := hrLoadBalances(ctx, tx, e, hrToday(e.TimeZone).Year())
	if err != nil {
		return err
	}
	resp.Balances = balances
	// Read inside the transaction so the response matches what was saved.
	entries, err := hrListEntriesWith(ctx, tx, v, e, from, to, "")
	if err != nil {
		return err
	}
	resp.Entries = entries
	if len(entries) > 0 {
		resp.Entry = &entries[0]
	}
	return nil
}

func (s *Server) hrCreateMakeupShifts(ctx context.Context, tx pgx.Tx, actor Actor, e hrEmployee, in hrAbsenceInput) ([]uuid.UUID, string, error) {
	office, err := hrLoadOffice(ctx, tx, e.OfficeID)
	if err != nil || office == nil {
		return nil, "", fmt.Errorf("load office: %w", err)
	}
	holidays, err := hrLoadHolidays(ctx, tx, e.OfficeID, in.DateFrom, in.DateTo)
	if err != nil {
		return nil, "", err
	}
	isHoliday := map[string]bool{}
	for _, h := range holidays {
		isHoliday[h.Day] = true
	}
	ids := []uuid.UUID{}
	for _, day := range hrDatesBetween(in.DateFrom, in.DateTo) {
		pattern, err := hrPatternOn(ctx, tx, e.ID, day)
		if err != nil {
			return nil, "", err
		}
		hours := 8.0
		if pattern != nil {
			hours = pattern.HoursPerDay
		}
		officeDay := office.Hours[hrISOWeekday(day)]
		if isHoliday[hrFormatDate(day)] {
			return nil, hrErrOutsideHours, nil
		}
		start, end, open := hrMakeupShift(officeDay, hours, in.Part != hrPartFull)
		if !open {
			return nil, hrErrOutsideHours, nil
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
			insert into public.hr_shifts (employee_id, work_date, start_time, end_time, kind, created_by, updated_by)
			values ($1, $2, $3::time, $4::time, 'makeup', $5, $5)
			on conflict (employee_id, work_date) do update
			  set start_time = excluded.start_time, end_time = excluded.end_time, kind = 'makeup',
			      updated_by = excluded.updated_by, updated_at = now()
			returning id
		`, e.ID, day, hrFormatClock(start), hrFormatClock(end), actor.ID).Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	return ids, "", nil
}

type hrAbsenceRow struct {
	ID         uuid.UUID
	EmployeeID uuid.UUID
	Type       string
	DateFrom   time.Time
	DateTo     time.Time
	Part       string
}

func (s *Server) hrLoadAbsenceForWrite(w http.ResponseWriter, r *http.Request, v hrViewer) (*hrAbsenceRow, *hrEmployee, bool) {
	id, err := uuid.Parse(r.PathValue("absenceId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Absence not found", nil)
		return nil, nil, false
	}
	var a hrAbsenceRow
	err = s.pool.QueryRow(r.Context(), `
		select id, employee_id, type, date_from, date_to, part from public.hr_absences
		where id = $1 and deleted_at is null
	`, id).Scan(&a.ID, &a.EmployeeID, &a.Type, &a.DateFrom, &a.DateTo, &a.Part)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Absence not found", nil)
		return nil, nil, false
	}
	if err != nil {
		s.hrFail(w, r, err)
		return nil, nil, false
	}
	raw := a.EmployeeID.String()
	e, ok := s.hrTargetEmployee(w, r, v, &raw)
	if !ok {
		return nil, nil, false
	}
	return &a, e, true
}

func (s *Server) handleHRUpdateAbsence(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	var req hrAbsenceRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid absence", nil)
		return
	}
	old, e, ok := s.hrLoadAbsenceForWrite(w, r, v)
	if !ok {
		return
	}
	in, code, fields := hrValidateAbsence(req)
	if code == "" && in.Type == hrTypeMakeupDay {
		code, fields = "validation_error", map[string]string{"type": "validation_error"}
	}
	if code != "" {
		s.hrRuleError(w, r, code, fields)
		return
	}
	today := hrToday(e.TimeZone)
	if code := hrEditWindowError(v.isAdmin(), hrScopeAbsences, today, old.DateFrom, old.DateTo); code != "" {
		s.hrRuleError(w, r, code, map[string]string{"dateFrom": code})
		return
	}
	if code := hrEditWindowError(v.isAdmin(), hrScopeAbsences, today, in.DateFrom, in.DateTo); code != "" {
		s.hrRuleError(w, r, code, map[string]string{"dateFrom": code})
		return
	}
	comment := ""
	if in.Comment != nil {
		comment = *in.Comment
	}
	const operation = "hr.absence.update"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, old.ID.String(), in.Type, hrFormatDate(in.DateFrom), hrFormatDate(in.DateTo), in.Part, comment)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()
	if _, err := hrEmployeeByID(ctx, tx, e.ID, true); err != nil {
		s.hrFail(w, r, err)
		return
	}
	overlap, err := s.hrOverlapExists(ctx, tx, e.ID, in.DateFrom, in.DateTo, &old.ID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if overlap {
		s.hrRuleError(w, r, hrErrOverlap, map[string]string{"dateFrom": hrErrOverlap})
		return
	}
	if _, err := tx.Exec(ctx, `
		update public.hr_absences
		set type = $2, date_from = $3, date_to = $4, part = $5, comment = $6, updated_by = $7, updated_at = now()
		where id = $1
	`, old.ID, in.Type, in.DateFrom, in.DateTo, in.Part, in.Comment, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{
		Icon: "✏️", Action: "Змінено відсутність", Person: e.DisplayName,
		Detail: hrSlackAbsence(in.Type, in.DateFrom, in.DateTo, in.Part),
		Before: hrSlackAbsence(old.Type, old.DateFrom, old.DateTo, old.Part),
		Actor:  s.hrActorName(ctx, tx, v.actor.ID),
	})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp := hrMutationResponse{}
	if err := s.hrFillMutation(ctx, tx, v, *e, in.DateFrom, in.DateTo, &resp); err != nil {
		s.hrFail(w, r, err)
		return
	}
	for i := range resp.Entries {
		if resp.Entries[i].ID == old.ID {
			resp.Entry = &resp.Entries[i]
		}
	}
	raw, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, resp)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, raw)
}

func (s *Server) handleHRDeleteAbsence(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	old, e, ok := s.hrLoadAbsenceForWrite(w, r, v)
	if !ok {
		return
	}
	if code := hrEditWindowError(v.isAdmin(), hrScopeAbsences, hrToday(e.TimeZone), old.DateFrom, old.DateTo); code != "" {
		s.hrRuleError(w, r, code, map[string]string{"dateFrom": code})
		return
	}
	const operation = "hr.absence.delete"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, old.ID.String())
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()
	if _, err := tx.Exec(ctx, `
		update public.hr_absences set deleted_at = now(), deleted_by = $2, updated_at = now(), updated_by = $2
		where id = $1 and deleted_at is null
	`, old.ID, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{
		Icon: "🗑️", Action: "Видалено відсутність", Person: e.DisplayName,
		Detail: hrSlackAbsence(old.Type, old.DateFrom, old.DateTo, old.Part),
		Actor:  s.hrActorName(ctx, tx, v.actor.ID),
	})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp := hrMutationResponse{}
	balances, err := hrLoadBalances(ctx, tx, *e, hrToday(e.TimeZone).Year())
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp.Balances = balances
	raw, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, resp)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, raw)
}

// ---- planned hours (PUT /v1/hr/shifts) -------------------------------------------------------

type hrShiftDayRequest struct {
	Date      string `json:"date"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Kind      string `json:"kind"`
}

type hrShiftWeekRequest struct {
	Days []hrShiftDayRequest `json:"days"`
}

type hrShiftWeekResponse struct {
	WeekStart string        `json:"weekStart"`
	Cells     []hrCellJSON  `json:"cells"`
	Summary   hrWeekSummary `json:"summary"`
}

type hrShiftIn struct {
	Day   time.Time
	Start int
	End   int
	Kind  string
}

func (s *Server) handleHRPutShifts(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("employeeId")
	e, ok := s.hrTargetEmployee(w, r, v, &raw)
	if !ok {
		return
	}
	weekStart, okWeek := hrParseDate(r.URL.Query().Get("weekStart"))
	if !okWeek || hrISOWeekday(weekStart) != 1 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "weekStart must be a Monday", map[string]string{"weekStart": "validation_error"})
		return
	}
	weekEnd := weekStart.AddDate(0, 0, 6)
	var req hrShiftWeekRequest
	if err := decodeJSON(w, r, 32*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid week", nil)
		return
	}
	ctx := r.Context()
	office, err := hrLoadOffice(ctx, s.pool, e.OfficeID)
	if err != nil || office == nil {
		s.hrFail(w, r, err)
		return
	}
	holidays, err := hrLoadHolidays(ctx, s.pool, e.OfficeID, weekStart, weekEnd)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	isHoliday := map[string]bool{}
	for _, h := range holidays {
		isHoliday[h.Day] = true
	}
	today := hrToday(e.TimeZone)
	fields := map[string]string{}
	firstCode := ""
	setErr := func(field, code string) {
		fields[field] = code
		if firstCode == "" {
			firstCode = code
		}
	}
	shifts := []hrShiftIn{}
	seen := map[string]bool{}
	for _, d := range req.Days {
		day, okDay := hrParseDate(d.Date)
		field := "days." + strings.TrimSpace(d.Date)
		if !okDay || day.Before(weekStart) || day.After(weekEnd) || seen[d.Date] {
			setErr(field, "validation_error")
			continue
		}
		seen[d.Date] = true
		kind := strings.TrimSpace(d.Kind)
		if kind == "" {
			kind = "planned"
		}
		if kind != "planned" && kind != "makeup" {
			setErr(field, "validation_error")
			continue
		}
		start, okS := hrParseClock(d.StartTime)
		end, okE := hrParseClock(d.EndTime)
		if !okS || !okE {
			setErr(field, "validation_error")
			continue
		}
		if code := hrEditWindowError(v.isAdmin(), hrScopeHours, today, day); code != "" {
			setErr(field, code)
			continue
		}
		if code := hrShiftWithinOffice(office.Hours[hrISOWeekday(day)], isHoliday[d.Date], start, end); code != "" {
			setErr(field, code)
			continue
		}
		shifts = append(shifts, hrShiftIn{Day: day, Start: start, End: end, Kind: kind})
	}
	if firstCode != "" {
		s.hrRuleError(w, r, firstCode, fields)
		return
	}
	// The editable part of the week is replaced; locked days are left as they are.
	editFrom := weekStart
	if !v.isAdmin() {
		if tomorrow := today.AddDate(0, 0, 1); tomorrow.After(editFrom) {
			editFrom = tomorrow
		}
	}
	sort.Slice(shifts, func(i, j int) bool { return shifts[i].Day.Before(shifts[j].Day) })
	hashParts := []string{e.ID.String(), hrFormatDate(weekStart)}
	for _, sh := range shifts {
		hashParts = append(hashParts, fmt.Sprintf("%s %d %d %s", hrFormatDate(sh.Day), sh.Start, sh.End, sh.Kind))
	}
	const operation = "hr.shifts.put"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, hashParts...)
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	if _, err := hrEmployeeByID(ctx, tx, e.ID, true); err != nil {
		s.hrFail(w, r, err)
		return
	}
	// A fixed schedule is set by the super admin (D6); the person may only add make-up days.
	if !v.isAdmin() {
		for _, sh := range shifts {
			if sh.Kind != "planned" {
				continue
			}
			pattern, err := hrPatternOn(ctx, tx, e.ID, sh.Day)
			if err != nil {
				s.hrFail(w, r, err)
				return
			}
			if pattern == nil || pattern.Kind != "flexible" {
				s.hrRuleError(w, r, hrErrForbidden, map[string]string{"days." + hrFormatDate(sh.Day): hrErrForbidden})
				return
			}
		}
	}
	if !editFrom.After(weekEnd) {
		if _, err := tx.Exec(ctx, `delete from public.hr_shifts where employee_id = $1 and work_date between $2 and $3`, e.ID, editFrom, weekEnd); err != nil {
			s.hrFail(w, r, err)
			return
		}
	}
	parts := []string{}
	for _, sh := range shifts {
		if _, err := tx.Exec(ctx, `
			insert into public.hr_shifts (employee_id, work_date, start_time, end_time, kind, created_by, updated_by)
			values ($1, $2, $3::time, $4::time, $5, $6, $6)
		`, e.ID, sh.Day, hrFormatClock(sh.Start), hrFormatClock(sh.End), sh.Kind, v.actor.ID); err != nil {
			s.hrFail(w, r, err)
			return
		}
		label := sh.Day.Format("02.01") + " " + hrSlackHours(hrFormatClock(sh.Start), hrFormatClock(sh.End))
		if sh.Kind == "makeup" {
			label += " (відпрацювання)"
		}
		parts = append(parts, label)
	}
	detail := "тиждень " + hrSlackDates(weekStart, weekEnd) + ": "
	if len(parts) == 0 {
		detail += "без годин"
	} else {
		detail += strings.Join(parts, ", ")
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "🕘", Action: "Заплановані години", Person: e.DisplayName,
		Detail: detail, Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	rows, err := hrLoadDays(ctx, tx, nil, weekStart, weekEnd, &e.ID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp := hrShiftWeekResponse{WeekStart: hrFormatDate(weekStart), Cells: []hrCellJSON{}}
	for _, row := range rows {
		resp.Cells = append(resp.Cells, hrCellJSON{EmployeeID: row.EmployeeID, Date: hrFormatDate(row.Day), IsWorkday: row.IsWorkday,
			IsPatternDay: row.IsPatternDay, PlannedStart: row.PlannedStart, PlannedEnd: row.PlannedEnd, PlannedMinutes: row.PlannedMinutes,
			ShiftKind: row.ShiftKind, IsMakeup: row.IsMakeup, OfficeDays: row.OfficeDays})
	}
	pattern, err := hrPatternOn(ctx, tx, e.ID, weekStart)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if pattern != nil {
		resp.Summary = hrSummariseWeek(rows, pattern.DaysPerWeek, pattern.HoursPerDay)
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, resp)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}

// DELETE /v1/hr/shifts/{shiftId}: removes a planned / make-up day (entries list, day popover).
func (s *Server) handleHRDeleteShift(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("shiftId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Shift not found", nil)
		return
	}
	var employeeID uuid.UUID
	var day time.Time
	var kind, start, end string
	err = s.pool.QueryRow(r.Context(), `
		select employee_id, work_date, kind, to_char(start_time, 'HH24:MI'), to_char(end_time, 'HH24:MI')
		from public.hr_shifts where id = $1
	`, id).Scan(&employeeID, &day, &kind, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Shift not found", nil)
		return
	}
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	raw := employeeID.String()
	e, ok := s.hrTargetEmployee(w, r, v, &raw)
	if !ok {
		return
	}
	if code := hrEditWindowError(v.isAdmin(), hrScopeHours, hrToday(e.TimeZone), day); code != "" {
		s.hrRuleError(w, r, code, map[string]string{"date": code})
		return
	}
	const operation = "hr.shift.delete"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, id.String())
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	ctx := r.Context()
	if _, err := tx.Exec(ctx, `delete from public.hr_shifts where id = $1`, id); err != nil {
		s.hrFail(w, r, err)
		return
	}
	action := "Видалено заплановані години"
	detail := day.Format("02.01.2006") + " " + hrSlackHours(start, end)
	if kind == "makeup" {
		action = "Видалено день відпрацювання"
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "🗑️", Action: action, Person: e.DisplayName, Detail: detail,
		Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp := hrMutationResponse{}
	if resp.Balances, err = hrLoadBalances(ctx, tx, *e, hrToday(e.TimeZone).Year()); err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, resp)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}
