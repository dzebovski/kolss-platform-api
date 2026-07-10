package botcheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDisabledVerifier(t *testing.T) {
	var v DisabledVerifier
	if err := v.Verify(context.Background(), "", ""); err != nil {
		t.Fatalf("disabled verifier should pass: %v", err)
	}
}

func TestTurnstileVerifier_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("secret") == "" || r.Form.Get("response") == "" {
			t.Fatalf("missing form fields")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "lead_submission",
			"hostname": "localhost",
		})
	}))
	defer srv.Close()

	v := NewTurnstileVerifier(TurnstileConfig{
		SecretKey:        "test-secret",
		AllowedHostnames: HostnamesSet([]string{"localhost"}),
		HTTPClient:       srv.Client(),
		SiteverifyURL:    srv.URL,
	})
	if err := v.Verify(context.Background(), "token", "127.0.0.1"); err != nil {
		t.Fatalf("expected success: %v", err)
	}
}

func TestTurnstileVerifier_RejectsBadAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "other",
			"hostname": "localhost",
		})
	}))
	defer srv.Close()

	v := NewTurnstileVerifier(TurnstileConfig{
		SecretKey:        "secret",
		AllowedHostnames: HostnamesSet([]string{"localhost"}),
		HTTPClient:       srv.Client(),
		SiteverifyURL:    srv.URL,
	})
	if err := v.Verify(context.Background(), "token", "127.0.0.1"); err == nil {
		t.Fatal("expected verification failure for bad action")
	}
}

func TestTurnstileVerifier_RejectsHostname(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "lead_submission",
			"hostname": "evil.example",
		})
	}))
	defer srv.Close()

	v := NewTurnstileVerifier(TurnstileConfig{
		SecretKey:        "secret",
		AllowedHostnames: HostnamesSet([]string{"localhost"}),
		HTTPClient:       srv.Client(),
		SiteverifyURL:    srv.URL,
	})
	if err := v.Verify(context.Background(), "token", ""); err == nil {
		t.Fatal("expected hostname rejection")
	}
}
