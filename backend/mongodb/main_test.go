package mongodb

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"

	storemd "github.com/readmedotmd/store.md"

	"github.com/ory/dockertest/v3"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var mongoClient *mongo.Client

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	resource, err := pool.Run("mongo", "7", nil)
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	uri := fmt.Sprintf("mongodb://localhost:%s", resource.GetPort("27017/tcp"))

	if err := pool.Retry(func() error {
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			return err
		}
		err = client.Ping(context.Background(), nil)
		if err != nil {
			return err
		}
		mongoClient = client
		return nil
	}); err != nil {
		log.Fatalf("Could not connect to mongo: %s", err)
	}

	code := m.Run()

	if err := pool.Purge(resource); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}

	os.Exit(code)
}

func TestStore(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		col := mongoClient.Database("testdb").Collection(t.Name())
		return New(col)
	})
}

func TestStore_Clear(t *testing.T) {
	storemd.RunClearTests(t, func(t *testing.T) storemd.Clearable {
		col := mongoClient.Database("testdb").Collection(t.Name())
		return New(col)
	})
}
