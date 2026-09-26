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
)

// TestEventCorrectionDB runs the W12 correction handler against a real database with all
// migrations applied. It only runs when KOLSS_TEST_DATABASE_URL points at a disposable database
// (never production): it writes fixtures.
func TestEventCorrectionDB(t *testing.T) {
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
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal("office fixture:", err)
	}
	userID := uuid.New()
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, userID, userID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Tester') on conflict (id) do nothing`, userID)
	actor := Actor{ID: userID, Role: "super_admin", IsActive: true, DisplayName: ptr("Tester")}

	newLead := func(t *testing.T) uuid.UUID {
		leadID := uuid.New()
		mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone) values ($1,$2,$3,'Test','+48600000000')`, leadID, officeID, leadID.String())
		return leadID
	}
	addEvent := func(t *testing.T, leadID uuid.UUID, at time.Time, eventType, category string, code *string, comment *string, oldValue, newValue map[string]any) uuid.UUID {
		id := uuid.New()
		mustExec(t, pool, `insert into public.lead_events (id, lead_id, actor_id, event_type, event_category, status_code, comment, old_value, new_value, created_at) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			id, leadID, userID, eventType, category, code, comment, oldValue, newValue, at)
		return id
	}
	correct := func(t *testing.T, leadID, eventID uuid.UUID, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPatch, "/v1/leads/x/events/y/correction", strings.NewReader(body))
		req.SetPathValue("leadId", leadID.String())
		req.SetPathValue("eventId", eventID.String())
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, actor))
		rec := httptest.NewRecorder()
		server.handleCorrectEvent(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	type leadRow struct {
		call, v2 *string
		client   string
		due      *time.Time
		attempts int
	}
	readLead := func(t *testing.T, leadID uuid.UUID) leadRow {
		var row leadRow
		if err := pool.QueryRow(ctx, `select call_status, v2_status, client_status, callback_due_at, no_answer_attempts from public.leads where id=$1`, leadID).
			Scan(&row.call, &row.v2, &row.client, &row.due, &row.attempts); err != nil {
			t.Fatal(err)
		}
		return row
	}
	base := time.Now().Add(-2 * time.Hour).UTC()
	due := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Minute)

	t.Run("latest no answer becomes call later and the lead follows", func(t *testing.T) {
		leadID := newLead(t)
		mustExec(t, pool, `update public.leads set call_status='no_answer', v2_status='noanswer', no_answer_attempts=1 where id=$1`, leadID)
		eventID := addEvent(t, leadID, base, "call_status_changed", "call_status", ptr("no_answer"), nil,
			map[string]any{"call_status": nil}, map[string]any{"v2_status": "noanswer", "attempt": 1})
		code, body := correct(t, leadID, eventID, `{"type":"later","dueAt":"`+due.Format(time.RFC3339)+`","reason":"Pressed No answer by mistake"}`)
		if code != http.StatusOK || body["leadStatusChanged"] != true {
			t.Fatalf("status %d body %v", code, body)
		}
		row := readLead(t, leadID)
		if row.call == nil || *row.call != "callback_requested" || row.v2 == nil || *row.v2 != "later" || row.attempts != 0 || row.due == nil || !row.due.Equal(due) {
			t.Fatalf("lead after correction: %+v", row)
		}
		var eventType, statusCode string
		var corrections int
		if err := pool.QueryRow(ctx, `select event_type, status_code, jsonb_array_length(new_value->'corrections') from public.lead_events where id=$1`, eventID).Scan(&eventType, &statusCode, &corrections); err != nil {
			t.Fatal(err)
		}
		if eventType != "call_status_changed" || statusCode != "callback_requested" || corrections != 1 {
			t.Fatalf("event after correction: %s %s %d", eventType, statusCode, corrections)
		}
	})

	t.Run("older entry changes history only", func(t *testing.T) {
		leadID := newLead(t)
		mustExec(t, pool, `update public.leads set call_status='reached', v2_status='success' where id=$1`, leadID)
		older := addEvent(t, leadID, base, "call_status_changed", "call_status", ptr("no_answer"), nil, map[string]any{"call_status": nil}, map[string]any{})
		addEvent(t, leadID, base.Add(time.Hour), "call_status_changed", "call_status", ptr("reached"), ptr("Talked"), map[string]any{"call_status": "no_answer"}, map[string]any{})
		code, body := correct(t, leadID, older, `{"type":"later","dueAt":"`+due.Format(time.RFC3339)+`","reason":"Wrong type"}`)
		if code != http.StatusOK || body["leadStatusChanged"] != false {
			t.Fatalf("status %d body %v", code, body)
		}
		if row := readLead(t, leadID); row.v2 == nil || *row.v2 != "success" {
			t.Fatalf("lead must keep success: %+v", row)
		}
	})

	t.Run("latest thinking becomes a comment and the status reverts", func(t *testing.T) {
		leadID := newLead(t)
		mustExec(t, pool, `update public.leads set call_status='reached', client_status='thinking', v2_status='thinking', callback_due_at=$2 where id=$1`, leadID, due)
		addEvent(t, leadID, base, "call_status_changed", "call_status", ptr("reached"), ptr("Talked"), map[string]any{"call_status": nil}, map[string]any{})
		thinking := addEvent(t, leadID, base.Add(time.Hour), "client_status_changed", "client_status", ptr("thinking"), ptr("Comparing studios"),
			map[string]any{"client_status": "new_lead"}, map[string]any{"v2_status": "thinking"})
		code, body := correct(t, leadID, thinking, `{"type":"comment","reason":"It was only a note"}`)
		if code != http.StatusOK {
			t.Fatalf("status %d body %v", code, body)
		}
		row := readLead(t, leadID)
		if row.client != "new_lead" || row.v2 == nil || *row.v2 != "success" || row.due != nil {
			t.Fatalf("lead after revert: %+v", row)
		}
	})

	t.Run("rejects lost entries and missing reason", func(t *testing.T) {
		leadID := newLead(t)
		lost := addEvent(t, leadID, base, "client_status_changed", "client_status", ptr("closed_lost"), ptr("x"), map[string]any{}, map[string]any{})
		if code, _ := correct(t, leadID, lost, `{"type":"success","reason":"x"}`); code != http.StatusBadRequest {
			t.Fatalf("lost entry: status %d", code)
		}
		note := addEvent(t, leadID, base, "comment_added", "comment", nil, ptr("Note"), map[string]any{}, map[string]any{})
		if code, _ := correct(t, leadID, note, `{"comment":"Fixed"}`); code != http.StatusBadRequest {
			t.Fatalf("missing reason: status %d", code)
		}
	})
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(sql)[0:3], err)
	}
}
