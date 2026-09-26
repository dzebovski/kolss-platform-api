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

// PresignPutInput asks for a direct-to-storage upload URL (task W11): the browser PUTs the file
// bytes straight to object storage, never through the API (BODY_LIMIT_BYTES is 64 KB and the API
// is a single container — a 25 MB file must not be streamed through it).
type PresignPutInput struct {
	Bucket      string
	Key         string
	ContentType string
	Expires     time.Duration
}

type PresignPutResult struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// HeadObjectInput/Result let the API confirm an upload (task W11) by checking the object exists
// and reading its actual size/content type, without downloading it.
type HeadObjectInput struct {
	Bucket string
	Key    string
}

type HeadObjectResult struct {
	SizeBytes   int64
	ContentType string
}

// ObjectStorage exposes the CRM attachment download operation (historical) and the lead
// document upload operations (task W11).
type ObjectStorage interface {
	PresignGet(ctx context.Context, in PresignGetInput) (PresignGetResult, error)
	PresignPut(ctx context.Context, in PresignPutInput) (PresignPutResult, error)
	HeadObject(ctx context.Context, in HeadObjectInput) (HeadObjectResult, error)
}

// NilStorage rejects every object storage operation when S3 is not configured.
type NilStorage struct{}

func (NilStorage) PresignGet(context.Context, PresignGetInput) (PresignGetResult, error) {
	return PresignGetResult{}, ErrNotConfigured
}

func (NilStorage) PresignPut(context.Context, PresignPutInput) (PresignPutResult, error) {
	return PresignPutResult{}, ErrNotConfigured
}

func (NilStorage) HeadObject(context.Context, HeadObjectInput) (HeadObjectResult, error) {
	return HeadObjectResult{}, ErrNotConfigured
}
