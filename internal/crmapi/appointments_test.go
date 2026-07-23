package crmapi

import (
	"testing"
	"time"
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
