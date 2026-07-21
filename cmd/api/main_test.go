package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/dzebovski/kolss-platform-api/internal/botcheck"
	"github.com/dzebovski/kolss-platform-api/internal/crmapi"
	"github.com/dzebovski/kolss-platform-api/internal/httpapi"
	"github.com/dzebovski/kolss-platform-api/internal/submissions"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

func TestBuildRouterCombinesHealthPublicAndCRMRoutes(t *testing.T) {
	public := httpapi.NewServer(routeTestService{}, httpapi.Options{
		Enabled:            true,
		RateLimitPerMinute: 100,
		BotVerifier:        botcheck.DisabledVerifier{},
	})
	crm := crmapi.New(crmapi.Options{})
	handler := buildRouter(public, crm)

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/health/live", want: http.StatusOK},
		{method: http.MethodGet, path: "/v1/me", want: http.StatusUnauthorized},
		{method: http.MethodPost, path: "/v1/integrations/google-sheets/lead-imports", want: http.StatusNotFound},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s %s: status=%d want=%d body=%s", test.method, test.path, response.Code, test.want, response.Body.String())
		}
	}
}

func TestBuildRouterPreservesRoutingSemantics(t *testing.T) {
	public := httpapi.NewServer(routeTestService{}, httpapi.Options{
		Enabled:            true,
		AllowedOrigins:     []string{"https://crm.kolss.eu"},
		RateLimitPerMinute: 100,
		BotVerifier:        botcheck.DisabledVerifier{},
	})
	crm := crmapi.New(crmapi.Options{AllowedOrigins: []string{"https://crm.kolss.eu"}})
	handler := buildRouter(public, crm)

	tests := []struct {
		name   string
		method string
		path   string
		origin string
		want   int
	}{
		{name: "removed completion route", method: http.MethodPost, path: "/v1/public/sites/kolss-pl/lead-submissions/" + uuid.NewString() + "/complete", want: http.StatusNotFound},
		{name: "known route wrong method", method: http.MethodGet, path: "/v1/public/sites/kolss-pl/lead-submissions", want: http.StatusMethodNotAllowed},
		{name: "unknown v1 route", method: http.MethodPost, path: "/v1/does-not-exist", want: http.StatusNotFound},
		{name: "crm preflight bypasses auth", method: http.MethodOptions, path: "/v1/leads", origin: "https://crm.kolss.eu", want: http.StatusNoContent},
		{name: "unknown preflight", method: http.MethodOptions, path: "/v1/does-not-exist", origin: "https://crm.kolss.eu", want: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			if test.method == http.MethodOptions && test.want == http.StatusNoContent {
				if got := response.Header().Get("Access-Control-Allow-Origin"); got != test.origin {
					t.Fatalf("CORS origin=%q want=%q", got, test.origin)
				}
			}
		})
	}
}

func TestBuildRouterRecoversPublicHandlerPanic(t *testing.T) {
	public := httpapi.NewServer(panicRouteTestService{}, httpapi.Options{
		Enabled:            true,
		BodyLimitBytes:     64 * 1024,
		RateLimitPerMinute: 100,
		BotVerifier:        botcheck.DisabledVerifier{},
	})
	handler := buildRouter(public, crmapi.New(crmapi.Options{}))
	body, err := json.Marshal(map[string]any{
		"idempotency_key":        uuid.NewString(),
		"name":                   "Panic Test",
		"phone":                  "+380000000003",
		"privacy_accepted":       true,
		"privacy_policy_version": "ua-v1",
		"page_url":               "https://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/public/sites/kolss-ua/lead-submissions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Request-Id") == "" {
		t.Fatal("panic response is missing X-Request-Id")
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("panic response is not JSON: %v body=%s", err, response.Body.String())
	}
	errorBody, ok := payload["error"].(map[string]any)
	if !ok || errorBody["code"] != "internal_error" {
		t.Fatalf("unexpected panic response: %v", payload)
	}
}

type routeTestService struct{}

func (routeTestService) Ping(context.Context) error { return nil }

func (routeTestService) Create(context.Context, string, validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
	return submissions.CreateResult{}, nil
}

type panicRouteTestService struct{ routeTestService }

func (panicRouteTestService) Create(context.Context, string, validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
	panic("route test panic")
}
