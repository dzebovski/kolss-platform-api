package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDispatcherWakeIsNonBlockingAndCoalesced(t *testing.T) {
	dispatcher := NewDispatcher(nil, nil, nil, 20, time.Hour)
	dispatcher.Wake()
	dispatcher.Wake()
	if got := len(dispatcher.wake); got != 1 {
		t.Fatalf("queued wakes = %d, want 1", got)
	}
}

func TestDispatcherSchedulesRateLimitRecoveryWake(t *testing.T) {
	dispatcher := NewDispatcher(nil, nil, nil, 20, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.scheduleRetry(ctx, 10*time.Millisecond)
	select {
	case <-dispatcher.retry:
	case <-time.After(time.Second):
		t.Fatal("retry wake was not scheduled")
	}
}

func TestDispatcherDefaults(t *testing.T) {
	dispatcher := NewDispatcher(nil, nil, nil, 0, 0)
	if dispatcher.batchSize() != 20 || dispatcher.SweepInterval != time.Hour {
		t.Fatalf("defaults = batch %d interval %s", dispatcher.batchSize(), dispatcher.SweepInterval)
	}
	if maxDeliveryAttempts != 10 {
		t.Fatalf("max attempts = %d", maxDeliveryAttempts)
	}
}

func TestSendTelegramUsesOfficeTokenAndHTML(t *testing.T) {
	var request *http.Request
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		request = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}, nil
	})}
	dispatcher := NewDispatcher(nil, testCredentials{}, nil, 20, time.Hour)
	if err := dispatcher.sendTelegram(context.Background(), client, "kyiv", "-1001", "hello"); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.URL.String() != "https://api.telegram.org/botkyiv-token/sendMessage" {
		t.Fatalf("request URL = %v", request)
	}
}

func TestSendSlackUsesBotTokenAndChannel(t *testing.T) {
	var request *http.Request
	var body map[string]any
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		request = req
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}, nil
	})}
	dispatcher := NewDispatcher(nil, testCredentials{}, nil, 20, time.Hour)
	if err := dispatcher.sendSlack(context.Background(), client, "warsaw", "C123WARSAW", "cześć"); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.URL.String() != "https://slack.com/api/chat.postMessage" {
		t.Fatalf("request URL = %v", request)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer warsaw-slack-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if body["channel"] != "C123WARSAW" || body["text"] != "cześć" || body["unfurl_links"] != false || body["unfurl_media"] != false {
		t.Fatalf("body = %#v", body)
	}
}

func TestSendSlackRejectsHTTPAndAPIErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "http", status: http.StatusBadGateway, body: `{"ok":false}`, want: "status 502"},
		{name: "api", status: http.StatusOK, body: `{"ok":false,"error":"channel_not_found"}`, want: "channel_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})}
			err := SendSlackMessage(context.Background(), client, "xoxb-secret", "C123", "hello")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "xoxb-secret") {
				t.Fatalf("error exposed token: %v", err)
			}
		})
	}
}

func TestSendSlackHonorsRetryAfter(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Retry-After", "7")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"ok":false}`)), Header: header}, nil
	})}
	err := SendSlackMessage(context.Background(), client, "xoxb-secret", "C123", "hello")
	var rateLimit *rateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.RetryAfter != 7*time.Second {
		t.Fatalf("error = %#v", err)
	}
}

