package crmapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// impersonationHeader is the optional header carrying the target user to
// impersonate. A real, active super_admin may set it to act as another
// (non super_admin) manager for the duration of the request.
const impersonationHeader = "X-Impersonate-User-Id"

// errInvalidImpersonation is returned by resolveEffectiveActor whenever an
// impersonation attempt is not allowed. The middleware maps it to HTTP 403
// with error code "invalid_impersonation".
var errInvalidImpersonation = errors.New("invalid_impersonation")

// resolveEffectiveActor determines which Actor should be treated as the
// effective actor for a request given the real (authenticated) actor and the
// raw value of the impersonation header.
//
// Rules:
//   - Empty/whitespace header -> return real actor unchanged.
//   - Header set but real actor is not a super_admin -> errInvalidImpersonation.
//   - Header is not a valid UUID -> errInvalidImpersonation.
//   - load(target) fails (missing/inactive) -> errInvalidImpersonation.
//   - Target is itself a super_admin -> errInvalidImpersonation.
//   - Otherwise -> return the loaded target as the effective actor.
func resolveEffectiveActor(real Actor, header string, load func(uuid.UUID) (Actor, error)) (Actor, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return real, nil
	}
	if !real.IsSuperAdmin() {
		return Actor{}, errInvalidImpersonation
	}
	targetID, err := uuid.Parse(header)
	if err != nil {
		return Actor{}, errInvalidImpersonation
	}
	target, err := load(targetID)
	if err != nil {
		return Actor{}, errInvalidImpersonation
	}
	if target.IsSuperAdmin() {
		return Actor{}, errInvalidImpersonation
	}
	return target, nil
}

// applyImpersonation resolves the effective actor from the request headers.
// It returns the effective actor and true on success. On an invalid
// impersonation attempt it writes a 403 invalid_impersonation error and
// returns false, signalling the caller to stop processing the request.
func (s *Server) applyImpersonation(w http.ResponseWriter, r *http.Request, real Actor) (Actor, bool) {
	effective, err := resolveEffectiveActor(real, r.Header.Get(impersonationHeader), func(id uuid.UUID) (Actor, error) {
		return s.loadActor(r.Context(), id)
	})
	if err != nil {
		s.writeError(w, r, http.StatusForbidden, "invalid_impersonation", "Impersonation is not permitted", nil)
		return Actor{}, false
	}
	return effective, true
}
