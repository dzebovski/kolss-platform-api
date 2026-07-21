package metaleads

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

func (i *Integration) alertConnectionFailure(ctx context.Context, pageID, kind, message string) {
	key := strings.TrimSpace(pageID) + ":" + strings.TrimSpace(kind)
	if key == ":" {
		key = "meta:unknown"
	}
	var lastKey *string
	var lastAlerted *time.Time
	err := i.Pool.QueryRow(ctx, `
		update public.meta_page_connections
		set health_status='unhealthy',
		    token_status=case when $3='oauth' then 'invalid' else token_status end,
		    token_checked_at=case when $3='oauth' then now() else token_checked_at end,
		    last_error=$2,last_error_at=now(),updated_at=now()
		where page_id=$1
		returning last_alert_key,last_alerted_at
	`, pageID, truncateError(message, 2000), kind).Scan(&lastKey, &lastAlerted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		i.log().Warn("Meta connection failure state update failed", "page_id", pageID, "error", err)
	}
	if lastKey != nil && *lastKey == key && lastAlerted != nil && time.Since(*lastAlerted) < time.Hour {
		return
	}
	if !i.allowInMemoryAlert(key) {
		return
	}
	alert := fmt.Sprintf(
		"⚠️ Meta Lead Ads\nPage: %s\nType: %s\n%s",
		html.EscapeString(pageID),
		html.EscapeString(kind),
		html.EscapeString(truncateError(message, 1200)),
	)
	if err := i.sendAlert(ctx, alert); err != nil {
		i.log().Warn("Meta Telegram alert failed", "page_id", pageID, "kind", kind, "error", err)
		return
	}
	_, _ = i.Pool.Exec(ctx, `
		update public.meta_page_connections
		set last_alert_key=$2,last_alerted_at=now()
		where page_id=$1
	`, pageID, key)
}

func (i *Integration) recordSyncFailure(ctx context.Context, pageID, kind string, syncErr error) {
	var failures int
	var graphErr *GraphError
	isOAuth := errors.As(syncErr, &graphErr) && graphErr.OAuth()
	if isOAuth {
		kind = "oauth"
	}
	err := i.Pool.QueryRow(ctx, `
		update public.meta_page_connections
		set health_status='unhealthy',consecutive_failures=consecutive_failures+1,
		    token_status=case when $2 then 'invalid' else token_status end,
		    token_checked_at=case when $2 then now() else token_checked_at end,
		    last_error=$3,last_error_at=now(),updated_at=now()
		where page_id=$1
		returning consecutive_failures
	`, pageID, isOAuth, truncateError(syncErr.Error(), 2000)).Scan(&failures)
	if err != nil {
		i.log().Warn("Meta sync failure state update failed", "page_id", pageID, "error", err)
	}
	if isOAuth || failures >= 3 {
		i.alertConnectionFailure(ctx, pageID, kind, syncErr.Error())
	}
}

func (i *Integration) markConnectionHealthy(ctx context.Context, pageID string) {
	var lastAlertKey *string
	err := i.Pool.QueryRow(ctx, `
		update public.meta_page_connections
		set health_status='healthy',token_status='valid',token_checked_at=now(),consecutive_failures=0,
		    last_success_at=now(),last_reconciled_at=now(),
		    last_error=null,last_error_at=null,updated_at=now()
		where page_id=$1
		returning last_alert_key
	`, pageID).Scan(&lastAlertKey)
	if err != nil {
		i.log().Warn("Meta connection health update failed", "page_id", pageID, "error", err)
		return
	}
	if lastAlertKey == nil || strings.TrimSpace(*lastAlertKey) == "" {
		return
	}
	if err := i.sendAlert(ctx, "✅ Meta Lead Ads connection recovered\nPage: "+html.EscapeString(pageID)); err != nil {
		i.log().Warn("Meta recovery alert failed", "page_id", pageID, "error", err)
		return
	}
	_, _ = i.Pool.Exec(ctx, `
		update public.meta_page_connections
		set last_alert_key=null,last_alerted_at=null
		where page_id=$1
	`, pageID)
}

func (i *Integration) alertIgnoredEvents(ctx context.Context) {
	rows, err := i.Pool.Query(ctx, `
		select distinct page_id
		from public.meta_lead_events
		where status='ignored' and alerted_at is null and last_error='webhook received for an unconfigured Meta page'
		limit 20
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	var pages []string
	for rows.Next() {
		var pageID string
		if rows.Scan(&pageID) == nil {
			pages = append(pages, pageID)
		}
	}
	for _, pageID := range pages {
		key := "unknown-page:" + pageID
		if i.allowInMemoryAlert(key) {
			_ = i.sendAlert(ctx, "⚠️ Meta webhook received from an unconfigured Page\nPage: "+html.EscapeString(pageID))
		}
		_, _ = i.Pool.Exec(ctx, `
			update public.meta_lead_events set alerted_at=now()
			where page_id=$1 and status='ignored' and alerted_at is null
		`, pageID)
	}
}

func (i *Integration) checkStaleConnections(ctx context.Context) {
	rows, err := i.Pool.Query(ctx, `
		select page_id
		from public.meta_page_connections
		where coalesce(last_success_at,created_at) < now() - interval '30 minutes'
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pageID string
		if rows.Scan(&pageID) == nil {
			i.alertConnectionFailure(ctx, pageID, "stale", "No successful reconciliation in the last 30 minutes")
		}
	}
}

func (i *Integration) allowInMemoryAlert(key string) bool {
	i.alertMu.Lock()
	defer i.alertMu.Unlock()
	now := time.Now()
	if previous, ok := i.alertTimes[key]; ok && now.Sub(previous) < time.Hour {
		return false
	}
	i.alertTimes[key] = now
	return true
}

func (i *Integration) sendAlert(ctx context.Context, message string) error {
	return notifications.SendTelegramMessage(
		ctx,
		nil,
		i.Config.AlertTelegramBotToken,
		i.Config.AlertTelegramChatID,
		message,
	)
}
