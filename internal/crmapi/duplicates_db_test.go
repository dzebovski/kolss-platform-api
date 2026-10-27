package crmapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMarkLeadDuplicateDB runs "Duplicate of…" against a disposable database
// (KOLSS_TEST_DATABASE_URL): close + link + events, the guards, report exclusion and reopen.
func TestMarkLeadDuplicateDB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID, otherOfficeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select id from public.offices order by code desc limit 1`).Scan(&otherOfficeID); err != nil || otherOfficeID == officeID {
		t.Fatalf("two offices are required: %v", err)
	}
	managerID := uuid.New()
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, managerID, managerID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Duplicate Manager') on conflict (id) do nothing`, managerID)
	mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1, $2)`, managerID, officeID)
	manager := Actor{ID: managerID, Role: "manager", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}

	phone := fmt.Sprintf("+48%09d", 100000000+rand.IntN(899999999))
	var leadIDs, clientIDs []uuid.UUID
	insertLead := func(office uuid.UUID, message string) (uuid.UUID, string) {
		id := uuid.New()
		leadIDs = append(leadIDs, id)
		var code string
		var clientID uuid.UUID
		if err := pool.QueryRow(ctx, `
			insert into public.leads (id, office_id, source_system, external_lead_id, name, phone, order_comment, assigned_to)
			values ($1,$2,'manual',$3,'Teresa Duplicate',$4,$5,$6) returning reference_id, client_id
		`, id, office, id.String(), phone, message, managerID).Scan(&code, &clientID); err != nil {
			t.Fatalf("insert lead: %v", err)
		}
		clientIDs = append(clientIDs, clientID)
		return id, code
	}
	t.Cleanup(func() {
		mustExec(t, pool, `delete from public.leads where id = any($1::uuid[])`, leadIDs)
		mustExec(t, pool, `delete from public.clients where id = any($1::uuid[])`, clientIDs)
		mustExec(t, pool, `delete from public.user_office_memberships where user_id = $1`, managerID)
		mustExec(t, pool, `delete from public.profiles where id = $1`, managerID)
		mustExec(t, pool, `delete from auth.users where id = $1`, managerID)
	})

	mark := func(leadID, originalID uuid.UUID) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/leads/"+leadID.String()+"/duplicate",
			strings.NewReader(`{"originalLeadId":"`+originalID.String()+`"}`))
		req.SetPathValue("leadId", leadID.String())
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, manager))
		rec := httptest.NewRecorder()
		server.handleMarkLeadDuplicate(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	original, originalCode := insertLead(officeID, "first request")
	duplicate, duplicateCode := insertLead(officeID, "second request")

	if code, _ := mark(duplicate, duplicate); code != http.StatusBadRequest {
		t.Fatalf("self duplicate = %d, want 400", code)
	}
	foreign, _ := insertLead(otherOfficeID, "other office")
	if code, _ := mark(duplicate, foreign); code != http.StatusBadRequest {
		t.Fatalf("original in another office = %d, want 400", code)
	}

	if code, body := mark(duplicate, original); code != http.StatusOK {
		t.Fatalf("mark duplicate = %d %v", code, body)
	}
	var clientStatus, lossReason string
	var duplicateOf *uuid.UUID
	if err := pool.QueryRow(ctx, `select client_status, loss_reason, duplicate_of_lead_id from public.leads where id=$1`, duplicate).
		Scan(&clientStatus, &lossReason, &duplicateOf); err != nil {
		t.Fatal(err)
	}
	if clientStatus != "closed_lost" || lossReason != lossReasonDuplicate || duplicateOf == nil || *duplicateOf != original {
		t.Fatalf("duplicate = %s/%s/%v, want closed_lost/duplicate/%s", clientStatus, lossReason, duplicateOf, original)
	}
	var marked, repeat map[string]any
	if err := pool.QueryRow(ctx, `select new_value from public.lead_events where lead_id=$1 and event_type='marked_duplicate'`, duplicate).Scan(&marked); err != nil {
		t.Fatalf("marked_duplicate event: %v", err)
	}
	if marked["original_reference_id"] != originalCode {
		t.Fatalf("marked_duplicate = %v, want reference %s", marked, originalCode)
	}
	if err := pool.QueryRow(ctx, `select new_value from public.lead_events where lead_id=$1 and event_type='repeat_request'`, original).Scan(&repeat); err != nil {
		t.Fatalf("repeat_request event: %v", err)
	}
	if repeat["duplicate_reference_id"] != duplicateCode || repeat["message"] != "second request" {
		t.Fatalf("repeat_request = %v", repeat)
	}

	if code, _ := mark(duplicate, original); code != http.StatusConflict {
		t.Fatalf("second mark = %d, want 409", code)
	}
	third, _ := insertLead(officeID, "third request")
	if code, _ := mark(third, duplicate); code != http.StatusConflict {
		t.Fatalf("duplicate as original = %d, want 409", code)
	}

	// Reports skip the duplicate.
	query, args, fields := buildLeadReportQuery(reportCriteria{}, []uuid.UUID{officeID})
	if len(fields) > 0 {
		t.Fatalf("report query fields: %v", fields)
	}
	if !strings.Contains(query, "l.duplicate_of_lead_id is null") || len(args) == 0 {
		t.Fatal("lead report must skip duplicates")
	}

	// Reopen undoes the link.
	lead, err := func() (activityLead, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return activityLead{}, err
		}
		defer tx.Rollback(ctx)
		req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
		lead, err := loadActivityLeadForUpdate(req, tx, duplicate)
		if err != nil {
			return lead, err
		}
		if err := server.applyLeadActivity(req, tx, manager, duplicate, lead, leadActivityRequest{Type: activityReopen}, nil); err != nil {
			return lead, err
		}
		return lead, tx.Commit(ctx)
	}()
	if err != nil {
		t.Fatalf("reopen: %v (%+v)", err, lead)
	}
	if err := pool.QueryRow(ctx, `select duplicate_of_lead_id from public.leads where id=$1`, duplicate).Scan(&duplicateOf); err != nil || duplicateOf != nil {
		t.Fatalf("reopen must clear duplicate_of_lead_id: %v %v", duplicateOf, err)
	}

	// An already closed lead only gets the reason and the link.
	closed, _ := insertLead(officeID, "closed request")
	mustExec(t, pool, `update public.leads set client_status='closed_lost', loss_reason='other' where id=$1`, closed)
	if code, body := mark(closed, original); code != http.StatusOK {
		t.Fatalf("mark closed lead = %d %v", code, body)
	}
	if err := pool.QueryRow(ctx, `select loss_reason from public.leads where id=$1`, closed).Scan(&lossReason); err != nil || lossReason != lossReasonDuplicate {
		t.Fatalf("closed lead reason = %q (%v)", lossReason, err)
	}
}
