package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Config struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
}

type S3 struct {
	client    *s3.Client
	presigner *s3.PresignClient
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("incomplete S3 config")
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: true,
	})
	return &S3{
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (s *S3) PresignGet(ctx context.Context, in PresignGetInput) (PresignGetResult, error) {
	expires := in.Expires
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	input := &s3.GetObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	}
	if in.Filename != "" {
		input.ResponseContentDisposition = aws.String(fmt.Sprintf("attachment; filename=%q", in.Filename))
	}
	out, err := s.presigner.PresignGetObject(ctx, input, s3.WithPresignExpires(expires))
	if err != nil {
		return PresignGetResult{}, err
	}
	return PresignGetResult{URL: out.URL, ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

// PresignPut signs a direct browser-to-storage PUT (task W11). Pinning ContentType in the
// signed input forces the uploader to send that exact Content-Type header, so the API's
// server-derived content type (from the file extension, never trusted from the client as-is)
// is what actually gets stored.
func (s *S3) PresignPut(ctx context.Context, in PresignPutInput) (PresignPutResult, error) {
	expires := in.Expires
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	}
	headers := map[string]string{}
	if in.ContentType != "" {
		input.ContentType = aws.String(in.ContentType)
		headers["Content-Type"] = in.ContentType
	}
	out, err := s.presigner.PresignPutObject(ctx, input, s3.WithPresignExpires(expires))
	if err != nil {
		return PresignPutResult{}, err
	}
	method := out.Method
	if method == "" {
		method = "PUT"
	}
	return PresignPutResult{URL: out.URL, Method: method, Headers: headers, ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

// HeadObject confirms an upload (task W11) exists and reads its actual size/content type. Any
// error (including "not found") is reported as-is; the caller treats it uniformly as "the file
// was not uploaded yet".
func (s *S3) HeadObject(ctx context.Context, in HeadObjectInput) (HeadObjectResult, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	})
	if err != nil {
		return HeadObjectResult{}, err
	}
	result := HeadObjectResult{}
	if out.ContentLength != nil {
		result.SizeBytes = *out.ContentLength
	}
	if out.ContentType != nil {
		result.ContentType = *out.ContentType
	}
	return result, nil
}
