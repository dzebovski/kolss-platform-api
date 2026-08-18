// Command dailyreport-preview composes and prints the morning digest for one
// office and one office-local report date to stdout, without sending
// anything to Telegram or Slack. It reuses the exact same cohort counts
// (internal/leadcohorts) and message templates (internal/dailyreport) the
// real 09:00 schedule uses, so its output is what the office would actually
// receive — it is the only way to inspect that without waiting for the
// schedule to fire.
//
// Usage:
//
//	go run ./cmd/dailyreport-preview -office=warsaw -date=2026-08-17
//
// -date defaults to today in the office's timezone when omitted. It needs
// only DATABASE_URL (required) and optionally CRM_SITE_URL_PUBLIC /
// SITE_URL_PUBLIC in the environment — see internal/config.LoadPreview.
// Unlike cmd/api, it does NOT need Supabase auth or Telegram/Slack delivery
// secrets: it only runs read-only SELECTs and prints text.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/config"
	"github.com/dzebovski/kolss-platform-api/internal/dailyreport"
	"github.com/dzebovski/kolss-platform-api/internal/leadcohorts"
	"github.com/dzebovski/kolss-platform-api/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dailyreport-preview:", err)
		os.Exit(1)
	}
}

func run() error {
	officeFlag := flag.String("office", "", "office code, e.g. kyiv or warsaw")
	dateFlag := flag.String("date", "", "office-local report date, YYYY-MM-DD (defaults to today in the office timezone)")
	timeout := flag.Duration("timeout", 30*time.Second, "database operation timeout")
	flag.Parse()

	officeCode := strings.TrimSpace(*officeFlag)
	if officeCode == "" {
		return fmt.Errorf("-office is required (kyiv or warsaw)")
	}
	timezone, ok := dailyreport.OfficeTimezone(officeCode)
	if !ok {
		return fmt.Errorf("unknown office code %q", officeCode)
	}
	channel, _ := dailyreport.OfficeChannel(officeCode)
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", timezone, err)
	}

	localDate := strings.TrimSpace(*dateFlag)
	if localDate == "" {
		localDate = time.Now().In(loc).Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", localDate); err != nil {
		return fmt.Errorf("-date must be YYYY-MM-DD: %w", err)
	}

	cfg, err := config.LoadPreview()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	counts, err := leadcohorts.FetchCounts(ctx, pool, leadcohorts.Params{
		OfficeCode: officeCode,
		LocalDate:  localDate,
		Timezone:   timezone,
	})
	if err != nil {
		return fmt.Errorf("fetch cohort counts: %w", err)
	}

	scheduler := &dailyreport.Scheduler{CRMSiteURLPublic: cfg.CRMSiteURLPublic}
	var message string
	if channel == "slack" {
		message = scheduler.FormatSlackMessage(counts, officeCode, localDate)
	} else {
		message = scheduler.FormatTelegramMessage(counts, officeCode, localDate)
	}

	fmt.Printf("office:   %s\n", officeCode)
	fmt.Printf("channel:  %s\n", channel)
	fmt.Printf("date:     %s\n", localDate)
	fmt.Printf("timezone: %s\n\n", timezone)
	fmt.Println("counts:")
	fmt.Printf("  new_leads:                     %d\n", counts.NewLeads)
	fmt.Printf("  no_answer_or_callback_undated: %d\n", counts.NoAnswerOrCallbackUndated)
	fmt.Printf("  callback_due_today:            %d\n", counts.CallbackDueToday)
	fmt.Printf("  visits_due_today:              %d\n", counts.VisitsDueToday)
	fmt.Printf("  reminder_due_today:            %d\n", counts.ReminderDueToday)
	fmt.Printf("  overdue:                       %d\n", counts.Overdue)
	fmt.Println()
	fmt.Println("message:")
	fmt.Println(message)

	return nil
}
