package crmapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLeadReferenceIDIsSerializedInCalendarAndReportPayloads(t *testing.T) {
	appointmentPayload, err := json.Marshal(appointmentLeadSummary{
		ID:          uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		ReferenceID: "k0001",
		Name:        "Test",
		Phone:       "+380500000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	reportPayload, err := json.Marshal(reportLead{
		ID:          uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		ReferenceID: "w0042",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(appointmentPayload), `"referenceId":"k0001"`) {
		t.Fatalf("appointment payload missing lead reference: %s", appointmentPayload)
	}
	if !strings.Contains(string(reportPayload), `"referenceId":"w0042"`) {
		t.Fatalf("report payload missing lead reference: %s", reportPayload)
	}
}

