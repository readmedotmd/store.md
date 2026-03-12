package s3store

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	storemd "github.com/readmedotmd/store.md"
)

const defaultTimeout = 30 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// StoreS3 implements storemd.Store using an S3-compatible backend.
type StoreS3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// New creates a new StoreS3. The prefix is optional and will be prepended to all keys.
func New(client *minio.Client, bucket, prefix string) *StoreS3 {
	return &StoreS3{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}
}

func (s *StoreS3) fullKey(key string) string {
	return s.prefix + key
}

func (s *StoreS3) stripPrefix(key string) string {
	return strings.TrimPrefix(key, s.prefix)
}

func (s *StoreS3) Get(ctx context.Context, key string) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	obj, err := s.client.GetObject(ctx, s.bucket, s.fullKey(key), minio.GetObjectOptions{})
	if err != nil {
		return "", err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		var errResp minio.ErrorResponse
		if errors.As(err, &errResp) && errResp.Code == "NoSuchKey" {
			return "", storemd.NotFoundError
		}
		if strings.Contains(err.Error(), "NoSuchKey") {
			return "", storemd.NotFoundError
		}
		return "", err
	}
	return string(data), nil
}

func (s *StoreS3) Set(ctx context.Context, key, value string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.client.PutObject(ctx, s.bucket, s.fullKey(key), strings.NewReader(value), int64(len(value)), minio.PutObjectOptions{})
	return err
}

func (s *StoreS3) Delete(ctx context.Context, key string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.client.StatObject(ctx, s.bucket, s.fullKey(key), minio.StatObjectOptions{})
	if err != nil {
		var errResp minio.ErrorResponse
		if errors.As(err, &errResp) && (errResp.Code == "NoSuchKey" || errResp.Code == "NotFound") {
			return storemd.NotFoundError
		}
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return storemd.NotFoundError
		}
		return err
	}

	return s.client.RemoveObject(ctx, s.bucket, s.fullKey(key), minio.RemoveObjectOptions{})
}

func (s *StoreS3) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	opts := minio.ListObjectsOptions{
		Prefix:    s.fullKey(args.Prefix),
		Recursive: true,
	}
	if args.StartAfter != "" {
		opts.StartAfter = s.fullKey(args.StartAfter)
	}

	result := make([]storemd.KeyValuePair, 0)
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if args.Limit > 0 && len(result) >= args.Limit {
			break
		}
		key := s.stripPrefix(obj.Key)
		obj, err := s.client.GetObject(ctx, s.bucket, s.fullKey(key), minio.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(obj)
		obj.Close()
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, storemd.KeyValuePair{
			Key:   key,
			Value: string(data),
		})
	}

	return result, nil
}
