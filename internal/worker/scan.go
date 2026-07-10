package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MalwareScanner is a stub hook for future AV integration.
type MalwareScanner interface {
	Scan(ctx context.Context, filename string, contentType string, r io.Reader) (clean bool, detail string, err error)
}

// NoopMalwareScanner always reports clean after content checks have already passed.
type NoopMalwareScanner struct{}

func (NoopMalwareScanner) Scan(_ context.Context, _ string, _ string, r io.Reader) (bool, string, error) {
	if r != nil {
		_, _ = io.Copy(io.Discard, r)
	}
	return true, "noop", nil
}

// Scanner scans pending_scan uploads/attachments: SHA-256 + magic-byte MIME check + malware stub.
type Scanner struct {
	Pool    *pgxpool.Pool
	Store   ObjectStore
	Malware MalwareScanner
	Logger  *slog.Logger
	Limit   int
}

type scanTarget struct {
	Kind         string // "upload" | "attachment"
	ID           string
	Bucket       string
	Key          string
	Filename     string
	DeclaredMIME string
}

func (s *Scanner) RunOnce(ctx context.Context) (processed int, err error) {
	if s.Malware == nil {
		s.Malware = NoopMalwareScanner{}
	}
	limit := s.Limit
	if limit <= 0 {
		limit = 10
	}

	for i := 0; i < limit; i++ {
		did, err := s.claimAndScanUpload(ctx)
		if err != nil {
			return processed, err
		}
		if !did {
			break
		}
		processed++
	}
	remaining := limit - processed
	for i := 0; i < remaining; i++ {
		did, err := s.claimAndScanAttachment(ctx)
		if err != nil {
			return processed, err
		}
		if !did {
			break
		}
		processed++
	}
	return processed, nil
}

func (s *Scanner) claimAndScanUpload(ctx context.Context) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var t scanTarget
	t.Kind = "upload"
	err = tx.QueryRow(ctx, `
		select id, storage_bucket, storage_path, original_filename, declared_content_type
		from public.lead_submission_uploads
		where status = 'pending_scan'
		order by created_at asc
		limit 1
		for update skip locked
	`).Scan(&t.ID, &t.Bucket, &t.Key, &t.Filename, &t.DeclaredMIME)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	scanErr := s.scanLocked(ctx, tx, t)
	if scanErr != nil {
		s.log().Error("scan upload failed", "id", t.ID, "error", scanErr)
	}
	// Always commit so ready/blocked updates from scanLocked persist.
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Scanner) claimAndScanAttachment(ctx context.Context) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var t scanTarget
	t.Kind = "attachment"
	err = tx.QueryRow(ctx, `
		select id, storage_bucket, storage_path, file_name, mime_type
		from public.lead_attachments
		where status = 'pending_scan'
		order by created_at asc
		limit 1
		for update skip locked
	`).Scan(&t.ID, &t.Bucket, &t.Key, &t.Filename, &t.DeclaredMIME)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	scanErr := s.scanLocked(ctx, tx, t)
	if scanErr != nil {
		s.log().Error("scan attachment failed", "id", t.ID, "error", scanErr)
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Scanner) scanLocked(ctx context.Context, tx pgx.Tx, t scanTarget) error {
	body, sizeHint, _, err := s.Store.GetStream(ctx, t.Bucket, t.Key)
	if err != nil {
		_ = s.markBlockedTx(ctx, tx, t, "", 0)
		// Commit blocked state even when get fails.
		return fmt.Errorf("get object: %w", err)
	}
	defer body.Close()

	hasher := sha256.New()
	head := make([]byte, 0, 512)
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			total += int64(n)
			_, _ = hasher.Write(buf[:n])
			if len(head) < 512 {
				need := 512 - len(head)
				if need > n {
					need = n
				}
				head = append(head, buf[:need]...)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = s.markBlockedTx(ctx, tx, t, "", total)
			return fmt.Errorf("read object: %w", readErr)
		}
	}
	if sizeHint > 0 && total == 0 {
		total = sizeHint
	}

	detected, ok := DetectContentType(head, t.DeclaredMIME)
	sha := hex.EncodeToString(hasher.Sum(nil))
	if !ok {
		return s.markBlockedTx(ctx, tx, t, sha, total)
	}

	clean, detail, err := s.Malware.Scan(ctx, t.Filename, detected, bytes.NewReader(head))
	if err != nil {
		_ = s.markBlockedTx(ctx, tx, t, sha, total)
		return fmt.Errorf("malware scan error: %w", err)
	}
	if !clean {
		s.log().Warn("blocking object", "kind", t.Kind, "id", t.ID, "reason", detail)
		return s.markBlockedTx(ctx, tx, t, sha, total)
	}
	return s.markReadyTx(ctx, tx, t, sha, total, detected)
}

