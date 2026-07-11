package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	ErrNotFound      = errors.New("object not found")
	ErrNotConfigured = errors.New("object storage not configured")
)

type ObjectInfo struct {
	Bucket      string
	Key         string
	ContentType string
	SizeBytes   int64
	ETag        string
}

type PresignPutInput struct {
	Bucket      string
	Key         string
	ContentType string
	Expires     time.Duration
}

type PresignPutResult struct {
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

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

// ObjectStorage is the quarantine object store used by API/worker.
type ObjectStorage interface {
	PresignPut(ctx context.Context, in PresignPutInput) (PresignPutResult, error)
	PresignGet(ctx context.Context, in PresignGetInput) (PresignGetResult, error)
	Head(ctx context.Context, bucket, key string) (ObjectInfo, error)
	GetStream(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error)
	Delete(ctx context.Context, bucket, key string) error
}

type ObjectKeyParts struct {
	SiteCode     string
	SubmissionID string
	FileID       string
}

func ObjectKey(p ObjectKeyParts) string {
	return fmt.Sprintf("%s/%s/%s", p.SiteCode, p.SubmissionID, p.FileID)
}

// Memory is an in-process fake for tests and local empty-file flows.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	baseURL string
}

type memoryObject struct {
	info ObjectInfo
	data []byte
}

func NewMemory(baseURL string) *Memory {
	if baseURL == "" {
		baseURL = "https://storage.test/presign"
	}
	return &Memory{
		objects: make(map[string]memoryObject),
		baseURL: baseURL,
	}
}

func (m *Memory) key(bucket, objectKey string) string {
	return bucket + "/" + objectKey
}

func (m *Memory) PresignPut(_ context.Context, in PresignPutInput) (PresignPutResult, error) {
	expires := in.Expires
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	expiresAt := time.Now().UTC().Add(expires)
	url := fmt.Sprintf("%s/%s/%s?expires=%d", m.baseURL, in.Bucket, in.Key, expiresAt.Unix())
	return PresignPutResult{
		URL: url,
		Headers: map[string]string{
			"content-type": in.ContentType,
		},
		ExpiresAt: expiresAt,
	}, nil
}

func (m *Memory) PresignGet(_ context.Context, in PresignGetInput) (PresignGetResult, error) {
	expires := in.Expires
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	expiresAt := time.Now().UTC().Add(expires)
	return PresignGetResult{
		URL:       fmt.Sprintf("%s/%s/%s?download=1&expires=%d", m.baseURL, in.Bucket, in.Key, expiresAt.Unix()),
		ExpiresAt: expiresAt,
	}, nil
}

func (m *Memory) PutForTest(bucket, key, contentType string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[m.key(bucket, key)] = memoryObject{
		info: ObjectInfo{
			Bucket:      bucket,
			Key:         key,
			ContentType: contentType,
			SizeBytes:   int64(len(data)),
			ETag:        `"memory-etag"`,
		},
		data: append([]byte(nil), data...),
	}
}

func (m *Memory) Head(_ context.Context, bucket, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[m.key(bucket, key)]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return obj.info, nil
}

func (m *Memory) GetStream(_ context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[m.key(bucket, key)]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(io.NewSectionReader(
		&bytesReaderAt{b: obj.data}, 0, int64(len(obj.data)),
	)), obj.info, nil
}

func (m *Memory) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, m.key(bucket, key))
	return nil
}

type bytesReaderAt struct{ b []byte }

func (r *bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// NilStorage rejects all operations. Allowed only when files are empty and BOTCHECK_DISABLED.
type NilStorage struct{}

func (NilStorage) PresignPut(context.Context, PresignPutInput) (PresignPutResult, error) {
	return PresignPutResult{}, ErrNotConfigured
}
func (NilStorage) PresignGet(context.Context, PresignGetInput) (PresignGetResult, error) {
	return PresignGetResult{}, ErrNotConfigured
}
func (NilStorage) Head(context.Context, string, string) (ObjectInfo, error) {
	return ObjectInfo{}, ErrNotConfigured
}
func (NilStorage) GetStream(context.Context, string, string) (io.ReadCloser, ObjectInfo, error) {
	return nil, ObjectInfo{}, ErrNotConfigured
}
func (NilStorage) Delete(context.Context, string, string) error {
	return ErrNotConfigured
}
