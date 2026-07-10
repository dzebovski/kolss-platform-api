package worker

import (
	"context"
	"io"
)

// ObjectStore is the narrow storage surface the worker needs.
// Adapts to internal/storage once that package finishes converging.
type ObjectStore interface {
	Head(ctx context.Context, bucket, key string) (size int64, contentType, etag string, err error)
	GetStream(ctx context.Context, bucket, key string) (body io.ReadCloser, size int64, contentType string, err error)
	Delete(ctx context.Context, bucket, key string) error
}
