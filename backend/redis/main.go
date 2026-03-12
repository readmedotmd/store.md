package redisstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/redis/go-redis/v9"
)

const defaultTimeout = 30 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// StoreRedis implements storemd.StoreRedis using Redis.
type StoreRedis struct {
	client *redis.Client
	prefix string
}

// New creates a new Redis-backed StoreRedis. The keyPrefix is prepended to all keys
// to namespace them within the Redis instance.
func New(client *redis.Client, keyPrefix string) *StoreRedis {
	return &StoreRedis{
		client: client,
		prefix: keyPrefix,
	}
}

func (s *StoreRedis) redisKey(key string) string {
	return s.prefix + key
}

func (s *StoreRedis) userKey(redisKey string) string {
	return strings.TrimPrefix(redisKey, s.prefix)
}

func (s *StoreRedis) Get(ctx context.Context, key string) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	val, err := s.client.Get(ctx, s.redisKey(key)).Result()
	if err == redis.Nil {
		return "", storemd.NotFoundError
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

func (s *StoreRedis) Set(ctx context.Context, key, value string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return s.client.Set(ctx, s.redisKey(key), value, 0).Err()
}

func (s *StoreRedis) Delete(ctx context.Context, key string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	n, err := s.client.Del(ctx, s.redisKey(key)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return storemd.NotFoundError
	}
	return nil
}

func (s *StoreRedis) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	// Build SCAN match pattern
	matchPattern := s.prefix + args.Prefix + "*"

	// Collect all matching keys using SCAN
	var keys []string
	var cursor uint64
	for {
		var batch []string
		var err error
		batch, cursor, err = s.client.Scan(ctx, cursor, matchPattern, 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		return []storemd.KeyValuePair{}, nil
	}

	// Sort keys lexicographically
	sort.Strings(keys)

	// Convert to user keys and apply StartAfter filter
	var filtered []string
	for _, rk := range keys {
		uk := s.userKey(rk)
		if args.StartAfter != "" && uk <= args.StartAfter {
			continue
		}
		filtered = append(filtered, rk)
	}

	// Apply Limit
	if args.Limit > 0 && args.Limit < len(filtered) {
		filtered = filtered[:args.Limit]
	}

	if len(filtered) == 0 {
		return []storemd.KeyValuePair{}, nil
	}

	// MGET values
	vals, err := s.client.MGet(ctx, filtered...).Result()
	if err != nil {
		return nil, err
	}

	result := make([]storemd.KeyValuePair, len(filtered))
	for i, rk := range filtered {
		val := ""
		if vals[i] != nil {
			str, ok := vals[i].(string)
			if !ok {
				return nil, fmt.Errorf("unexpected type for key %q: %T", s.userKey(rk), vals[i])
			}
			val = str
		}
		result[i] = storemd.KeyValuePair{
			Key:   s.userKey(rk),
			Value: val,
		}
	}

	return result, nil
}
