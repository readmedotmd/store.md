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

// SetIfNotExists writes the key only if it does not already exist.
// This uses S3 versioning: PutObject unconditionally, then check if our version
// is the oldest. If not, we lost the race — delete our version and return false.
// Requires bucket versioning to be enabled.
func (s *StoreS3) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	fullKey := s.fullKey(key)

	// Put the object unconditionally.
	info, err := s.client.PutObject(ctx, s.bucket, fullKey, strings.NewReader(value), int64(len(value)), minio.PutObjectOptions{})
	if err != nil {
		return false, err
	}
	ourVersionID := info.VersionID

	// List versions to see if ours is the oldest (i.e., the winner).
	won, err := s.isOldestVersion(ctx, fullKey, ourVersionID)
	if err != nil {
		return false, err
	}

	if !won {
		// We lost the race. Clean up our version.
		s.client.RemoveObject(ctx, s.bucket, fullKey, minio.RemoveObjectOptions{
			VersionID: ourVersionID,
		})
		return false, nil
	}

	// Safety net: sleep and re-verify to account for S3 timestamp granularity.
	time.Sleep(1500 * time.Millisecond)

	won, err = s.isOldestVersion(ctx, fullKey, ourVersionID)
	if err != nil {
		return false, err
	}
	if !won {
		s.client.RemoveObject(ctx, s.bucket, fullKey, minio.RemoveObjectOptions{
			VersionID: ourVersionID,
		})
		return false, nil
	}

	// Clean up any newer versions that lost the race but weren't cleaned up.
	s.cleanLosingVersions(ctx, fullKey, ourVersionID)

	return true, nil
}

// isOldestVersion checks whether the given versionID is the oldest version of the object.
func (s *StoreS3) isOldestVersion(ctx context.Context, key, versionID string) (bool, error) {
	versions := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:       key,
		WithVersions: true,
	})

	var oldest string
	for v := range versions {
		if v.Err != nil {
			return false, v.Err
		}
		if v.Key != key {
			continue
		}
		// ListObjects with versions returns newest first; track last seen as oldest.
		oldest = v.VersionID
	}
	return oldest == versionID, nil
}

// cleanLosingVersions removes all versions of the key except the winner.
func (s *StoreS3) cleanLosingVersions(ctx context.Context, key, winnerVersionID string) {
	versions := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:       key,
		WithVersions: true,
	})
	for v := range versions {
		if v.Err != nil || v.Key != key {
			continue
		}
		if v.VersionID != winnerVersionID {
			s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{
				VersionID: v.VersionID,
			})
		}
	}
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

// Close is a no-op. The caller owns the minio.Client lifecycle.
func (s *StoreS3) Close() error { return nil }

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
