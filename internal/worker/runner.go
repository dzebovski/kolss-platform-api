package worker

import (
	"context"
	"log/slog"
	"time"
)

// Runner ticks cleanup, scan, and notify loops until ctx is cancelled.
type Runner struct {
	Cleanup *Cleanup
	Scanner *Scanner
	Notify  *Notifier
	Logger  *slog.Logger

	CleanupInterval time.Duration
	ScanInterval    time.Duration
	NotifyInterval  time.Duration
}

func (r *Runner) Start(ctx context.Context) {
	if r.Cleanup != nil {
		go r.loop(ctx, "cleanup", r.CleanupInterval, func(c context.Context) error {
			expired, deleted, err := r.Cleanup.RunOnce(c)
			if err != nil {
				return err
			}
			if expired > 0 || deleted > 0 {
				r.log().Info("cleanup tick", "expired_submissions", expired, "deleted_objects", deleted)
			}
			return nil
		})
	}
	if r.Scanner != nil {
		go r.loop(ctx, "scan", r.ScanInterval, func(c context.Context) error {
			n, err := r.Scanner.RunOnce(c)
			if err != nil {
				return err
			}
			if n > 0 {
				r.log().Info("scan tick", "processed", n)
			}
			return nil
		})
	}
	if r.Notify != nil {
		go r.loop(ctx, "notify", r.NotifyInterval, func(c context.Context) error {
			sent, failed, err := r.Notify.RunOnce(c)
			if err != nil {
				return err
			}
			if sent > 0 || failed > 0 {
				r.log().Info("notify tick", "sent", sent, "failed", failed)
			}
			return nil
		})
	}
}

func (r *Runner) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once immediately so cold start does not wait a full interval.
	if err := fn(ctx); err != nil && ctx.Err() == nil {
		r.log().Error(name+" failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			r.log().Info(name + " stopped")
			return
		case <-t.C:
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				r.log().Error(name+" failed", "error", err)
			}
		}
	}
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
