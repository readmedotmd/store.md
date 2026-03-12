//go:build js && wasm

package indexeddb

import (
	"context"
	"sort"
	"strings"
	"syscall/js"

	storemd "github.com/readmedotmd/store.md"
)

const defaultStoreName = "kv"

// StoreIndexedDB is an IndexedDB-backed implementation of storemd.Store for use in WASM.
type StoreIndexedDB struct {
	db        js.Value
	storeName string
}

// New opens (or creates) an IndexedDB database with the given name and returns a StoreIndexedDB.
func New(dbName string) (*StoreIndexedDB, error) {
	db, err := openDB(dbName, defaultStoreName)
	if err != nil {
		return nil, err
	}
	return &StoreIndexedDB{db: db, storeName: defaultStoreName}, nil
}

// Close closes the underlying IndexedDB database.
func (s *StoreIndexedDB) Close() error {
	s.db.Call("close")
	return nil
}

// Clear removes all key-value pairs from the store.
func (s *StoreIndexedDB) Clear(ctx context.Context) error {
	ch := make(chan error, 1)
	go func() {
		tx := s.db.Call("transaction", s.storeName, "readwrite")
		store := tx.Call("objectStore", s.storeName)
		store.Call("clear")
		ch <- awaitTransaction(tx)
	}()
	return <-ch
}

func (s *StoreIndexedDB) Get(ctx context.Context, key string) (string, error) {
	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		tx := s.db.Call("transaction", s.storeName, "readonly")
		store := tx.Call("objectStore", s.storeName)
		req := store.Call("get", key)

		val, err := awaitRequest(req)
		if err != nil {
			ch <- result{err: err}
			return
		}
		if val.IsUndefined() || val.IsNull() {
			ch <- result{err: storemd.NotFoundError}
			return
		}
		ch <- result{val: val.String()}
	}()
	r := <-ch
	return r.val, r.err
}

func (s *StoreIndexedDB) Set(ctx context.Context, key, value string) error {
	ch := make(chan error, 1)
	go func() {
		tx := s.db.Call("transaction", s.storeName, "readwrite")
		store := tx.Call("objectStore", s.storeName)
		store.Call("put", value, key)
		ch <- awaitTransaction(tx)
	}()
	return <-ch
}

func (s *StoreIndexedDB) Delete(ctx context.Context, key string) error {
	// Check existence first — IndexedDB delete is a no-op for missing keys.
	_, err := s.Get(ctx, key)
	if err != nil {
		return err
	}

	ch := make(chan error, 1)
	go func() {
		tx := s.db.Call("transaction", s.storeName, "readwrite")
		store := tx.Call("objectStore", s.storeName)
		store.Call("delete", key)
		ch <- awaitTransaction(tx)
	}()
	return <-ch
}

func (s *StoreIndexedDB) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	type result struct {
		items []storemd.KeyValuePair
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		tx := s.db.Call("transaction", s.storeName, "readonly")
		store := tx.Call("objectStore", s.storeName)

		// Use a key range if we have a prefix to narrow the scan.
		var req js.Value
		if args.Prefix != "" {
			lower := js.Global().Get("IDBKeyRange").Call("bound", args.Prefix, args.Prefix+"\uffff")
			req = store.Call("openCursor", lower)
		} else {
			req = store.Call("openCursor")
		}

		var items []storemd.KeyValuePair
		done := make(chan error, 1)

		var onSuccess, onError js.Func
		onSuccess = js.FuncOf(func(this js.Value, p []js.Value) any {
			cursor := req.Get("result")
			if cursor.IsNull() || cursor.IsUndefined() {
				done <- nil
				return nil
			}

			key := cursor.Get("key").String()
			value := cursor.Get("value").String()

			if args.Prefix != "" && !strings.HasPrefix(key, args.Prefix) {
				done <- nil
				return nil
			}

			if args.StartAfter != "" && key <= args.StartAfter {
				cursor.Call("continue")
				return nil
			}

			items = append(items, storemd.KeyValuePair{Key: key, Value: value})

			if args.Limit > 0 && len(items) >= args.Limit {
				done <- nil
				return nil
			}

			cursor.Call("continue")
			return nil
		})
		onError = js.FuncOf(func(this js.Value, p []js.Value) any {
			done <- js.Error{Value: req.Get("error")}
			return nil
		})

		req.Set("onsuccess", onSuccess)
		req.Set("onerror", onError)

		err := <-done
		onSuccess.Release()
		onError.Release()

		if err != nil {
			ch <- result{err: err}
			return
		}
		if items == nil {
			items = []storemd.KeyValuePair{}
		}
		// IndexedDB iterates keys in order, but sort to guarantee the contract.
		sort.Slice(items, func(i, j int) bool {
			return items[i].Key < items[j].Key
		})
		ch <- result{items: items}
	}()
	r := <-ch
	return r.items, r.err
}

