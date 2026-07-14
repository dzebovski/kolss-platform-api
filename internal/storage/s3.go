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
