package crmapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeadJSONExpressionEmbedsChronologicalFirstContactAttempt(t *testing.T) {
	expr := leadJSONExpression

	for _, fragment := range []string{
		"'first_contact_attempt'",
		"from public.lead_contact_attempts a",
		"where a.lead_id = l.id",
		"order by a.created_at asc",
		"limit 1",
		"'result', a.result",
		"'comment', a.comment",
		"'created_at', a.created_at",
		"'manager_id', a.manager_id",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}

	if strings.Contains(expr, "order by a.created_at desc") {
		t.Fatal("first_contact_attempt must use chronological first attempt (asc), not latest (desc)")
	}
}

func TestLeadJSONExpressionEmbedsSharedMarkers(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'markers'",
		"from public.lead_markers m",
		"left join public.profiles mp on mp.id = m.actor_id",
		"'kind', m.kind",
		"'actor_id', m.actor_id",
		"'actor_name', coalesce(mp.display_name, '')",
		"'marked_at', m.marked_at",
		"'[]'::jsonb",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestFirstContactAttemptListJSONShape(t *testing.T) {
	managerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	createdAt := time.Date(2026, 7, 14, 14, 14, 0, 0, time.UTC)

	withAttempt := map[string]any{
		"id": "22222222-2222-2222-2222-222222222222",
		"first_contact_attempt": map[string]any{
			"result":     "reached",
			"comment":    "Клиент підтвердив потребу",
			"created_at": createdAt.Format(time.RFC3339),
			"manager_id": managerID.String(),
		},
	}
	withoutAttempt := map[string]any{
		"id":                    "33333333-3333-3333-3333-333333333333",
		"first_contact_attempt": nil,
	}

	raw, err := json.Marshal(map[string]any{"items": []any{withAttempt, withoutAttempt}})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Items []struct {
			ID                  string          `json:"id"`
			FirstContactAttempt json.RawMessage `json:"first_contact_attempt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("items=%d", len(decoded.Items))
	}

	var attempt struct {
		Result    string    `json:"result"`
		Comment   string    `json:"comment"`
		CreatedAt time.Time `json:"created_at"`
		ManagerID uuid.UUID `json:"manager_id"`
	}
	if err := json.Unmarshal(decoded.Items[0].FirstContactAttempt, &attempt); err != nil {
		t.Fatalf("lead with attempt: %v", err)
	}
	if attempt.Result != "reached" || attempt.Comment == "" || attempt.ManagerID != managerID {
		t.Fatalf("unexpected attempt: %#v", attempt)
	}

	if string(decoded.Items[1].FirstContactAttempt) != "null" {
		t.Fatalf("lead without attempt: want null, got %s", decoded.Items[1].FirstContactAttempt)
	}
}

func TestLeadJSONExpressionEmbedsContractFromSuccessfulEvent(t *testing.T) {
	expr := leadJSONExpression

	for _, fragment := range []string{
		"'contract'",
		"from public.lead_events e",
		"e.event_type in ('successful', 'contract_signed')",
		"e.new_value ? 'amount'",
		"'contract_number', e.new_value->>'contract_number'",
		"'amount', (e.new_value->>'amount')::numeric",
		"'currency', e.new_value->>'currency'",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestContractListJSONShape(t *testing.T) {
	signedAt := time.Date(2026, 6, 18, 13, 20, 0, 0, time.UTC)

	withContract := map[string]any{
		"id": "22222222-2222-2222-2222-222222222222",
		"contract": map[string]any{
			"contract_number": "K-KY-2026-0618",
			"amount":          29800,
			"currency":        "EUR",
			"signed_at":       signedAt.Format(time.RFC3339),
		},
	}
	withoutContract := map[string]any{
		"id":       "33333333-3333-3333-3333-333333333333",
		"contract": nil,
	}

	raw, err := json.Marshal(map[string]any{"items": []any{withContract, withoutContract}})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Items []struct {
			ID       string          `json:"id"`
			Contract json.RawMessage `json:"contract"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("items=%d", len(decoded.Items))
	}

	var contract struct {
		ContractNumber string  `json:"contract_number"`
		Amount         float64 `json:"amount"`
		Currency       string  `json:"currency"`
		SignedAt       string  `json:"signed_at"`
	}
	if err := json.Unmarshal(decoded.Items[0].Contract, &contract); err != nil {
		t.Fatalf("lead with contract: %v", err)
	}
	if contract.ContractNumber != "K-KY-2026-0618" || contract.Amount != 29800 || contract.Currency != "EUR" {
		t.Fatalf("unexpected contract: %#v", contract)
	}

	if string(decoded.Items[1].Contract) != "null" {
		t.Fatalf("lead without contract: want null, got %s", decoded.Items[1].Contract)
	}
}