// openDB opens an IndexedDB database, creating the object store if needed.
func openDB(dbName, storeName string) (js.Value, error) {
	idb := js.Global().Get("indexedDB")
	req := idb.Call("open", dbName, 1)

	upgradeDone := make(chan struct{}, 1)

	var onUpgrade js.Func
	onUpgrade = js.FuncOf(func(this js.Value, p []js.Value) any {
		db := req.Get("result")
		if !db.Get("objectStoreNames").Call("contains", storeName).Bool() {
			db.Call("createObjectStore", storeName)
		}
		upgradeDone <- struct{}{}
		return nil
	})
	req.Set("onupgradeneeded", onUpgrade)

	result := make(chan js.Value, 1)
	errCh := make(chan error, 1)

	var onSuccess, onError js.Func
	onSuccess = js.FuncOf(func(this js.Value, p []js.Value) any {
		result <- req.Get("result")
		return nil
	})
	onError = js.FuncOf(func(this js.Value, p []js.Value) any {
		errCh <- js.Error{Value: req.Get("error")}
		return nil
	})
	req.Set("onsuccess", onSuccess)
	req.Set("onerror", onError)

	select {
	case db := <-result:
		onUpgrade.Release()
		onSuccess.Release()
		onError.Release()
		return db, nil
	case err := <-errCh:
		onUpgrade.Release()
		onSuccess.Release()
		onError.Release()
		return js.Value{}, err
	}
}

// awaitRequest blocks until an IDBRequest completes and returns the result.
func awaitRequest(req js.Value) (js.Value, error) {
	done := make(chan js.Value, 1)
	errCh := make(chan error, 1)

	var onSuccess, onError js.Func
	onSuccess = js.FuncOf(func(this js.Value, p []js.Value) any {
		done <- req.Get("result")
		return nil
	})
	onError = js.FuncOf(func(this js.Value, p []js.Value) any {
		errCh <- js.Error{Value: req.Get("error")}
		return nil
	})
	req.Set("onsuccess", onSuccess)
	req.Set("onerror", onError)

	select {
	case val := <-done:
		onSuccess.Release()
		onError.Release()
		return val, nil
	case err := <-errCh:
		onSuccess.Release()
		onError.Release()
		return js.Value{}, err
	}
}

// awaitTransaction blocks until an IDBTransaction completes.
func awaitTransaction(tx js.Value) error {
	done := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	var onComplete, onError js.Func
	onComplete = js.FuncOf(func(this js.Value, p []js.Value) any {
		done <- struct{}{}
		return nil
	})
	onError = js.FuncOf(func(this js.Value, p []js.Value) any {
		errCh <- js.Error{Value: tx.Get("error")}
		return nil
	})
	tx.Set("oncomplete", onComplete)
	tx.Set("onerror", onError)

	select {
	case <-done:
		onComplete.Release()
		onError.Release()
		return nil
	case err := <-errCh:
		onComplete.Release()
		onError.Release()
		return err
	}
}
