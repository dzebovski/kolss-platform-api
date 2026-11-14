package crmapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Meetings and tasks of a project (P12, owner decision 2026-11-14).
//
//	POST  /v1/appointments                       with `projectId`: book a project meeting
//	PATCH /v1/appointments/{id}                  reschedule / held / no-show / cancel (as any meeting)
//	POST  /v1/tasks                              with link {type: project}: a task of the project
//	GET   /v1/projects/{projectId}/activities    the project's tasks and meetings
//
// A project meeting is a row of lead_showroom_visits with project_id set. It keeps the project's
// lead (calendar, manager overlap and the Tasks page see it like any visit) but never changes the
// lead's statuses: the project timeline gets meeting_* events instead of lead events.

const projectActivitiesLimit = 200

// lockOpenAppointmentProject locks the project of a meeting and refuses closed or finished ones.
func lockOpenAppointmentProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, `select status from public.projects where id=$1 for update`, projectID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAppointmentProjectMissing
	}
	if err != nil {
		return err
	}
	if isFinalProjectStatus(status) {
		return errAppointmentProjectClosed
	}
	return nil
}

func isFinalProjectStatus(status string) bool {
	return status == projectStatusCancelled || status == projectStatusCompleted
}

func (s *Server) createProjectAppointment(
	r *http.Request,
	tx pgx.Tx,
	actor Actor,
	req appointmentMutationRequest,
) (appointment, error) {
	projectID := *req.ProjectID
	var leadID, officeID uuid.UUID
	var status string
	err := tx.QueryRow(r.Context(), `
		select p.lead_id, p.office_id, p.status
		from public.projects p
		join public.leads l on l.id = p.lead_id
		where p.id = $1 and l.archived_at is null
		for update of p
	`, projectID).Scan(&leadID, &officeID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return appointment{}, errAppointmentProjectMissing
	}
	if err != nil {
		return appointment{}, err
	}
	if !actor.CanAccessOffice(officeID) {
		return appointment{}, errAppointmentOfficeDenied
	}
	if isFinalProjectStatus(status) {
		return appointment{}, errAppointmentProjectClosed
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
	kind := appointmentKindShowroom
	if req.Kind != nil {
		kind = *req.Kind
	}
	if err := ensureManagerFree(r.Context(), tx, uuid.Nil, *req.ResponsibleManagerID, startsAt, endsAt); err != nil {
		return appointment{}, err
	}
	var appointmentID uuid.UUID
	if err := tx.QueryRow(r.Context(), `
		insert into public.lead_showroom_visits (
		  lead_id, project_id, kind, scheduled_at, ends_at, status, comment,
		  responsible_manager_id, created_by, updated_by, purpose
		)
		values ($1,$2,$3,$4,$5,'scheduled',$6,$7,$8,$8,$9)
		returning id
	`, leadID, projectID, kind, startsAt, endsAt, cleanOptional(req.Comment), *req.ResponsibleManagerID,
		actor.ID, cleanOptional(req.Purpose)).Scan(&appointmentID); err != nil {
		return appointment{}, err
	}
	manager := req.ResponsibleManagerID
	if err := writeProjectEvent(r.Context(), tx, projectID, actor.ID, "meeting_scheduled", nil,
		projectMeetingEventValue(appointmentID, kind, startsAt, endsAt, manager, appointmentStatusScheduled),
		cleanOptional(req.Comment),
	); err != nil {
		return appointment{}, err
	}
	return loadAppointmentTx(r, tx, appointmentID)
}

// projectMeetingEventType maps the lead event type of a meeting change onto the project's.
func projectMeetingEventType(leadEventType string) string {
	switch leadEventType {
	case "appointment_status_changed":
		return "meeting_status_changed"
	case "appointment_rescheduled":
		return "meeting_rescheduled"
	default:
		return "meeting_updated"
	}
}

func projectMeetingEventValue(
	appointmentID uuid.UUID,
	kind string,
	startsAt, endsAt time.Time,
	managerID *uuid.UUID,
	status string,
) map[string]any {
	return map[string]any{
		"appointmentId":        appointmentID,
		"kind":                 kind,
		"startsAt":             startsAt.UTC().Format(time.RFC3339),
		"endsAt":               endsAt.UTC().Format(time.RFC3339),
		"responsibleManagerId": managerID,
		"status":               status,
	}
}

// handleListProjectActivities returns the project's manual tasks (open first by due date, then
// done, latest first) and its meetings (latest first). Anyone with access to the office reads them.
func (s *Server) handleListProjectActivities(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	h, ok := s.loadReadableProject(w, r, actor)
	if !ok {
		return
	}
	fail := func() {
		s.writeError(w, r, http.StatusInternalServerError, "project_activities_load_failed", "Could not load the project's tasks and meetings", nil)
	}

	tasks := make([]taskFeedItem, 0)
	rows, err := s.pool.Query(r.Context(), taskFeedSelectSQL(
		`source = 'manual' and link_project_id = $1`,
		`(status = 'done'), case when status = 'open' then due_at end nulls last, updated_at desc, id`,
		projectActivitiesLimit,
	), h.ID)
	if err != nil {
		fail()
		return
	}
	for rows.Next() {
		item, scanErr := scanTaskFeedItem(rows, actor)
		if scanErr != nil {
			rows.Close()
			fail()
			return
		}
		tasks = append(tasks, item)
	}
	rows.Close()
	if rows.Err() != nil {
		fail()
		return
	}

	meetings := make([]appointment, 0)
	meetingRows, err := s.pool.Query(r.Context(), appointmentSelect+`
		where v.project_id = $1
		order by v.scheduled_at desc, v.id
		limit `+strconv.Itoa(projectActivitiesLimit), h.ID)
	if err != nil {
		fail()
		return
	}
	for meetingRows.Next() {
		item, scanErr := scanAppointment(meetingRows)
		if scanErr != nil {
			meetingRows.Close()
			fail()
			return
		}
		meetings = append(meetings, item)
	}
	meetingRows.Close()
	if meetingRows.Err() != nil {
		fail()
		return
	}
	for i := range meetings {
		item := &meetings[i]
		location, locErr := time.LoadLocation(item.Office.TimezoneName)
		if locErr != nil {
			fail()
			return
		}
		item.IsOutsideWorkingHours = appointmentOutsideWorkingHours(item.StartsAt, item.EndsAt, location)
		if item.Status == appointmentStatusScheduled {
			item.HasConflict, err = s.appointmentHasConflict(r, item.ID, item.ResponsibleManager, item.StartsAt, item.EndsAt)
			if err != nil {
				fail()
				return
			}
		}
		item.Warnings = appointmentWarnings(item.HasConflict, item.IsOutsideWorkingHours)
	}

	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "meetings": meetings})
}
