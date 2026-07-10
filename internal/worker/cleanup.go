package worker

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cleanup expires awaiting_upload submissions past expires_at and deletes orphan quarantine objects.
type Cleanup struct {
	Pool   *pgxpool.Pool
	Store  ObjectStore
	Logger *slog.Logger
	Limit  int
}

type expiredUpload struct {
	ID     string
	Bucket string
	Key    string
}

func (c *Cleanup) RunOnce(ctx context.Context) (expiredSubmissions int, deletedObjects int, err error) {
	limit := c.Limit
	if limit <= 0 {
		limit = 50
	}

	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		update public.lead_submissions
		set status = 'expired'
		where id in (
			select id
			from public.lead_submissions
			where status = 'awaiting_upload'
			  and expires_at < now()
			order by expires_at asc
			limit $1
			for update skip locked
		)
		returning id
	`, limit)
	if err != nil {
		return 0, 0, err
	}
	var submissionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		submissionIDs = append(submissionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	expiredSubmissions = len(submissionIDs)
	if expiredSubmissions == 0 {
		return 0, 0, tx.Commit(ctx)
	}

	uploadRows, err := tx.Query(ctx, `
		select id, storage_bucket, storage_path
		from public.lead_submission_uploads
		where submission_id = any($1::uuid[])
		  and status <> 'deleted'
		for update skip locked
	`, submissionIDs)
	if err != nil {
		return expiredSubmissions, 0, err
	}
	var uploads []expiredUpload
	for uploadRows.Next() {
		var u expiredUpload
		if err := uploadRows.Scan(&u.ID, &u.Bucket, &u.Key); err != nil {
			uploadRows.Close()
			return expiredSubmissions, 0, err
		}
		uploads = append(uploads, u)
	}
	uploadRows.Close()
	if err := uploadRows.Err(); err != nil {
		return expiredSubmissions, 0, err
	}

	// Mark deleted in the same transaction; object deletes happen after commit.
	for _, u := range uploads {
		if _, err := tx.Exec(ctx, `
			update public.lead_submission_uploads
			set status = 'deleted'
			where id = $1::uuid
		`, u.ID); err != nil {
			return expiredSubmissions, 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return expiredSubmissions, 0, err
	}

	for _, u := range uploads {
		if delErr := c.Store.Delete(ctx, u.Bucket, u.Key); delErr != nil {
			c.log().Warn("delete quarantine object failed", "bucket", u.Bucket, "key", u.Key, "error", delErr)
			continue
		}
		deletedObjects++
	}
	return expiredSubmissions, deletedObjects, nil
}

func (c *Cleanup) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
