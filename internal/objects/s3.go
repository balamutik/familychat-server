package objects

import (
	"context"
	"errors"
	"io"
	"strings"

	"familychat/server/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Store struct {
	client *s3.Client
	bucket string
}

type Metadata struct {
	Size        int64
	ContentType string
}
type Object struct {
	Body         io.ReadCloser
	Metadata     Metadata
	ContentRange string
}

func New(c config.Config) *Store {
	opts := s3.Options{
		Region:       c.S3Region,
		Credentials:  credentials.NewStaticCredentialsProvider(c.S3AccessKey, c.S3SecretKey, ""),
		UsePathStyle: c.S3UsePathStyle,
	}
	if c.S3Endpoint != "" {
		opts.BaseEndpoint = aws.String(c.S3Endpoint)
	}
	return &Store{client: s3.New(opts), bucket: c.S3Bucket}
}

func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.client == nil || s.bucket == "" {
		return errors.New("S3 unavailable")
	}
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return errors.New("S3 unavailable")
	}
	return nil
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if key == "" || size < 0 {
		return errors.New("invalid object")
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType)}, s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
	return err
}

func (s *Store) Head(ctx context.Context, key string) (Metadata, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}, nil
}

func (s *Store) Get(ctx context.Context, key, byteRange string) (Object, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if byteRange != "" {
		if !strings.HasPrefix(byteRange, "bytes=") || len(byteRange) > 100 {
			return Object{}, errors.New("invalid byte range")
		}
		in.Range = aws.String(byteRange)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return Object{}, err
	}
	return Object{Body: out.Body, Metadata: Metadata{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}, ContentRange: aws.ToString(out.ContentRange)}, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}
