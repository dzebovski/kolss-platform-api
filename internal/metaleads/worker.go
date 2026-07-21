package metaleads

import (
	"context"
	"sync"
	"time"
)

func (i *Integration) Run(ctx context.Context) {
	if !i.Enabled() {
		i.log().Info("Meta Lead Ads integration disabled")
		return
	}
	if err := i.ensureConnections(ctx); err != nil && ctx.Err() == nil {
		i.log().Error("Meta page connection initialization failed", "error", err)
	}

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		i.runEventLoop(ctx)
	}()
	go func() {
		defer workers.Done()
		i.runSyncLoop(ctx)
	}()
	workers.Wait()
	i.log().Info("Meta Lead Ads integration stopped")
}

func (i *Integration) runEventLoop(ctx context.Context) {
	i.processAvailable(ctx, true)
	i.alertIgnoredEvents(ctx)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-i.wake:
			i.processAvailable(ctx, false)
		case <-ticker.C:
			i.processAvailable(ctx, true)
			i.alertIgnoredEvents(ctx)
		}
	}
}

func (i *Integration) runSyncLoop(ctx context.Context) {
	i.syncConfiguredPages(ctx, false)
	interval := i.Config.ReconciliationInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	activeTicker := time.NewTicker(interval)
	fullTimer := time.NewTimer(time.Until(nextNightlySync(time.Now())))
	staleTicker := time.NewTicker(15 * time.Minute)
	defer activeTicker.Stop()
	defer fullTimer.Stop()
	defer staleTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activeTicker.C:
			i.syncConfiguredPages(ctx, false)
		case <-fullTimer.C:
			i.syncConfiguredPages(ctx, true)
			fullTimer.Reset(time.Until(nextNightlySync(time.Now())))
		case <-staleTicker.C:
			i.checkStaleConnections(ctx)
		}
	}
}

func nextNightlySync(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
