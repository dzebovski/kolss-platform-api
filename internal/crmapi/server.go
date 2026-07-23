package crmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	platformauth "github.com/dzebovski/kolss-platform-api/internal/auth"
	"github.com/dzebovski/kolss-platform-api/internal/metaleads"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

type Options struct {
	Pool              *pgxpool.Pool
	Verifier          *platformauth.Verifier
	AllowedOrigins    []string
	SupabaseURL       string
	SupabaseSecretKey string
	CRMSiteURLPublic  string
	Outbox            notifications.Outbox
	NotificationWaker notifications.Waker
	Storage           storage.ObjectStorage
	Translator        Translator
	Logger            *slog.Logger
	MetaIntegration   *metaleads.Integration
}

type Server struct {
	pool              *pgxpool.Pool
	verifier          *platformauth.Verifier
	allowedOrigins    map[string]struct{}
	supabaseURL       string
	supabaseSecretKey string
	crmSiteURLPublic  string
	outbox            notifications.Outbox
	notificationWaker notifications.Waker
	storage           storage.ObjectStorage
	translator        Translator
	logger            *slog.Logger
	metaIntegration   *metaleads.Integration
}

func New(opts Options) *Server {
	origins := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, origin := range opts.AllowedOrigins {
		origins[strings.TrimSpace(origin)] = struct{}{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		pool:              opts.Pool,
		verifier:          opts.Verifier,
		allowedOrigins:    origins,
		supabaseURL:       strings.TrimRight(opts.SupabaseURL, "/"),
		supabaseSecretKey: opts.SupabaseSecretKey,
		crmSiteURLPublic:  strings.TrimRight(opts.CRMSiteURLPublic, "/"),
		outbox:            opts.Outbox,
		notificationWaker: opts.NotificationWaker,
		storage:           opts.Storage,
		translator:        opts.Translator,
		logger:            logger,
		metaIntegration:   opts.MetaIntegration,
	}
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	s.RegisterRoutes(router)
	return router
}

func (s *Server) RegisterRoutes(router chi.Router) {
	router.Group(func(r chi.Router) {
		r.Use(s.BaseMiddleware)
		r.Use(s.recoverPanic)
		if s.metaIntegration != nil {
			r.Get("/v1/integrations/meta/webhook", s.metaIntegration.VerifyWebhook)
			r.Post("/v1/integrations/meta/webhook", s.metaIntegration.ReceiveWebhook)
		}

		r.Group(func(r chi.Router) {
			r.Use(s.AuthMiddleware)
			r.Get("/v1/me", s.handleMe)
			r.Get("/v1/offices", s.handleOffices)
			r.Get("/v1/loss-reasons", s.handleLossReasons)
			r.Get("/v1/leads", s.handleListLeads)
			r.Post("/v1/leads", s.handleCreateLead)
			r.Get("/v1/leads/{leadId}", s.handleGetLead)
			r.Patch("/v1/leads/{leadId}", s.handleUpdateLead)
			r.Put("/v1/leads/{leadId}/markers/{kind}", s.handleSetLeadMarker)
			r.Delete("/v1/leads/{leadId}/markers/{kind}", s.handleDeleteLeadMarker)
			r.Patch("/v1/leads/{leadId}/events/{eventId}", s.handleUpdateEvent)
			r.Delete("/v1/leads/{leadId}/events/{eventId}", s.handleDeleteEvent)
			r.Post("/v1/leads/{leadId}/events/{eventId}/translate", s.handleTranslateEvent)
			r.Post("/v1/leads/{leadId}/archive", s.handleArchiveLead)
			r.Post("/v1/leads/{leadId}/restore", s.handleRestoreLead)
			r.Post("/v1/leads/{leadId}/delete", s.handleDeleteLead)
			r.Post("/v1/leads/{leadId}/activities", s.handleLeadActivity)
			r.Post("/v1/leads/{leadId}/actions/{action}", s.handleDeprecatedLeadAction)
			r.Get("/v1/appointments", s.handleListAppointments)
			r.Post("/v1/appointments", s.handleCreateAppointment)
			r.Patch("/v1/appointments/{appointmentId}", s.handleUpdateAppointment)
			r.Get("/v1/users", s.handleListUsers)
			r.Get("/v1/managers", s.handleListManagers)
			r.Post("/v1/users", s.handleCreateUser)
			r.Get("/v1/users/{userId}", s.handleGetUser)
			r.Patch("/v1/users/{userId}", s.handleUpdateUser)
			r.Post("/v1/users/{userId}/deactivate", s.handleDeactivateUser)
			r.Post("/v1/users/{userId}/reactivate", s.handleReactivateUser)
			r.Post("/v1/users/{userId}/delete", s.handleDeleteUser)
			r.Get("/v1/dashboard/overview", s.handleDashboardOverview)
			r.Get("/v1/reports/leads", s.handleLeadReport)
			r.Get("/v1/files/{fileId}/download-url", s.handleFileDownloadURL)
		})

		for _, pattern := range crmCORSRoutePatterns {
			r.Options(pattern, s.handleOptions)
		}
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				s.logger.Error(
					"crm api panic recovered",
					"panic", recovered,
					"request_id", requestID(r.Context()),
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if r.Header.Get("Connection") != "Upgrade" {
					writeJSON(w, http.StatusInternalServerError, errorResponse{
						Code:      "internal_error",
						Message:   "Internal server error",
						RequestID: requestID(r.Context()),
					})
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

var crmCORSRoutePatterns = []string{
	"/v1/me",
	"/v1/offices",
	"/v1/loss-reasons",
	"/v1/leads",
	"/v1/leads/{leadId}",
	"/v1/leads/{leadId}/markers/{kind}",
	"/v1/leads/{leadId}/events/{eventId}",
	"/v1/leads/{leadId}/events/{eventId}/translate",
	"/v1/leads/{leadId}/archive",
	"/v1/leads/{leadId}/restore",
	"/v1/leads/{leadId}/delete",
	"/v1/leads/{leadId}/activities",
	"/v1/leads/{leadId}/actions/{action}",
	"/v1/appointments",
	"/v1/appointments/{appointmentId}",
	"/v1/users",
	"/v1/managers",
	"/v1/users/{userId}",
	"/v1/users/{userId}/deactivate",
	"/v1/users/{userId}/reactivate",
	"/v1/users/{userId}/delete",
	"/v1/dashboard/overview",
	"/v1/reports/leads",
	"/v1/files/{fileId}/download-url",
}

func (s *Server) BaseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", requestID)
		w.Header().Set("Content-Type", "application/json")
		s.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := s.authenticate(r)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", nil)
			return
		}
		effective, ok := s.applyImpersonation(w, r, actor)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, effective)))
	})
}

