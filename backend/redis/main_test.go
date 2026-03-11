package redisstore

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"

	storemd "github.com/readmedotmd/store.md"
	"github.com/ory/dockertest/v3"
	"github.com/redis/go-redis/v9"
)

var redisAddr string

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		log.Fatalf("Could not connect to Docker: %s", err)
	}

	resource, err := pool.Run("redis", "7", nil)
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	redisAddr = fmt.Sprintf("localhost:%s", resource.GetPort("6379/tcp"))

	// Wait for Redis to be ready
	if err := pool.Retry(func() error {
		client := redis.NewClient(&redis.Options{Addr: redisAddr})
		defer client.Close()
		return client.Ping(context.Background()).Err()
	}); err != nil {
		log.Fatalf("Could not connect to Redis: %s", err)
	}

	code := m.Run()

	if err := pool.Purge(resource); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}

	os.Exit(code)
}

func TestRedisStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		client := redis.NewClient(&redis.Options{Addr: redisAddr})
		t.Cleanup(func() { client.Close() })

		prefix := t.Name() + ":"
		return New(client, prefix)
	})
}
