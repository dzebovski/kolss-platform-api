package crmapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dzebovski/kolss-platform-api/internal/metaleads"
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

func TestRemovedGoogleSheetsImportRouteReturnsNotFound(t *testing.T) {
	server := New(Options{})
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/google-sheets/lead-imports", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMetaWebhookVerificationDoesNotUseCRMAuth(t *testing.T) {
	server := New(Options{MetaIntegration: &metaleads.Integration{Config: metaleads.Config{
		Enabled:            true,
		WebhookVerifyToken: "verify-secret",
	}}})
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/integrations/meta/webhook?hub.mode=subscribe&hub.verify_token=verify-secret&hub.challenge=42",
		nil,
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "42" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
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
	if methods := response.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, http.MethodPut) {
		t.Fatalf("CORS methods do not allow marker PUT: %q", methods)
	}
}

func TestUnknownCRMRouteIsNotClaimedByOptionsWildcard(t *testing.T) {
	server := New(Options{AllowedOrigins: []string{"https://crm.kolss.eu"}})

	for _, method := range []string{http.MethodPost, http.MethodOptions} {
		request := httptest.NewRequest(method, "/v1/does-not-exist", nil)
		request.Header.Set("Origin", "https://crm.kolss.eu")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
}

func TestEveryCRMRouteHasExplicitOptions(t *testing.T) {
	server := New(Options{})
	router := chi.NewRouter()
	server.RegisterRoutes(router)

	routes := make(map[string]struct{})
	options := make(map[string]struct{})
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodOptions {
			options[route] = struct{}{}
		} else {
			routes[route] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for route := range routes {
		if _, ok := options[route]; !ok {
			t.Errorf("route %s has no explicit OPTIONS handler", route)
		}
	}
}

func TestCRMRecoveryUsesJSONErrorSchema(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(Options{Logger: logger})
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(server.BaseMiddleware)
		r.Use(server.recoverPanic)
		r.Get("/v1/panic", func(http.ResponseWriter, *http.Request) {
			panic("crm route test panic")
		})
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	var payload errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("panic response is not JSON: %v body=%s", err, response.Body.String())
	}
	if payload.Code != "internal_error" || payload.RequestID == "" {
		t.Fatalf("unexpected panic response: %+v", payload)
	}
}
