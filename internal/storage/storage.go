package storage

import (
	"context"
	"errors"
	"time"
)

var ErrNotConfigured = errors.New("object storage not configured")

type PresignGetInput struct {
	Bucket   string
	Key      string
	Filename string
	Expires  time.Duration
}

type PresignGetResult struct {
	URL       string
	ExpiresAt time.Time
}

// ObjectStorage exposes only the historical CRM attachment download operation.
type ObjectStorage interface {
	PresignGet(ctx context.Context, in PresignGetInput) (PresignGetResult, error)
}

// NilStorage rejects historical attachment downloads when S3 is not configured.
type NilStorage struct{}

func (NilStorage) PresignGet(context.Context, PresignGetInput) (PresignGetResult, error) {
	return PresignGetResult{}, ErrNotConfigured
}