func (s *Scanner) markReadyTx(ctx context.Context, tx pgx.Tx, t scanTarget, sha string, size int64, contentType string) error {
	switch t.Kind {
	case "upload":
		_, err := tx.Exec(ctx, `
			update public.lead_submission_uploads
			set status = 'ready',
			    sha256 = $2,
			    actual_size_bytes = $3,
			    actual_content_type = $4,
			    scanned_at = now()
			where id = $1::uuid
		`, t.ID, sha, size, contentType)
		return err
	case "attachment":
		_, err := tx.Exec(ctx, `
			update public.lead_attachments
			set status = 'ready',
			    sha256 = $2,
			    size_bytes = $3,
			    mime_type = $4
			where id = $1::uuid
		`, t.ID, sha, size, contentType)
		return err
	default:
		return fmt.Errorf("unknown scan target kind %q", t.Kind)
	}
}

func (s *Scanner) markBlockedTx(ctx context.Context, tx pgx.Tx, t scanTarget, sha string, size int64) error {
	s.log().Warn("blocking object", "kind", t.Kind, "id", t.ID)
	switch t.Kind {
	case "upload":
		_, err := tx.Exec(ctx, `
			update public.lead_submission_uploads
			set status = 'blocked',
			    sha256 = nullif($2, ''),
			    actual_size_bytes = nullif($3, 0),
			    scanned_at = now()
			where id = $1::uuid
		`, t.ID, sha, size)
		return err
	case "attachment":
		_, err := tx.Exec(ctx, `
			update public.lead_attachments
			set status = 'blocked',
			    sha256 = nullif($2, '')
			where id = $1::uuid
		`, t.ID, sha)
		return err
	default:
		return fmt.Errorf("unknown scan target kind %q", t.Kind)
	}
}

func (s *Scanner) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// DetectContentType returns a canonical MIME from magic bytes (and declared type for text).
func DetectContentType(head []byte, declared string) (string, bool) {
	declared = strings.ToLower(strings.TrimSpace(declared))
	switch {
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "application/pdf", true
	case len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff:
		return "image/jpeg", true
	case bytes.HasPrefix(head, []byte{0x89, 0x50, 0x4e, 0x47}):
		return "image/png", true
	case isWebP(head):
		return "image/webp", true
	case isMostlyText(head):
		if declared == "text/csv" || strings.Contains(declared, "csv") {
			return "text/csv", true
		}
		return "text/plain", true
	default:
		return "", false
	}
}

func isWebP(head []byte) bool {
	return len(head) >= 12 &&
		bytes.Equal(head[0:4], []byte("RIFF")) &&
		bytes.Equal(head[8:12], []byte("WEBP"))
}

func isMostlyText(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	sample := head
	if len(sample) > 512 {
		sample = sample[:512]
	}
	if !utf8.Valid(sample) {
		nonPrintable := 0
		for _, b := range sample {
			if b < 0x09 || (b > 0x0d && b < 0x20) {
				nonPrintable++
			}
		}
		return nonPrintable*10 <= len(sample)
	}
	nonPrintable := 0
	runes := 0
	for _, r := range string(sample) {
		runes++
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if !unicode.IsPrint(r) {
			nonPrintable++
		}
	}
	if runes == 0 {
		return false
	}
	return nonPrintable*10 <= runes
}
