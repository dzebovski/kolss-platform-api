package crmapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestResolveEffectiveActor(t *testing.T) {
	t.Parallel()

	adminID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	managerID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	otherAdminID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	officeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	admin := Actor{
		ID:        adminID,
		Role:      "super_admin",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}
	manager := Actor{
		ID:       managerID,
		Role:     "office_member",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}
	otherAdmin := Actor{
		ID:        otherAdminID,
		Role:      "super_admin",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}
	member := Actor{
		ID:        uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Role:      "office_member",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}

	tests := []struct {
		name       string
		real       Actor
		header     string
		load       func(uuid.UUID) (Actor, error)
		wantID     uuid.UUID
		wantRole   string
		wantErr    bool
		wantOffice uuid.UUID
	}{
		{
			name:     "no header returns real",
			real:     admin,
			header:   "",
			wantID:   adminID,
			wantRole: "super_admin",
		},
		{
			name:     "whitespace header returns real",
			real:     admin,
			header:   "   ",
			wantID:   adminID,
			wantRole: "super_admin",
		},
		{
			name:    "non super_admin with header rejected",
			real:    member,
			header:  managerID.String(),
			wantErr: true,
		},
		{
			name:   "missing target rejected",
			real:   admin,
			header: managerID.String(),
			load: func(uuid.UUID) (Actor, error) {
				return Actor{}, errors.New("inactive or missing profile")
			},
			wantErr: true,
		},
		{
			name:   "target super_admin rejected",
			real:   admin,
			header: otherAdminID.String(),
			load: func(id uuid.UUID) (Actor, error) {
				if id != otherAdminID {
					t.Fatalf("unexpected load id %s", id)
				}
				return otherAdmin, nil
			},
			wantErr: true,
		},
		{
			name:    "invalid uuid rejected",
			real:    admin,
			header:  "not-a-uuid",
			wantErr: true,
		},
		{
			name:   "active manager succeeds",
			real:   admin,
			header: managerID.String(),
			load: func(id uuid.UUID) (Actor, error) {
				if id != managerID {
					t.Fatalf("unexpected load id %s", id)
				}
				return manager, nil
			},
			wantID:     managerID,
			wantRole:   "office_member",
			wantOffice: officeID,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			load := tt.load
			if load == nil {
				load = func(uuid.UUID) (Actor, error) {
					t.Fatal("load should not be called")
					return Actor{}, nil
				}
			}
			got, err := resolveEffectiveActor(tt.real, tt.header, load)
			if tt.wantErr {
				if !errors.Is(err, errInvalidImpersonation) {
					t.Fatalf("expected errInvalidImpersonation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.wantID || got.Role != tt.wantRole {
				t.Fatalf("got %+v, want id=%s role=%s", got, tt.wantID, tt.wantRole)
			}
			if tt.wantOffice != uuid.Nil {
				if _, ok := got.OfficeIDs[tt.wantOffice]; !ok {
					t.Fatalf("expected office %s on effective actor", tt.wantOffice)
				}
			}
		})
	}
}

func TestApplyCORSIncludesImpersonationHeader(t *testing.T) {
	t.Parallel()

	s := &Server{allowedOrigins: map[string]struct{}{"http://localhost:4200": {}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/me", nil)
	req.Header.Set("Origin", "http://localhost:4200")
	s.applyCORS(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(got, "X-Impersonate-User-Id") {
		t.Fatalf("CORS allow-headers missing X-Impersonate-User-Id: %q", got)
	}
}

func TestApplyImpersonationPutsEffectiveActorInContext(t *testing.T) {
	t.Parallel()

	adminID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	managerID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	admin := Actor{ID: adminID, Role: "super_admin", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{}}
	manager := Actor{ID: managerID, Role: "office_member", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{}}

	var seen Actor
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFromContext(r.Context())
		if !ok {
			t.Fatal("expected actor in context")
		}
		seen = actor
		w.WriteHeader(http.StatusNoContent)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &Server{}
		effective, err := resolveEffectiveActor(admin, r.Header.Get(impersonationHeader), func(id uuid.UUID) (Actor, error) {
			if id == managerID {
				return manager, nil
			}
			return Actor{}, errors.New("missing")
		})
		if err != nil {
			s.writeError(w, r, http.StatusForbidden, "invalid_impersonation", "Impersonation is not permitted", nil)
			return
		}
		downstream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, effective)))
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set(impersonationHeader, managerID.String())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen.ID != managerID || seen.Role != "office_member" {
		t.Fatalf("downstream saw %+v, want manager", seen)
	}
}

func TestApplyImpersonationRejectsNonAdmin(t *testing.T) {
	t.Parallel()

	member := Actor{
		ID:        uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Role:      "office_member",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}
	managerID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set(impersonationHeader, managerID.String())
	rec := httptest.NewRecorder()

	_, ok := s.applyImpersonation(rec, req, member)
	if ok {
		t.Fatal("expected applyImpersonation to fail for non-admin")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
	var body errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "invalid_impersonation" {
		t.Fatalf("code=%q", body.Code)
	}
}
