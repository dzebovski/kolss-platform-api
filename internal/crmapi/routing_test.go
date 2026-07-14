package crmapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtectedRouteRequiresCRMAuth(t *testing.T) {
	server := New(Options{})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"message":"Unauthorized"`) {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestImportRouteUsesImportSecretInsteadOfCRMAuth(t *testing.T) {
	server := New(Options{})
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/google-sheets/lead-imports", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"message":"Invalid import secret"`) {
		t.Fatalf("import route incorrectly used CRM auth: %s", response.Body.String())
	}
}

func TestCRMOptionsPreservesCORSWithoutAuth(t *testing.T) {
	server := New(Options{AllowedOrigins: []string{"https://crm.kolss.eu"}})
	request := httptest.NewRequest(http.MethodOptions, "/v1/leads", nil)
	request.Header.Set("Origin", "https://crm.kolss.eu")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://crm.kolss.eu" {
		t.Fatalf("CORS origin = %q", got)
	}
}
