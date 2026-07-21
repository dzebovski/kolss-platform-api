package metaleads

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

func TestWebhookVerification(t *testing.T) {
	integration := New(nil, Config{Enabled: true, WebhookVerifyToken: "verify-secret"}, notificationsOutbox(), nil, nil)
	tests := []struct {
		name  string
		query string
		want  int
		body  string
	}{
		{name: "valid", query: "hub.mode=subscribe&hub.verify_token=verify-secret&hub.challenge=12345", want: http.StatusOK, body: "12345"},
		{name: "wrong token", query: "hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=12345", want: http.StatusForbidden},
		{name: "missing challenge", query: "hub.mode=subscribe&hub.verify_token=verify-secret", want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/integrations/meta/webhook?"+test.query, nil)
			response := httptest.NewRecorder()
			integration.VerifyWebhook(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if test.body != "" && response.Body.String() != test.body {
				t.Fatalf("body=%q", response.Body.String())
			}
		})
	}
}

func TestWebhookRejectsMissingSignatureBeforeDatabase(t *testing.T) {
	integration := New(nil, Config{Enabled: true, AppSecret: "secret"}, notificationsOutbox(), nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/meta/webhook", nil)
	response := httptest.NewRecorder()
	integration.ReceiveWebhook(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestWebhookRejectsInvalidSignatureBeforeDatabase(t *testing.T) {
	integration := New(nil, Config{Enabled: true, AppSecret: "secret"}, notificationsOutbox(), nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/meta/webhook", bytes.NewBufferString(`{"object":"page"}`))
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	response := httptest.NewRecorder()
	integration.ReceiveWebhook(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestWebhookAcceptsValidSignatureBeforePersistenceFailure(t *testing.T) {
	const body = `{"object":"page","entry":[]}`
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(body))
	integration := New(nil, Config{Enabled: true, AppSecret: "secret"}, notificationsOutbox(), nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/meta/webhook", bytes.NewBufferString(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	integration.ReceiveWebhook(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestLeadgenEventsExtractMultipleEntriesAndChanges(t *testing.T) {
	payload := webhookPayload{Entry: []webhookEntry{
		{ID: "page-kyiv", Time: 100, Changes: []webhookChange{
			{Field: "leadgen", Value: webhookChangeValue{LeadgenID: "lead-1", FormID: "form-1", AdID: "ad-1"}},
			{Field: "feed", Value: webhookChangeValue{LeadgenID: "ignored"}},
		}},
		{ID: "entry-fallback", Time: 200, Changes: []webhookChange{
			{Field: "leadgen", Value: webhookChangeValue{LeadgenID: "lead-2", PageID: "page-warsaw", FormID: "form-2", CreatedAt: 300}},
		}},
	}}
	events := leadgenEvents(payload)
	if len(events) != 2 {
		t.Fatalf("events=%#v", events)
	}
	if events[0].PageID != "page-kyiv" || events[0].CreatedAt == nil || events[0].CreatedAt.Unix() != 100 {
		t.Fatalf("first event=%#v", events[0])
	}
	if events[1].PageID != "page-warsaw" || events[1].CreatedAt == nil || events[1].CreatedAt.Unix() != 300 {
		t.Fatalf("second event=%#v", events[1])
	}
}

func TestPageRoutingUsesConfiguredPageID(t *testing.T) {
	client := NewClient(Config{Pages: []Page{
		{OfficeCode: "kyiv", PageID: "page-kyiv"},
		{OfficeCode: "warsaw", PageID: "page-warsaw"},
	}})
	if page, ok := client.Page("page-kyiv"); !ok || page.OfficeCode != "kyiv" {
		t.Fatalf("Kyiv route=%#v ok=%v", page, ok)
	}
	if page, ok := client.Page("page-warsaw"); !ok || page.OfficeCode != "warsaw" {
		t.Fatalf("Warsaw route=%#v ok=%v", page, ok)
	}
	if _, ok := client.Page("unknown"); ok {
		t.Fatal("unknown Page was accepted")
	}
}

func TestRetrySchedule(t *testing.T) {
	wants := []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour}
	for index, want := range wants {
		if got := retryDelay(index + 1); got != want {
			t.Fatalf("attempt=%d delay=%s want=%s", index+1, got, want)
		}
	}
}

func TestRetryClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limit", err: &GraphError{HTTPStatus: http.StatusTooManyRequests, Code: 4}, want: true},
		{name: "server error", err: &GraphError{HTTPStatus: http.StatusBadGateway}, want: true},
		{name: "lead propagation delay", err: &GraphError{HTTPStatus: http.StatusBadRequest, Code: 100, Message: "Object does not exist yet"}, want: true},
		{name: "oauth", err: &GraphError{HTTPStatus: http.StatusUnauthorized, Code: 190}, want: true},
		{name: "permanent permission", err: &GraphError{HTTPStatus: http.StatusForbidden, Code: 200}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableError(test.err); got != test.want {
				t.Fatalf("retryable=%v want=%v error=%v", got, test.want, test.err)
			}
		})
	}
}

func TestNextNightlySyncUsesTwoUTC(t *testing.T) {
	now := time.Date(2026, 7, 21, 4, 1, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := nextNightlySync(now)
	want := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next sync=%s want=%s", got, want)
	}
}

func notificationsOutbox() notifications.Outbox { return notifications.Outbox{} }
