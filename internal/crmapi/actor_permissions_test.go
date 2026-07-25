package crmapi

import (
	"testing"

	"github.com/google/uuid"
)

func TestActorLeadPermissions(t *testing.T) {
	t.Parallel()

	officeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	otherOfficeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	superAdmin := Actor{
		ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Role:      "super_admin",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}
	officeAdmin := Actor{
		ID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Role:     "office_admin",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}
	member := Actor{
		ID:       uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Role:     "office_member",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}
	curator := Actor{
		ID:       uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Role:     "curator",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}

	tests := []struct {
		name       string
		actor      Actor
		officeID   uuid.UUID
		wantEdit   bool
		wantArchive bool
	}{
		{name: "super_admin edits and archives any office", actor: superAdmin, officeID: otherOfficeID, wantEdit: true, wantArchive: true},
		{name: "office_admin edits and archives own office", actor: officeAdmin, officeID: officeID, wantEdit: true, wantArchive: true},
		{name: "office_admin blocked for other office", actor: officeAdmin, officeID: otherOfficeID, wantEdit: false, wantArchive: false},
		{name: "office_member edits own office but cannot archive", actor: member, officeID: officeID, wantEdit: true, wantArchive: false},
		{name: "office_member blocked for other office", actor: member, officeID: otherOfficeID, wantEdit: false, wantArchive: false},
		{name: "curator edits own office but cannot archive", actor: curator, officeID: officeID, wantEdit: true, wantArchive: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.actor.CanEditLead(tt.officeID); got != tt.wantEdit {
				t.Fatalf("CanEditLead = %v, want %v", got, tt.wantEdit)
			}
			if got := tt.actor.CanArchiveLead(tt.officeID); got != tt.wantArchive {
				t.Fatalf("CanArchiveLead = %v, want %v", got, tt.wantArchive)
			}
		})
	}
}

func TestActorCanMutateLeadEvent(t *testing.T) {
	t.Parallel()

	officeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	otherOfficeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	authorID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	otherAuthorID := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	superAdmin := Actor{
		ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Role:      "super_admin",
		IsActive:  true,
		OfficeIDs: map[uuid.UUID]struct{}{},
	}
	member := Actor{
		ID:       authorID,
		Role:     "office_member",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}
	officeAdmin := Actor{
		ID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Role:     "office_admin",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}
	curator := Actor{
		ID:       uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Role:     "curator",
		IsActive: true,
		OfficeIDs: map[uuid.UUID]struct{}{
			officeID: {},
		},
	}

	tests := []struct {
		name         string
		actor        Actor
		officeID     uuid.UUID
		eventActorID uuid.UUID
		want         bool
	}{
		{name: "super_admin mutates any event", actor: superAdmin, officeID: otherOfficeID, eventActorID: otherAuthorID, want: true},
		{name: "super_admin mutates event with nil actor", actor: superAdmin, officeID: officeID, eventActorID: uuid.Nil, want: true},
		{name: "author member mutates own event", actor: member, officeID: officeID, eventActorID: authorID, want: true},
		{name: "member blocked for other author", actor: member, officeID: officeID, eventActorID: otherAuthorID, want: false},
		{name: "member blocked without office access", actor: member, officeID: otherOfficeID, eventActorID: authorID, want: false},
		{name: "member blocked for nil actor", actor: member, officeID: officeID, eventActorID: uuid.Nil, want: false},
		{name: "office_admin mutates own event", actor: officeAdmin, officeID: officeID, eventActorID: officeAdmin.ID, want: true},
		{name: "office_admin blocked for other author", actor: officeAdmin, officeID: officeID, eventActorID: authorID, want: false},
		{name: "curator mutates own event", actor: curator, officeID: officeID, eventActorID: curator.ID, want: true},
		{name: "curator blocked for other author", actor: curator, officeID: officeID, eventActorID: authorID, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.actor.CanMutateLeadEvent(tt.officeID, tt.eventActorID); got != tt.want {
				t.Fatalf("CanMutateLeadEvent = %v, want %v", got, tt.want)
			}
		})
	}
}