func TestBuildTelegramNotificationMessage(t *testing.T) {
	msg := BuildTelegramNotificationMessage(map[string]any{
		"name":                     "Іван <Менеджер>",
		"phone":                    "+380501112233",
		"source_system":            "meta_lead_ads",
		"office_code":              "kyiv",
		"created_at":               "2026-07-13T12:15:00Z",
		"product_interest":         "Кухня & шафа",
		"project_stage":            "Потрібен <проєкт>",
		"communication_preference": "Telegram",
		"crm_url":                  "https://crm.example/crm/leads/1?a=1&b=2",
	})
	want := "🔔 Нова заявка! 13.07.2026, 15:15\n👤 Ім'я: Іван &lt;Менеджер&gt;\n🏠 Що цікавить?: Кухня &amp; шафа\n🪜 Етап проекту?: Потрібен &lt;проєкт&gt;\n💬 Як спілкуватися?: Telegram\n📞 Тел: +380501112233\n🌐 Джерело: Facebook Forms\n🔗 <a href=\"https://crm.example/crm/leads/1?a=1&amp;b=2\">Відкрити в CRM</a>"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

func TestBuildTelegramNotificationMessageFallsBackToClientInfo(t *testing.T) {
	msg := BuildTelegramNotificationMessage(map[string]any{
		"name":          "Іван",
		"phone":         "+380501112233",
		"source_system": "site_form",
		"office_code":   "kyiv",
		"created_at":    "2026-01-01T10:00:00Z",
		"client_info":   "Короткий опис",
	})
	want := "🔔 Нова заявка! 01.01.2026, 12:00\n👤 Ім'я: Іван\nℹ️ Інформація: Короткий опис\n📞 Тел: +380501112233\n🌐 Джерело: Site Form"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

func TestBuildTelegramNotificationMessageManualSource(t *testing.T) {
	msg := BuildTelegramNotificationMessage(map[string]any{
		"name":             "Марія",
		"phone":            "+380671112233",
		"source_system":    "manual",
		"office_code":      "kyiv",
		"created_at":       "2026-07-14T10:00:00Z",
		"product_interest": "Кухня",
		"crm_url":          "https://crm.example/crm/leads/abc",
	})
	want := "🔔 Нова заявка! 14.07.2026, 13:00\n👤 Ім'я: Марія\n🏠 Що цікавить?: Кухня\n📞 Тел: +380671112233\n🌐 Джерело: Вручну\n🔗 <a href=\"https://crm.example/crm/leads/abc\">Відкрити в CRM</a>"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

func TestBuildSlackNotificationMessage(t *testing.T) {
	msg := BuildSlackNotificationMessage(map[string]any{
		"name":                     "Jan <Admin>",
		"phone":                    "+48123123123",
		"email":                    "jan@example.pl",
		"source_system":            "manual",
		"office_code":              "warsaw",
		"created_at":               "2026-07-13T12:15:00Z",
		"product_interest":         "Kuchnia & szafa",
		"project_stage":            "Projekt <start>",
		"communication_preference": "Telefon",
		"crm_url":                  "https://crm.example/crm/leads/1?a=1&b=2",
	})
	want := "🔔 Nowe zgłoszenie! 13.07.2026, 14:15\n👤 Imię: Jan &lt;Admin&gt;\n🏠 Czego dotyczy?: Kuchnia &amp; szafa\n🪜 Etap projektu?: Projekt &lt;start&gt;\n💬 Preferowany kontakt: Telefon\n📞 Telefon: +48123123123\n✉️ E-mail: jan@example.pl\n🌐 Źródło: Ręcznie\n🔗 <https://crm.example/crm/leads/1?a=1&amp;b=2|Otwórz w CRM>"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

func TestDispatcherSendsSlackNotification(t *testing.T) {
	row := newFakeSlackNotification("pending", 0, true)
	database := &fakeNotificationDatabase{rows: []*fakeNotification{row}}
	sender := &countingTransport{status: http.StatusOK}
	dispatcher := NewDispatcher(database, testCredentials{}, nil, 20, time.Hour)
	dispatcher.HTTP = &http.Client{Transport: sender}

	sent, failed, err := dispatcher.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 || failed != 0 || row.status != "sent" || sender.requests != 1 {
		t.Fatalf("sent=%d failed=%d row=%#v requests=%d", sent, failed, row, sender.requests)
	}
}

func TestDispatcherStopsBatchAndSchedulesSlackRateLimitRetry(t *testing.T) {
	first := newFakeSlackNotification("pending", 0, true)
	second := newFakeSlackNotification("pending", 0, true)
	database := &fakeNotificationDatabase{rows: []*fakeNotification{first, second}}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		header := make(http.Header)
		header.Set("Retry-After", "7")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"ok":false}`)), Header: header}, nil
	})}
	dispatcher := NewDispatcher(database, testCredentials{}, nil, 20, time.Hour)
	dispatcher.HTTP = client
	sent, failed, err := dispatcher.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || failed != 1 || requests != 1 || first.status != "failed" || second.status != "pending" {
		t.Fatalf("sent=%d failed=%d requests=%d first=%#v second=%#v", sent, failed, requests, first, second)
	}
	if first.nextDelay != 7*time.Second {
		t.Fatalf("next delay = %s", first.nextDelay)
	}
}

func TestWakeDrainsQueueLargerThanBatch(t *testing.T) {
	database := &fakeNotificationDatabase{}
	for i := 0; i < 45; i++ {
		database.rows = append(database.rows, newFakeNotification("pending", 0, true))
	}
	sender := &countingTransport{status: http.StatusOK}
	dispatcher := NewDispatcher(database, testCredentials{}, nil, 20, time.Hour)
	dispatcher.HTTP = &http.Client{Transport: sender}

	dispatcher.sweep(context.Background(), "wake")

	if sender.requests != 45 {
		t.Fatalf("Telegram requests = %d, want 45", sender.requests)
	}
	for _, row := range database.rows {
		if row.status != "sent" || row.attempts != 1 {
			t.Fatalf("row not sent: %#v", row)
		}
	}
}

func TestFailedRowRetriesOnlyOnDueRecoverySweep(t *testing.T) {
	database := &fakeNotificationDatabase{rows: []*fakeNotification{newFakeNotification("pending", 0, true)}}
	sender := &countingTransport{status: http.StatusBadGateway}
	dispatcher := NewDispatcher(database, testCredentials{}, nil, 20, time.Hour)
	dispatcher.HTTP = &http.Client{Transport: sender}

	dispatcher.sweep(context.Background(), "wake")
	row := database.rows[0]
	if row.status != "failed" || row.attempts != 1 || sender.requests != 1 {
		t.Fatalf("after failure: row=%#v requests=%d", row, sender.requests)
	}
	dispatcher.sweep(context.Background(), "wake")
	if sender.requests != 1 {
		t.Fatalf("wake retried failed row; requests=%d", sender.requests)
	}

	row.due = true
	dispatcher.sweep(context.Background(), "hourly")
	if row.attempts != 2 || sender.requests != 2 {
		t.Fatalf("hourly retry: row=%#v requests=%d", row, sender.requests)
	}
}

func TestRecoverySweepProcessesMissedWakeAndStopsAfterTenAttempts(t *testing.T) {
	pending := newFakeNotification("pending", 0, true)
	exhausted := newFakeNotification("failed", maxDeliveryAttempts, true)
	database := &fakeNotificationDatabase{rows: []*fakeNotification{pending, exhausted}}
	sender := &countingTransport{status: http.StatusOK}
	dispatcher := NewDispatcher(database, testCredentials{}, nil, 20, time.Hour)
	dispatcher.HTTP = &http.Client{Transport: sender}

	dispatcher.sweep(context.Background(), "startup")

	if pending.status != "sent" || sender.requests != 1 {
		t.Fatalf("missed wake was not recovered: pending=%#v requests=%d", pending, sender.requests)
	}
	if exhausted.status != "failed" || exhausted.attempts != maxDeliveryAttempts {
		t.Fatalf("exhausted row changed: %#v", exhausted)
	}
}

type testCredentials struct{}

func (testCredentials) TelegramBotTokenFor(officeCode string) string {
	return officeCode + "-token"
}

func (testCredentials) SlackBotTokenFor(officeCode string) string {
	return officeCode + "-slack-token"
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type countingTransport struct {
	status   int
	requests int
}

func (transport *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.requests++
	return &http.Response{
		StatusCode: transport.status,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     make(http.Header),
	}, nil
}

type fakeNotification struct {
	id          string
	channel     string
	destination string
	payload     []byte
	status      string
	attempts    int
	due         bool
	claimed     bool
	claimToken  string
	nextDelay   time.Duration
}

func newFakeNotification(status string, attempts int, due bool) *fakeNotification {
	return &fakeNotification{
		id:          uuid.NewString(),
		channel:     "telegram",
		destination: "-1001",
		payload:     []byte(`{"office_code":"kyiv","name":"Anna"}`),
		status:      status,
		attempts:    attempts,
		due:         due,
	}
}

func newFakeSlackNotification(status string, attempts int, due bool) *fakeNotification {
	row := newFakeNotification(status, attempts, due)
	row.channel = "slack"
	row.destination = "C123WARSAW"
	row.payload = []byte(`{"office_code":"warsaw","name":"Anna"}`)
	return row
}

type fakeNotificationDatabase struct {
	rows []*fakeNotification
}

func (database *fakeNotificationDatabase) Begin(context.Context) (pgx.Tx, error) {
	return &fakeNotificationTx{database: database}, nil
}

func (database *fakeNotificationDatabase) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	row := database.find(args[0].(string))
	if row == nil {
		return pgconn.CommandTag{}, errors.New("notification not found")
	}
	switch {
	case strings.Contains(sql, "status='sent'"):
		row.status = "sent"
		row.attempts = args[1].(int)
		row.claimed = false
		row.claimToken = ""
	case strings.Contains(sql, "status='failed'"):
		row.status = "failed"
		row.attempts = args[1].(int)
		row.due = false
		row.claimed = false
		row.claimToken = ""
		row.nextDelay = time.Duration(args[4].(int64)) * time.Millisecond
	default:
		return pgconn.CommandTag{}, errors.New("unexpected notification update")
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (database *fakeNotificationDatabase) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "select count(*)") {
		return notificationScanRow(func(...any) error { return errors.New("unexpected query") })
	}
	count := 0
	for _, row := range database.rows {
		if (row.status == "pending" || row.status == "failed") && row.attempts < args[0].(int) {
			count++
		}
	}
	return notificationScanRow(func(dest ...any) error {
		*dest[0].(*int) = count
		return nil
	})
}

func (database *fakeNotificationDatabase) find(id string) *fakeNotification {
	for _, row := range database.rows {
		if row.id == id {
			return row
		}
	}
	return nil
}

type fakeNotificationTx struct {
	pgx.Tx
	database  *fakeNotificationDatabase
	selected  *fakeNotification
	committed bool
}

func (tx *fakeNotificationTx) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	maxAttempts := args[0].(int)
	includeFailed := args[1].(bool)
	for _, row := range tx.database.rows {
		eligibleStatus := row.status == "pending" || (includeFailed && row.status == "failed")
		if eligibleStatus && row.attempts < maxAttempts && row.due && !row.claimed {
			tx.selected = row
			return notificationScanRow(func(dest ...any) error {
				*dest[0].(*string) = row.id
				*dest[1].(*string) = row.channel
				*dest[2].(*string) = row.destination
				*dest[3].(*[]byte) = row.payload
				*dest[4].(*int) = row.attempts
				return nil
			})
		}
	}
	return notificationScanRow(func(...any) error { return pgx.ErrNoRows })
}

func (tx *fakeNotificationTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	if tx.selected == nil || tx.selected.id != args[0].(string) {
		return pgconn.CommandTag{}, errors.New("unexpected claim")
	}
	tx.selected.claimed = true
	tx.selected.claimToken = args[1].(string)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (tx *fakeNotificationTx) Commit(context.Context) error {
	tx.committed = true
	return nil
}

func (tx *fakeNotificationTx) Rollback(context.Context) error {
	if tx.committed {
		return pgx.ErrTxClosed
	}
	return nil
}

type notificationScanRow func(dest ...any) error

func (row notificationScanRow) Scan(dest ...any) error { return row(dest...) }
