package crmapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

type fakeDocumentStorage struct{ size int64 }

func (fakeDocumentStorage) PresignGet(context.Context, storage.PresignGetInput) (storage.PresignGetResult, error) {
	return storage.PresignGetResult{}, nil
}

func (fakeDocumentStorage) PresignPut(_ context.Context, in storage.PresignPutInput) (storage.PresignPutResult, error) {
	return storage.PresignPutResult{URL: "https://storage.test/" + in.Key, Method: "PUT", Headers: map[string]string{"Content-Type": in.ContentType}, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f fakeDocumentStorage) HeadObject(context.Context, storage.HeadObjectInput) (storage.HeadObjectResult, error) {
	return storage.HeadObjectResult{SizeBytes: f.size, ContentType: "application/pdf"}, nil
}

// TestLeadDocumentsDB runs the W11 upload → confirm → list flow against a disposable database
// (KOLSS_TEST_DATABASE_URL) with a fake object storage.
func TestLeadDocumentsDB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := &Server{pool: pool, storage: fakeDocumentStorage{size: 1200}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, userID, userID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Tester') on conflict (id) do nothing`, userID)
	actor := Actor{ID: userID, Role: "super_admin", IsActive: true, DisplayName: ptr("Tester")}
	leadID := uuid.New()
	mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone) values ($1,$2,$3,'Doc','+48600000001')`, leadID, officeID, leadID.String())

	call := func(handler http.HandlerFunc, method, body string, headers map[string]string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/v1/leads/x/documents", strings.NewReader(body))
		req.SetPathValue("leadId", leadID.String())
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, actor))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, upload := call(server.handleCreateDocumentUpload, http.MethodPost, `{"fileName":"plan.PDF","sizeBytes":1200}`, nil)
	if code != http.StatusOK || upload["attachmentId"] == nil {
		t.Fatalf("upload: %d %v", code, upload)
	}
	attachmentID := upload["attachmentId"].(string)
	if code, _ := call(server.handleListDocuments, http.MethodGet, ``, nil); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	code, doc := call(server.handleConfirmDocument, http.MethodPost, `{"attachmentId":"`+attachmentID+`","tag":"plan","note":"From the developer"}`, map[string]string{"Idempotency-Key": uuid.NewString()})
	if code != http.StatusCreated || doc["fileName"] != "plan.PDF" || doc["tag"] != "plan" || doc["uploadedByName"] == "" {
		t.Fatalf("confirm: %d %v", code, doc)
	}
	code, list := call(server.handleListDocuments, http.MethodGet, ``, nil)
	items, _ := list["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list after confirm: %d %v", code, list)
	}
	var events int
	if err := pool.QueryRow(ctx, `select count(*) from public.lead_events where lead_id=$1 and event_type='attachment' and new_value->>'note'='From the developer'`, leadID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("timeline event: %d %v", events, err)
	}
	if code, _ := call(server.handleCreateDocumentUpload, http.MethodPost, `{"fileName":"big.pdf","sizeBytes":30000000}`, nil); code != http.StatusBadRequest {
		t.Fatalf("over 25 MB must fail: %d", code)
	}
}
