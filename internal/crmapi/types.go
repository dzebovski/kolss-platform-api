package crmapi

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type contextKey struct{}

type Actor struct {
	ID          uuid.UUID
	Role        string
	DisplayName *string
	Email       string
	IsActive    bool
	OfficeIDs   map[uuid.UUID]struct{}
}

func (a Actor) IsSuperAdmin() bool { return a.Role == "super_admin" }

func (a Actor) CanAccessOffice(id uuid.UUID) bool {
	if a.IsSuperAdmin() {
		return true
	}
	_, ok := a.OfficeIDs[id]
	return ok
}

func (a Actor) CanEditLead(id uuid.UUID) bool {
	return a.CanAccessOffice(id)
}

func (a Actor) CanArchiveLead(id uuid.UUID) bool {
	return a.IsSuperAdmin() || (a.Role == "office_admin" && a.CanAccessOffice(id))
}

// CanMutateLeadEvent allows super_admin for any event, or the event author when they
// still have access to the lead's office. Events with a missing actor are super_admin-only.
func (a Actor) CanMutateLeadEvent(officeID uuid.UUID, eventActorID uuid.UUID) bool {
	if a.IsSuperAdmin() {
		return true
	}
	if eventActorID == uuid.Nil {
		return false
	}
	return a.CanAccessOffice(officeID) && a.ID == eventActorID
}

func actorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(contextKey{}).(Actor)
	return actor, ok
}

type Office struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	NameUK       string    `json:"name_uk"`
	NamePL       string    `json:"name_pl"`
	TimezoneName string    `json:"timezone_name"`
	IsActive     bool      `json:"is_active"`
}

type Profile struct {
	ID            uuid.UUID  `json:"id"`
	Role          string     `json:"role"`
	DisplayName   *string    `json:"display_name"`
	IsActive      bool       `json:"is_active"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Permissions struct {
	CanManageUsers    bool `json:"canManageUsers"`
	CanEditLeadFields bool `json:"canEditLeadFields"`
	CanArchiveLeads   bool `json:"canArchiveLeads"`
	CanRestoreLeads   bool `json:"canRestoreLeads"`
}

type MeResponse struct {
	User struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email,omitempty"`
	} `json:"user"`
	Profile     Profile     `json:"profile"`
	Offices     []Office    `json:"offices"`
	UserOffices []Office    `json:"userOffices"`
	Permissions Permissions `json:"permissions"`
}

type errorResponse struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"fieldErrors,omitempty"`
	RequestID   string            `json:"requestId"`
}
