package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/dzebovski/kolss-platform-api/internal/botcheck"
	"github.com/dzebovski/kolss-platform-api/internal/httpapi"
	"github.com/dzebovski/kolss-platform-api/internal/submissions"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

type fakeService struct {
	pingErr error
	create  func(ctx context.Context, siteCode string, data validation.ValidatedLeadSubmission) (submissions.CreateResult, error)
	calls   int
}

func (f *fakeService) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeService) Create(ctx context.Context, siteCode string, data validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
	f.calls++
	if f.create != nil {
		return f.create(ctx, siteCode, data)
	}
	id := uuid.New()
	lead := uuid.New()
	return submissions.CreateResult{
		SubmissionID: id,
		Status:       "accepted",
		LeadID:       lead,
	}, nil
}

func newTestServer(svc *fakeService) http.Handler {
	return httpapi.NewServer(svc, httpapi.Options{
		Enabled:            true,
		AllowedOrigins:     []string{"http://localhost:4200", "http://localhost:4201"},
		BodyLimitBytes:     64 * 1024,
		RateLimitPerMinute: 1000,
		RequireBotToken:    false,
		BotVerifier:        botcheck.DisabledVerifier{},
	}).Handler()
}

func validBody() map[string]any {
	return map[string]any{
		"idempotency_key":        uuid.NewString(),
		"name":                   "Anna Kowalska",
		"phone":                  "+48123456789",
		"email":                  "anna@example.com",
		"city":                   "Warsaw",
		"project_description":    "Kitchen remodel",
		"privacy_accepted":       true,
		"privacy_policy_version": "pl-v1",
		"page_url":               "http://localhost:4200/",
		"bot_token":              "test-token",
		"website":                "",
	}
}

func TestCreate_SuccessNoFiles(t *testing.T) {
	leadID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	subID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	svc := &fakeService{create: func(ctx context.Context, siteCode string, data validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
		if siteCode != "kolss-pl" {
			t.Fatalf("site=%s", siteCode)
		}
		return submissions.CreateResult{
			SubmissionID: subID,
			Status:       "accepted",
			LeadID:       leadID,
		}, nil
	}}
	handler := newTestServer(svc)

	body, _ := json.Marshal(validBody())
	req := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-pl/lead-submissions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:4200")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "http://localhost:4200" {
		t.Fatalf("missing CORS")
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "accepted" || resp["lead_id"] != leadID.String() {
		t.Fatalf("resp=%v", resp)
	}
}

func TestCreate_Honeypot(t *testing.T) {
	svc := &fakeService{}
	handler := newTestServer(svc)
	bodyMap := validBody()
	bodyMap["website"] = "http://spam.test"
	body, _ := json.Marshal(bodyMap)
	req := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-pl/lead-submissions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d", rr.Code)
	}
	if svc.calls != 0 {
		t.Fatal("honeypot must not call service")
	}
}

func TestCreate_RejectsLegacyFilesField(t *testing.T) {
	handler := newTestServer(&fakeService{})
	bodyMap := validBody()
	bodyMap["files"] = []any{}
	body, _ := json.Marshal(bodyMap)
	req := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-pl/lead-submissions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCompleteRouteRemoved(t *testing.T) {
	handler := newTestServer(&fakeService{})
	req := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-pl/lead-submissions/"+uuid.NewString()+"/complete", bytes.NewReader([]byte(`{"files":[]}`)))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreate_MethodNotAllowed(t *testing.T) {
	handler := newTestServer(&fakeService{})
	req := httptest.NewRequest(http.MethodGet, "/v1/public/sites/kolss-pl/lead-submissions", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreate_OptionsPreservesCORS(t *testing.T) {
	handler := newTestServer(&fakeService{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/public/sites/kolss-pl/lead-submissions", nil)
	req.Header.Set("Origin", "http://localhost:4200")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Fatalf("CORS origin = %q", got)
	}
}

func TestCreate_PanicRecovery(t *testing.T) {
	svc := &fakeService{create: func(context.Context, string, validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
		panic("boom")
	}}
	handler := newTestServer(svc)
	body, _ := json.Marshal(validBody())
	request := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-pl/lead-submissions", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHealth(t *testing.T) {
	handler := newTestServer(&fakeService{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("live=%d", rr.Code)
	}
}