func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

type requestIDContextKey struct{}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}

func (s *Server) authenticate(r *http.Request) (Actor, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(authHeader, "Bearer ") || s.verifier == nil {
		return Actor{}, errors.New("missing bearer")
	}
	claims, err := s.verifier.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")))
	if err != nil {
		return Actor{}, err
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Actor{}, err
	}
	actor, err := s.loadActor(r.Context(), userID)
	actor.Email = claims.Email
	return actor, err
}

func (s *Server) loadActor(ctx context.Context, userID uuid.UUID) (Actor, error) {
	var actor Actor
	actor.ID = userID
	actor.OfficeIDs = make(map[uuid.UUID]struct{})
	if err := s.pool.QueryRow(ctx, `
		select role::text, display_name, is_active
		from public.profiles where id = $1
	`, userID).Scan(&actor.Role, &actor.DisplayName, &actor.IsActive); err != nil || !actor.IsActive {
		return Actor{}, errors.New("inactive or missing profile")
	}
	rows, err := s.pool.Query(ctx, `select office_id from public.user_office_memberships where user_id = $1`, userID)
	if err != nil {
		return Actor{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var officeID uuid.UUID
		if err := rows.Scan(&officeID); err != nil {
			return Actor{}, err
		}
		actor.OfficeIDs[officeID] = struct{}{}
	}
	return actor, rows.Err()
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if _, ok := s.allowedOrigins[origin]; origin != "" && ok {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, If-Match, X-Request-Id, X-Impersonate-User-Id")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	if limit <= 0 {
		limit = 64 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, fields map[string]string) {
	if status >= http.StatusInternalServerError {
		s.logger.Error("crm api request failed", "status", status, "code", code, "request_id", requestID(r.Context()), "path", r.URL.Path)
	}
	writeJSON(w, status, errorResponse{Code: code, Message: message, FieldErrors: fields, RequestID: requestID(r.Context())})
}

func parseIfMatch(r *http.Request) (int64, bool) {
	raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if raw == "" {
		return 0, false
	}
	var value int64
	_, err := fmt.Sscan(raw, &value)
	return value, err == nil && value > 0
}

func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 30 * time.Second
	case 1:
		return 2 * time.Minute
	case 2:
		return 10 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}
