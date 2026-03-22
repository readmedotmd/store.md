package badger

import (
	"context"
	"strings"

	storemd "github.com/readmedotmd/store.md"

	badgerdb "github.com/dgraph-io/badger/v4"
)

// StoreBadger is a badger-backed implementation of storemd.StoreBadger.
type StoreBadger struct {
	db *badgerdb.DB
}

// New opens or creates a badger database at the given directory path.
func New(dir string) (*StoreBadger, error) {
	opts := badgerdb.DefaultOptions(dir)
	opts.Logger = nil
	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, err
	}
	return &StoreBadger{db: db}, nil
}

// Close closes the underlying badger database.
func (s *StoreBadger) Close() error {
	return s.db.Close()
}

// Clear removes all key-value pairs from the store.
func (s *StoreBadger) Clear(ctx context.Context) error {
	return s.db.DropAll()
}

func (s *StoreBadger) Get(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get([]byte(key))
		if err == badgerdb.ErrKeyNotFound {
			return storemd.NotFoundError
		}
		if err != nil {
			return err
		}
		v, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		val = string(v)
		return nil
	})
	return val, err
}

func (s *StoreBadger) Set(ctx context.Context, key, value string) error {
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte(key), []byte(value))
	})
}

func (s *StoreBadger) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	var created bool
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		_, err := txn.Get([]byte(key))
		if err == nil {
			return nil // key exists
		}
		if err != badgerdb.ErrKeyNotFound {
			return err
		}
		created = true
		return txn.Set([]byte(key), []byte(value))
	})
	return created, err
}

func (s *StoreBadger) Delete(ctx context.Context, key string) error {
	return s.db.Update(func(txn *badgerdb.Txn) error {
		// Check if key exists first.
		_, err := txn.Get([]byte(key))
		if err == badgerdb.ErrKeyNotFound {
			return storemd.NotFoundError
		}
		if err != nil {
			return err
		}
		return txn.Delete([]byte(key))
	})
}

func (s *StoreBadger) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	var result []storemd.KeyValuePair
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = true

		if args.Prefix != "" {
			opts.Prefix = []byte(args.Prefix)
		}

		it := txn.NewIterator(opts)
		defer it.Close()

		// Determine where to start iterating.
		if args.Prefix != "" {
			it.Seek([]byte(args.Prefix))
		} else {
			it.Rewind()
		}

		// If StartAfter is set, advance past it.
		if args.StartAfter != "" {
			startKey := []byte(args.StartAfter)
			it.Seek(startKey)
			// Skip past the StartAfter key itself (exclusive).
			if it.Valid() {
				item := it.Item()
				if string(item.Key()) == args.StartAfter {
					it.Next()
				}
			}
		}

		for ; it.Valid(); it.Next() {
			if args.Limit > 0 && len(result) >= args.Limit {
				break
			}
			item := it.Item()
			k := string(item.Key())

			// If prefix is set, ensure key still matches.
			if args.Prefix != "" && !strings.HasPrefix(k, args.Prefix) {
				break
			}

			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			result = append(result, storemd.KeyValuePair{Key: k, Value: string(v)})
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
