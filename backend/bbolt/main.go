package bbolt

import (
	"context"
	"strings"

	storemd "github.com/readmedotmd/store.md"

	bolt "go.etcd.io/bbolt"
)

var bucketName = []byte("kv")

// StoreBBolt is a bbolt-backed implementation of storemd.StoreBBolt.
type StoreBBolt struct {
	db *bolt.DB
}

// New opens (or creates) a bbolt database at the given path and returns a StoreBBolt.
func New(path string) (*StoreBBolt, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}
	// Ensure the bucket exists.
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &StoreBBolt{db: db}, nil
}

// Close closes the underlying bbolt database.
func (s *StoreBBolt) Close() error {
	return s.db.Close()
}

// Clear removes all key-value pairs from the store.
func (s *StoreBBolt) Clear(ctx context.Context) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(bucketName); err != nil {
			return err
		}
		_, err := tx.CreateBucket(bucketName)
		return err
	})
}

func (s *StoreBBolt) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(key))
		if v == nil {
			return storemd.NotFoundError
		}
		value = string(v)
		return nil
	})
	return value, err
}

func (s *StoreBBolt) Set(ctx context.Context, key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(key), []byte(value))
	})
}

func (s *StoreBBolt) Delete(ctx context.Context, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(key))
		if v == nil {
			return storemd.NotFoundError
		}
		return b.Delete([]byte(key))
	})
}

func (s *StoreBBolt) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	var result []storemd.KeyValuePair
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		c := b.Cursor()

		var k, v []byte
		if args.Prefix != "" {
			k, v = c.Seek([]byte(args.Prefix))
		} else {
			k, v = c.First()
		}

		for ; k != nil; k, v = c.Next() {
			key := string(k)

			// Stop if we've moved past the prefix.
			if args.Prefix != "" && !strings.HasPrefix(key, args.Prefix) {
				break
			}

			// Skip keys <= StartAfter.
			if args.StartAfter != "" && key <= args.StartAfter {
				continue
			}

			result = append(result, storemd.KeyValuePair{Key: key, Value: string(v)})

			if args.Limit > 0 && len(result) >= args.Limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = []storemd.KeyValuePair{}
	}
	return result, nil
}
