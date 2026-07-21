package metaleads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

const webhookBodyLimit = 256 * 1024

type Integration struct {
	Pool              *pgxpool.Pool
	Config            Config
	Client            *Client
	Outbox            notifications.Outbox
	NotificationWaker notifications.Waker
	Logger            *slog.Logger

	wake       chan struct{}
	alertMu    sync.Mutex
	alertTimes map[string]time.Time
}

func New(pool *pgxpool.Pool, cfg Config, outbox notifications.Outbox, notificationWaker notifications.Waker, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		Pool:              pool,
		Config:            cfg,
		Client:            NewClient(cfg),
		Outbox:            outbox,
		NotificationWaker: notificationWaker,
		Logger:            logger,
		wake:              make(chan struct{}, 1),
		alertTimes:        make(map[string]time.Time),
	}
}

func (i *Integration) Enabled() bool {
	return i != nil && i.Config.Enabled
}

func (i *Integration) Wake() {
	if !i.Enabled() {
		return
	}
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (i *Integration) VerifyWebhook(w http.ResponseWriter, r *http.Request) {
	if !i.Enabled() {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	if query.Get("hub.mode") != "subscribe" || !SecureTokenEqual(query.Get("hub.verify_token"), i.Config.WebhookVerifyToken) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	challenge := strings.TrimSpace(query.Get("hub.challenge"))
	if challenge == "" {
		http.Error(w, "missing challenge", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(challenge))
}

func (i *Integration) ReceiveWebhook(w http.ResponseWriter, r *http.Request) {
	if !i.Enabled() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webhookBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !VerifySignature(i.Config.AppSecret, body, strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.Object != "page" {
		writeWebhookAccepted(w, 0)
		return
	}
	inserted, pending, err := i.storeWebhook(r.Context(), payload, body)
	if err != nil {
		i.log().Error("Meta webhook persistence failed", "error", err)
		http.Error(w, "persistence failed", http.StatusInternalServerError)
		return
	}
	if pending > 0 {
		i.Wake()
	}
	writeWebhookAccepted(w, inserted)
}

func (i *Integration) storeWebhook(ctx context.Context, payload webhookPayload, raw []byte) (inserted int, pending int, err error) {
	if i.Pool == nil {
		return 0, 0, errors.New("Meta webhook database is not configured")
	}
	tx, err := i.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	for _, event := range leadgenEvents(payload) {
		pageID := event.PageID
		status := "pending"
		lastError := (*string)(nil)
		if _, ok := i.Client.Page(pageID); !ok {
			status = "ignored"
			message := "webhook received for an unconfigured Meta page"
			lastError = &message
		}
		var wasInserted bool
		err = tx.QueryRow(ctx, `
				insert into public.meta_lead_events (
				  leadgen_id,page_id,form_id,ad_id,event_created_at,status,webhook_payload,last_error
				) values ($1,$2,nullif($3,''),nullif($4,''),$5,$6,$7::jsonb,$8)
				on conflict (leadgen_id) do nothing
				returning true
			`, event.LeadgenID, pageID, event.FormID, event.AdID, event.CreatedAt, status, raw, lastError).Scan(&wasInserted)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return inserted, pending, err
			}
			// ON CONFLICT DO NOTHING returns no row for an idempotent retry.
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, pending, err
		}
		if wasInserted {
			inserted++
			if status == "pending" {
				pending++
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return inserted, pending, nil
}

type webhookLeadEvent struct {
	LeadgenID string
	PageID    string
	FormID    string
	AdID      string
	CreatedAt *time.Time
}

func leadgenEvents(payload webhookPayload) []webhookLeadEvent {
	events := make([]webhookLeadEvent, 0)
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			leadgenID := strings.TrimSpace(change.Value.LeadgenID)
			if change.Field != "leadgen" || leadgenID == "" {
				continue
			}
			pageID := strings.TrimSpace(change.Value.PageID)
			if pageID == "" {
				pageID = strings.TrimSpace(entry.ID)
			}
			createdAt := unixTime(change.Value.CreatedAt)
			if createdAt == nil {
				createdAt = unixTime(entry.Time)
			}
			events = append(events, webhookLeadEvent{
				LeadgenID: leadgenID,
				PageID:    pageID,
				FormID:    strings.TrimSpace(change.Value.FormID),
				AdID:      strings.TrimSpace(change.Value.AdID),
				CreatedAt: createdAt,
			})
		}
	}
	return events
}

func unixTime(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	parsed := time.Unix(value, 0).UTC()
	return &parsed
}

func writeWebhookAccepted(w http.ResponseWriter, inserted int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "inserted": inserted})
}

func (i *Integration) log() *slog.Logger {
	if i != nil && i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}
