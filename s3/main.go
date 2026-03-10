package s3store

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	storemd "github.com/readmedotmd/store.md"
)

// StoreS3 implements storemd.Store using an S3-compatible backend.
type StoreS3 struct {
	client *s3.Client
	bucket string
	prefix string
}

// New creates a new StoreS3. The prefix is optional and will be prepended to all keys.
func New(client *s3.Client, bucket, prefix string) *StoreS3 {
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
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return "", storemd.NotFoundError
		}
		// Some S3-compatible implementations return a generic error; check the message.
		if strings.Contains(err.Error(), "NoSuchKey") {
			return "", storemd.NotFoundError
		}
		return "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *StoreS3) Set(key, value string) error {
	_, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
		Body:   strings.NewReader(value),
	})
	return err
}

func (s *StoreS3) Delete(key string) error {
	// S3 DeleteObject does not error on missing keys, so check existence first.
	_, err := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return storemd.NotFoundError
		}
		// Some S3-compatible services return a generic error for 404 on HeadObject.
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
			return storemd.NotFoundError
		}
		return err
	}

	_, err = s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	return err
}

func (s *StoreS3) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.fullKey(args.Prefix)),
	}

	if args.StartAfter != "" {
		input.StartAfter = aws.String(s.fullKey(args.StartAfter))
	}

	if args.Limit > 0 {
		input.MaxKeys = aws.Int32(int32(args.Limit))
	}

	out, err := s.client.ListObjectsV2(context.Background(), input)
	if err != nil {
		return nil, err
	}

	result := make([]storemd.KeyValuePair, 0, len(out.Contents))
	for _, obj := range out.Contents {
		key := s.stripPrefix(aws.ToString(obj.Key))
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
