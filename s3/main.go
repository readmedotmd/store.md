package s3store

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"

	storemd "github.com/readmedotmd/store.md"
)

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

func (s *StoreS3) Get(key string) (string, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, s.fullKey(key), minio.GetObjectOptions{})
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

func (s *StoreS3) Set(key, value string) error {
	_, err := s.client.PutObject(context.Background(), s.bucket, s.fullKey(key), strings.NewReader(value), int64(len(value)), minio.PutObjectOptions{})
	return err
}

func (s *StoreS3) Delete(key string) error {
	_, err := s.client.StatObject(context.Background(), s.bucket, s.fullKey(key), minio.StatObjectOptions{})
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

	return s.client.RemoveObject(context.Background(), s.bucket, s.fullKey(key), minio.RemoveObjectOptions{})
}

func (s *StoreS3) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	opts := minio.ListObjectsOptions{
		Prefix:    s.fullKey(args.Prefix),
		Recursive: true,
	}
	if args.StartAfter != "" {
		opts.StartAfter = s.fullKey(args.StartAfter)
	}

	ctx := context.Background()
	result := make([]storemd.KeyValuePair, 0)
	for obj := range s.client.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if args.Limit > 0 && len(result) >= args.Limit {
			break
		}
		key := s.stripPrefix(obj.Key)
		val, err := s.Get(key)
		if err != nil {
			return nil, err
		}
		result = append(result, storemd.KeyValuePair{
			Key:   key,
			Value: val,
		})
	}

	return result, nil
}
