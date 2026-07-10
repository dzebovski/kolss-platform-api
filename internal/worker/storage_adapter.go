package worker

import (
	"context"
	"io"

	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

// StorageAdapter adapts internal/storage.ObjectStorage to the worker ObjectStore surface.
type StorageAdapter struct {
	Inner storage.ObjectStorage
}

func (a StorageAdapter) Head(ctx context.Context, bucket, key string) (int64, string, string, error) {
	info, err := a.Inner.Head(ctx, bucket, key)
	if err != nil {
		return 0, "", "", err
	}
	return info.SizeBytes, info.ContentType, info.ETag, nil
}

func (a StorageAdapter) GetStream(ctx context.Context, bucket, key string) (io.ReadCloser, int64, string, error) {
	body, info, err := a.Inner.GetStream(ctx, bucket, key)
	if err != nil {
		return nil, 0, "", err
	}
	return body, info.SizeBytes, info.ContentType, nil
}

func (a StorageAdapter) Delete(ctx context.Context, bucket, key string) error {
	return a.Inner.Delete(ctx, bucket, key)
}
