package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
		{method: http.MethodPost, path: "/v1/integrations/google-sheets/lead-imports", want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s %s: status=%d want=%d body=%s", test.method, test.path, response.Code, test.want, response.Body.String())
		}
	}
}

type routeTestService struct{}

func (routeTestService) Ping(context.Context) error { return nil }

func (routeTestService) Create(context.Context, string, validation.ValidatedLeadSubmission) (submissions.CreateResult, error) {
	return submissions.CreateResult{}, nil
}
