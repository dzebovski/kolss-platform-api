package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/dzebovski/kolss-platform-api/internal/botcheck"
	"github.com/dzebovski/kolss-platform-api/internal/submissions"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

type LeadService interface {
	Ping(ctx context.Context) error
	Create(ctx context.Context, siteCode string, data validation.ValidatedLeadSubmission) (submissions.CreateResult, error)
}

type Server struct {
	svc            LeadService
	bots           botcheck.BotVerifier
	requireBot     bool
	logger         *slog.Logger
	allowedOrigins map[string]struct{}
	bodyLimit      int64
	rateLimiter    *rateLimiter
	enabled        bool
}

type Options struct {
	Enabled            bool
	AllowedOrigins     []string
	BodyLimitBytes     int64
	RateLimitPerMinute int
	RequireBotToken    bool
	BotVerifier        botcheck.BotVerifier
	Logger             *slog.Logger
}

func NewServer(svc LeadService, opts Options) *Server {
	origins := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, o := range opts.AllowedOrigins {
		origins[o] = struct{}{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bots := opts.BotVerifier
	if bots == nil {
		bots = botcheck.DisabledVerifier{}
	}
	return &Server{
		svc:            svc,
		bots:           bots,
		requireBot:     opts.RequireBotToken,
		logger:         logger,
		allowedOrigins: origins,
		bodyLimit:      opts.BodyLimitBytes,
		rateLimiter:    newRateLimiter(opts.RateLimitPerMinute),
		enabled:        opts.Enabled,
	}
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(chimiddleware.Recoverer)
	s.RegisterRoutes(router)
	return router
}

func (s *Server) RegisterRoutes(router chi.Router) {
	router.Group(func(r chi.Router) {
		r.Use(s.Middleware)
		r.Get("/health/live", s.handleLive)
		r.Get("/health/ready", s.handleReady)
		r.Post("/v1/public/sites/{siteCode}/lead-submissions", s.handleCreate)
		r.Options("/v1/public/sites/{siteCode}/lead-submissions", s.handleOptions)
	})
}

func (s *Server) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		r = r.WithContext(ctx)

		s.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if _, ok := s.allowedOrigins[origin]; !ok {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r.Context())
	if err := s.svc.Ping(r.Context()); err != nil {
		s.logger.Error("readiness check failed", "error", err, "request_id", requestID)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "service not ready", requestID, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createRequest struct {
	IdempotencyKey       string  `json:"idempotency_key"`
	Name                 string  `json:"name"`
	Phone                string  `json:"phone"`
	Email                *string `json:"email"`
	City                 *string `json:"city"`
	ProjectDescription   *string `json:"project_description"`
	PrivacyAccepted      bool    `json:"privacy_accepted"`
	PrivacyPolicyVersion string  `json:"privacy_policy_version"`
	PageURL              *string `json:"page_url"`
	BotToken             string  `json:"bot_token"`
	Website              *string `json:"website"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r.Context())
	if !s.enabled {
		writeError(w, http.StatusServiceUnavailable, "feature_disabled", "public site forms are disabled", requestID, nil)
		return
	}
	siteCode := r.PathValue("siteCode")

	if !validation.IsAllowedSiteCode(siteCode) {
		writeError(w, http.StatusNotFound, "site_not_found", "site not found", requestID, nil)
		return
	}

	ip := clientIP(r)
	if !s.rateLimiter.allow(ip + "|" + siteCode) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests", requestID, nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit)
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createRequest
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large", requestID, nil)
			return
		}
		writeError(w, http.StatusBadRequest, "validation_error", "invalid JSON body", requestID, nil)
		return
	}

	validated, fieldErrs, code := validation.ValidateLeadSubmission(validation.LeadSubmissionInput{
		IdempotencyKey:       req.IdempotencyKey,
		Name:                 req.Name,
		Phone:                req.Phone,
		Email:                req.Email,
		City:                 req.City,
		ProjectDescription:   req.ProjectDescription,
		PrivacyAccepted:      req.PrivacyAccepted,
		PrivacyPolicyVersion: req.PrivacyPolicyVersion,
		PageURL:              req.PageURL,
		BotToken:             req.BotToken,
		Website:              req.Website,
		RequireBotToken:      s.requireBot,
	})
	if code == "consent_required" {
		writeError(w, http.StatusBadRequest, "consent_required", "privacy consent is required", requestID, fieldErrs)
		return
	}
	if code != "" {
		writeError(w, http.StatusBadRequest, code, "request validation failed", requestID, fieldErrs)
		return
	}

	if validated.HoneypotTriggered {
		writeJSON(w, http.StatusCreated, map[string]any{
			"submission_id": uuid.Nil.String(),
			"status":        "accepted",
			"duplicate":     false,
			"lead_id":       uuid.Nil.String(),
			"request_id":    requestID,
		})
		return
	}

	if err := s.bots.Verify(r.Context(), validated.BotToken, ip); err != nil {
		switch {
		case errors.Is(err, botcheck.ErrProviderUnavailable):
			writeError(w, http.StatusServiceUnavailable, "bot_provider_unavailable", "anti-bot provider unavailable", requestID, nil)
		default:
			writeError(w, http.StatusForbidden, "bot_check_failed", "bot verification failed", requestID, nil)
		}
		return
	}
	validated.BotToken = "" // never persist/log

	result, err := s.svc.Create(r.Context(), siteCode, validated)
	if err != nil {
		s.writeCreateErr(w, err, requestID, siteCode)
		return
	}

	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"submission_id": result.SubmissionID.String(),
		"lead_id":       result.LeadID.String(),
		"status":        result.Status,
		"duplicate":     result.Duplicate,
		"request_id":    requestID,
	})
}

func (s *Server) writeCreateErr(w http.ResponseWriter, err error, requestID, siteCode string) {
	switch {
	case errors.Is(err, submissions.ErrSiteNotFound):
		writeError(w, http.StatusNotFound, "site_not_found", "site not found", requestID, nil)
	case errors.Is(err, submissions.ErrPrivacyVersionMismatch):
		writeError(w, http.StatusBadRequest, "privacy_version_mismatch", "privacy policy version does not match active site version", requestID, []validation.FieldError{{
			Field: "privacy_policy_version", Message: "does not match active site version",
		}})
	case errors.Is(err, submissions.ErrSubmissionConflict):
		writeError(w, http.StatusConflict, "submission_conflict", "submission state conflict", requestID, nil)
	default:
		s.logger.Error("create submission failed", "error", err, "request_id", requestID, "site_code", siteCode)
		writeError(w, http.StatusInternalServerError, "internal_error", "unexpected server error", requestID, nil)
	}
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok && v != "" {
		return v
	}
	return uuid.NewString()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string, details []validation.FieldError) {
	body := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
		"request_id": requestID,
	}
	if len(details) > 0 {
		body["error"].(map[string]any)["details"] = details
	}
	writeJSON(w, status, body)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	requests map[string][]time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 30
	}
	return &rateLimiter{
		limit:    perMinute,
		window:   time.Minute,
		requests: make(map[string][]time.Time),
	}
}

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := now.Add(-rl.window)
	kept := rl.requests[key][:0]
	for _, t := range rl.requests[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.requests[key] = kept
		return false
	}
	rl.requests[key] = append(kept, now)
	return true
}
