package crmapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseAppointmentLocalUsesOfficeTimezone(t *testing.T) {
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseAppointmentLocal("2026-07-23T09:30", location)
	if err != nil {
		t.Fatalf("parseAppointmentLocal returned error: %v", err)
	}
	want := time.Date(2026, time.July, 23, 7, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseAppointmentLocalRejectsDSTGap(t *testing.T) {
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseAppointmentLocal("2026-03-29T02:30", location); err == nil {
		t.Fatal("expected DST gap to be rejected")
	}
}

func TestAppointmentOutsideWorkingHours(t *testing.T) {
	location, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatal(err)
	}
	local := func(year int, month time.Month, day, hour, minute int) time.Time {
		return time.Date(year, month, day, hour, minute, 0, 0, location).UTC()
	}
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  bool
	}{
		{name: "inside", start: local(2026, time.July, 23, 9, 0), end: local(2026, time.July, 23, 10, 0)},
		{name: "ends at close", start: local(2026, time.July, 23, 18, 0), end: local(2026, time.July, 23, 19, 0)},
		{name: "before open", start: local(2026, time.July, 23, 8, 45), end: local(2026, time.July, 23, 9, 45), want: true},
		{name: "after close", start: local(2026, time.July, 23, 18, 30), end: local(2026, time.July, 23, 19, 30), want: true},
		{name: "sunday", start: local(2026, time.July, 26, 10, 0), end: local(2026, time.July, 26, 11, 0), want: true},
		{name: "crosses midnight", start: local(2026, time.July, 23, 18, 30), end: local(2026, time.July, 24, 9, 30), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := appointmentOutsideWorkingHours(test.start, test.end, location); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestAppointmentDurationValidation(t *testing.T) {
	for _, minutes := range []int{15, 30, 60, 90, 120, 480} {
		if !validAppointmentDuration(minutes) {
			t.Fatalf("%d should be valid", minutes)
		}
	}
	for _, minutes := range []int{0, 14, 16, 481} {
		if validAppointmentDuration(minutes) {
			t.Fatalf("%d should be invalid", minutes)
		}
	}
}

func TestAppointmentWarnings(t *testing.T) {
	got := appointmentWarnings(true, true)
	if len(got) != 2 || got[0] != "manager_overlap" || got[1] != "outside_working_hours" {
		t.Fatalf("unexpected warnings: %#v", got)
	}
}

func TestCompletedAppointmentDetailsRemainEditable(t *testing.T) {
	comment := "Updated visit note"
	visited := "visited"
	if !canUpdateAppointment("visited", appointmentMutationRequest{Comment: &comment}) {
		t.Fatal("visited appointment details should remain editable")
	}
	if canUpdateAppointment("visited", appointmentMutationRequest{Status: &visited}) {
		t.Fatal("visited appointment status must remain terminal")
	}
	if !canUpdateAppointment("scheduled", appointmentMutationRequest{Status: &visited}) {
		t.Fatal("scheduled appointment should allow a terminal status transition")
	}
	if canUpdateAppointment("rescheduled", appointmentMutationRequest{Comment: &comment}) {
		t.Fatal("historical rescheduled appointment must remain immutable")
	}
}

func TestAppointmentAuditQueriesCastPolymorphicJSONParameters(t *testing.T) {
	requiredCreateCasts := []string{
		"$5::uuid",
		"$6::timestamptz",
		"$7::timestamptz",
		"$8::uuid",
		"$9::text",
	}
	for _, cast := range requiredCreateCasts {
		if !strings.Contains(appointmentScheduledEventInsert, cast) {
			t.Fatalf("scheduled event query must contain %s", cast)
		}
	}

	requiredUpdateCasts := []string{
		"$6::uuid",
		"$7::timestamptz",
		"$8::timestamptz",
		"$9::uuid",
		"$10::text",
		"$11::text",
		"$12::timestamptz",
		"$13::timestamptz",
		"$14::uuid",
	}
	for _, cast := range requiredUpdateCasts {
		if !strings.Contains(appointmentChangedEventInsert, cast) {
			t.Fatalf("changed event query must contain %s", cast)
		}
	}
}

func TestCancelScheduledAppointmentsUpdateCancelsOnlyScheduled(t *testing.T) {
	for _, fragment := range []string{
		"update public.lead_showroom_visits",
		"status='canceled'",
		"where lead_id=$1",
		"and status='scheduled'",
		"and ($3::text is null or kind=$3)",
		"returning id, scheduled_at, ends_at, responsible_manager_id, comment",
	} {
		if !strings.Contains(cancelScheduledAppointmentsUpdate, fragment) {
			t.Fatalf("cancelScheduledAppointmentsUpdate missing %q", fragment)
		}
	}
}

func TestCloseAndArchiveCancelScheduledAppointments(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)

	activitiesSrc, err := os.ReadFile(filepath.Join(dir, "activities.go"))
	if err != nil {
		t.Fatal(err)
	}
	activities := string(activitiesSrc)
	if !strings.Contains(activities, `req.Status == "closed_lost"`) {
		t.Fatal("close path must detect closed_lost")
	}
	if !strings.Contains(activities, "cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, leadID)") {
		t.Fatal("close must cancel scheduled appointments")
	}

	leadsSrc, err := os.ReadFile(filepath.Join(dir, "leads.go"))
	if err != nil {
		t.Fatal(err)
	}
	leads := string(leadsSrc)
	archiveStart := strings.Index(leads, "func (s *Server) handleArchiveLead")
	if archiveStart < 0 {
		t.Fatal("handleArchiveLead not found")
	}
	archiveEnd := strings.Index(leads[archiveStart+1:], "\nfunc (s *Server)")
	if archiveEnd < 0 {
		t.Fatal("handleArchiveLead end not found")
	}
	archive := leads[archiveStart : archiveStart+1+archiveEnd]
	if !strings.Contains(archive, "cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, leadID)") {
		t.Fatal("archive must cancel scheduled appointments")
	}
	if strings.Contains(archive, "active_appointment_exists") {
		t.Fatal("archive must not block on active appointments")
	}
	if strings.Contains(archive, "Cancel the scheduled appointment before archiving this lead") {
		t.Fatal("archive must not ask callers to cancel appointments first")
	}
}

func TestAppointmentKindValidation(t *testing.T) {
	showroom := appointmentKindShowroom
	measurement := appointmentKindMeasurement
	unknown := "delivery"
	manager := uuid.New()
	starts := "2026-08-11T10:00"
	duration := 120

	base := func(kind *string) appointmentMutationRequest {
		return appointmentMutationRequest{
			LeadID:               uuid.New(),
			Kind:                 kind,
			StartsAtLocal:        &starts,
			DurationMinutes:      &duration,
			ResponsibleManagerID: &manager,
		}
	}

	for name, kind := range map[string]*string{
		"omitted":     nil,
		"showroom":    &showroom,
		"measurement": &measurement,
	} {
		if fields := validateCreateAppointment(base(kind)); len(fields) != 0 {
			t.Fatalf("%s kind should be accepted, got %#v", name, fields)
		}
	}
	if fields := validateCreateAppointment(base(&unknown)); fields["kind"] == "" {
		t.Fatal("unknown kind must be rejected")
	}

	// Kind is immutable: rebooking as another kind means a new appointment.
	if fields := validateUpdateAppointment(appointmentMutationRequest{Kind: &measurement}); fields["kind"] == "" {
		t.Fatal("update must reject a kind change")
	}
}

func TestClientStatusForAppointmentKind(t *testing.T) {
	if got := clientStatusForAppointmentKind(appointmentKindMeasurement); got != "measurement_scheduled" {
		t.Fatalf("got %q, want measurement_scheduled", got)
	}
	for _, kind := range []string{appointmentKindShowroom, ""} {
		if got := clientStatusForAppointmentKind(kind); got != "showroom_invited" {
			t.Fatalf("kind %q: got %q, want showroom_invited", kind, got)
		}
	}
}

// The active-appointment probe is scoped by kind so a lead can hold a showroom
// meeting and a measurement at the same time, while the manager-overlap probe is
// deliberately not, so a measurement blocks a showroom slot and vice versa.
func TestAppointmentScopingIsPerKindExceptManagerAvailability(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "appointments.go"))
	if err != nil {
		t.Fatal(err)
	}
	appointments := string(source)

	if !strings.Contains(appointments, "where lead_id=$1 and kind=$2 and status='scheduled'") {
		t.Fatal("the active-appointment probe must be scoped by kind")
	}
	if !strings.Contains(appointments, "where lead_id=$1 and kind='showroom' and status='scheduled'") {
		t.Fatal("the legacy showroom_invited path must stay on the showroom kind")
	}

	overlapStart := strings.Index(appointments, "func managerOverlapExists")
	if overlapStart < 0 {
		t.Fatal("managerOverlapExists not found")
	}
	overlapEnd := strings.Index(appointments[overlapStart+1:], "\nfunc ")
	if overlapEnd < 0 {
		t.Fatal("managerOverlapExists end not found")
	}
	overlap := appointments[overlapStart : overlapStart+1+overlapEnd]
	if strings.Contains(overlap, "kind") {
		t.Fatal("manager availability must span every kind")
	}
	for _, fragment := range []string{
		"other.status='scheduled'",
		"other.responsible_manager_id=$2",
		"other.scheduled_at < $4",
		"other.ends_at > $3",
	} {
		if !strings.Contains(overlap, fragment) {
			t.Fatalf("manager overlap query missing %q", fragment)
		}
	}
}

// A double booking is rejected outright, so both mutation paths must take the
// per-manager lock before probing, and the error must map to 409 manager_busy.
func TestManagerDoubleBookingIsBlocked(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "appointments.go"))
	if err != nil {
		t.Fatal(err)
	}
	appointments := string(source)

	if !strings.Contains(appointments, "pg_advisory_xact_lock(hashtextextended($1::text, 0))") {
		t.Fatal("the availability check must be serialized per manager")
	}
	if strings.Count(appointments, "ensureManagerFree(") != 3 {
		t.Fatal("create and update must both check manager availability")
	}
	if !strings.Contains(appointments, `"manager_busy"`) ||
		!strings.Contains(appointments, "http.StatusConflict, \"manager_busy\"") {
		t.Fatal("a busy manager must surface as 409 manager_busy")
	}
}
