package submissions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dzebovski/kolss-platform-api/internal/leads"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

func TestCreateCommitsLeadSubmissionAndTelegramBeforeWake(t *testing.T) {
	leadID := uuid.New()
	tx := &fakeTx{}
	tx.queryRow = func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "insert into public.leads"):
			return scanRow(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = leadID
				return nil
			})
		case strings.Contains(sql, "insert into public.lead_submissions"):
			return scanRow(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = args[0].(uuid.UUID)
				return nil
			})
		default:
			t.Fatalf("unexpected transaction query: %s", sql)
			return scanRow(func(...any) error { return errors.New("unexpected query") })
		}
	}
	tx.exec = func(sql string, _ ...any) (pgconn.CommandTag, error) {
		if !strings.Contains(sql, "'telegram'::public.notification_channel") || strings.Contains(sql, "slack") {
			t.Fatalf("unexpected outbox query: %s", sql)
		}
		tx.notificationInserts++
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	db := &fakeDatabase{tx: tx, queryRow: func(string, ...any) pgx.Row {
		return scanRow(func(...any) error { return pgx.ErrNoRows })
	}}
	waker := &countingWaker{}
	service := NewService(db, fixedSiteRepository(), notifications.Outbox{TelegramChatIDWarsaw: "-1001"}, waker)

	result, err := service.Create(context.Background(), "kolss-pl", validSubmission())
	if err != nil {
		t.Fatal(err)
	}
	if result.LeadID != leadID || result.Status != "accepted" || result.Duplicate {
		t.Fatalf("result = %#v", result)
	}
	if !tx.committed || tx.notificationInserts != 1 || waker.count != 1 {
		t.Fatalf("commit=%v notifications=%d wakes=%d", tx.committed, tx.notificationInserts, waker.count)
	}
}

func TestCreateRollsBackAndDoesNotWakeWhenOutboxInsertFails(t *testing.T) {
	tx := successfulCreateTx(uuid.New())
	tx.exec = func(string, ...any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, errors.New("outbox unavailable")
	}
	db := &fakeDatabase{tx: tx, queryRow: func(string, ...any) pgx.Row {
		return scanRow(func(...any) error { return pgx.ErrNoRows })
	}}
	waker := &countingWaker{}
	service := NewService(db, fixedSiteRepository(), notifications.Outbox{TelegramChatIDWarsaw: "-1001"}, waker)

	if _, err := service.Create(context.Background(), "kolss-pl", validSubmission()); err == nil {
		t.Fatal("expected outbox error")
	}
	if tx.committed || !tx.rolledBack || waker.count != 0 {
		t.Fatalf("commit=%v rollback=%v wakes=%d", tx.committed, tx.rolledBack, waker.count)
	}
}

func TestCreateIdempotencyReplayDoesNotBeginOrWake(t *testing.T) {
	submissionID, leadID := uuid.New(), uuid.New()
	db := &fakeDatabase{queryRow: func(string, ...any) pgx.Row {
		return scanRow(func(dest ...any) error {
			*dest[0].(*uuid.UUID) = submissionID
			*dest[1].(**uuid.UUID) = &leadID
			*dest[2].(*string) = "accepted"
			return nil
		})
	}}
	waker := &countingWaker{}
	service := NewService(db, fixedSiteRepository(), notifications.Outbox{TelegramChatIDWarsaw: "-1001"}, waker)

	result, err := service.Create(context.Background(), "kolss-pl", validSubmission())
	if err != nil {
		t.Fatal(err)
	}
	if result.SubmissionID != submissionID || result.LeadID != leadID || !result.Duplicate {
		t.Fatalf("result = %#v", result)
	}
	if db.beginCalls != 0 || waker.count != 0 {
		t.Fatalf("begins=%d wakes=%d", db.beginCalls, waker.count)
	}
}

type fakeDatabase struct {
	tx         pgx.Tx
	queryRow   func(sql string, args ...any) pgx.Row
	beginCalls int
}

func (db *fakeDatabase) Ping(context.Context) error { return nil }

func (db *fakeDatabase) Begin(context.Context) (pgx.Tx, error) {
	db.beginCalls++
	return db.tx, nil
}

func (db *fakeDatabase) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return db.queryRow(sql, args...)
}

type fakeTx struct {
	pgx.Tx
	queryRow            func(sql string, args ...any) pgx.Row
	exec                func(sql string, args ...any) (pgconn.CommandTag, error)
	committed           bool
	rolledBack          bool
	notificationInserts int
}

func (tx *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return tx.queryRow(sql, args...)
}

func (tx *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return tx.exec(sql, args...)
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.committed = true
	return nil
}

func (tx *fakeTx) Rollback(context.Context) error {
	if tx.committed {
		return pgx.ErrTxClosed
	}
	tx.rolledBack = true
	return nil
}

type scanRow func(dest ...any) error

func (row scanRow) Scan(dest ...any) error { return row(dest...) }

type fixedSites struct{ site leads.Site }

func fixedSiteRepository() fixedSites {
	return fixedSites{site: leads.Site{
		Code:                 "kolss-pl",
		OfficeID:             uuid.New(),
		OfficeCode:           "warsaw",
		PrivacyPolicyVersion: "pl-v1",
		IsActive:             true,
	}}
}

func (repository fixedSites) GetActiveSite(context.Context, string) (leads.Site, error) {
	return repository.site, nil
}

type countingWaker struct{ count int }

func (w *countingWaker) Wake() { w.count++ }

func validSubmission() validation.ValidatedLeadSubmission {
	return validation.ValidatedLeadSubmission{
		IdempotencyKey:       uuid.New(),
		Name:                 "Anna Kowalska",
		Phone:                "+48123456789",
		PrivacyPolicyVersion: "pl-v1",
	}
}

func successfulCreateTx(leadID uuid.UUID) *fakeTx {
	return &fakeTx{queryRow: func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "insert into public.leads"):
			return scanRow(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = leadID
				return nil
			})
		case strings.Contains(sql, "insert into public.lead_submissions"):
			return scanRow(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = args[0].(uuid.UUID)
				return nil
			})
		default:
			return scanRow(func(...any) error { return errors.New("unexpected query") })
		}
	}}
}
