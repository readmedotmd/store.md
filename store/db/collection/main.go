// Package collection provides a typed document-collection abstraction on top
// of any [storemd.Store]. Each struct field is stored as an independent key,
// enabling field-level LWW (Last Writer Wins) conflict resolution when the
// underlying store is a sync store.
//
// # Key scheme
//
// Each document produces two kinds of store keys:
//
//   - Index key: {prefix}{collection}/%idx%{docID} → "1"
//   - Field keys: {prefix}{collection}/{docID}/{fieldName} → JSON-encoded value
//
// For example, a collection named "todos" with a struct containing Title,
// Done, and CreatedAt fields stores:
//
//	todos/%idx%abc123          → "1"
//	todos/abc123/title         → "Buy groceries"
//	todos/abc123/done          → false
//	todos/abc123/createdAt     → 1710000000
//
// The index key enables efficient document listing with pagination (Prefix,
// StartAfter, Limit) without scanning all field keys. Because each field is
// a separate key, sync stores resolve conflicts at the field level: if two
// nodes concurrently update different fields of the same document, both
// updates are preserved.
//
// # Struct mapping
//
// Field names are derived from `json` struct tags (falling back to the Go
// field name). Fields tagged `json:"-"` are skipped. Each field value is
// JSON-encoded as the store value string. Options after a comma in the json
// tag are ignored for naming purposes (e.g. `json:"name,omitempty"` uses
// "name" as the key segment).
//
// Only flat structs with exported fields are supported. Nested structs,
// slices, and maps are serialized as a single JSON value into one key
// (they won't get field-level LWW). Unexported fields are always ignored.
//
// # Listing documents
//
// [Collection.List] supports the same pagination options as [storemd.ListArgs]:
// Prefix (filters doc IDs), StartAfter (doc ID cursor), and Limit (max
// documents returned). Listing scans the index keys and then assembles each
// document from its field keys. Documents are always returned in lexicographic
// order by ID.
//
// The [ListArgs] parameter is variadic for convenience — calling List(ctx)
// with no args returns all documents.
//
// # Partial documents
//
// If a sync has delivered some fields but not others, [Collection.Get]
// returns a partially-filled struct (Go zero values for missing fields).
// This is expected behavior for field-level LWW. Similarly, [Collection.List]
// assembles each document from whatever field keys exist — missing fields
// are left as Go zero values.
//
// # Sync store integration
//
// Collection works with any [storemd.Store], including sync stores (which
// implement the Store interface). When used with a sync store, each field
// key syncs independently, giving automatic field-level LWW conflict
// resolution. Two nodes can update different fields of the same document
// concurrently and both changes will merge cleanly.
//
// For best performance with a sync store, wrap the store in a
// [cache.StoreCache] and wire up invalidation:
//
//	ss := core.New(backend)
//	cached := cache.New(ss)
//	ss.OnUpdate(func(item core.SyncStoreItem) {
//	    cached.InvalidateKey(item.Key)
//	})
//	c := collection.New[Todo](cached, "todos")
//
// # Thread safety
//
// Collection itself has no mutable state after construction — all state lives
// in the underlying [storemd.Store]. Thread safety depends entirely on the
// store implementation. All standard store implementations (memory, bbolt,
// badger, sql, sync) are safe for concurrent use, so Collection is too.
//
// # Usage
//
//	type Todo struct {
//	    Title string `json:"title"`
//	    Done  bool   `json:"done"`
//	}
//
//	c := collection.New[Todo](store, "todos")
//
//	// Create or update
//	c.Set(ctx, "abc123", Todo{Title: "Buy groceries", Done: false})
//
//	// Read
//	doc, err := c.Get(ctx, "abc123")
//
//	// List all documents
//	all, err := c.List(ctx)
//
//	// List with pagination
//	page, err := c.List(ctx, collection.ListArgs{Limit: 10})
//	next, err := c.List(ctx, collection.ListArgs{StartAfter: page[9].ID, Limit: 10})
//
//	// List with ID prefix filter
//	userDocs, err := c.List(ctx, collection.ListArgs{Prefix: "user-"})
//
//	// Combine prefix, cursor, and limit
//	page, err := c.List(ctx, collection.ListArgs{
//	    Prefix: "user-", StartAfter: "user-alice", Limit: 20,
//	})
//
//	// Delete
//	c.Delete(ctx, "abc123")
//
// With a key prefix (useful for multi-tenant or namespaced stores):
//
//	c := collection.New[Todo](store, "todos", collection.WithPrefix("myapp/"))
//	// Keys become: myapp/todos/%idx%abc123, myapp/todos/abc123/title, etc.
package collection

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	storemd "github.com/readmedotmd/store.md"
)

const idxInfix = "%idx%"

// fieldInfo holds cached reflection metadata for a single struct field.
type fieldInfo struct {
	name  string // store key segment (from json tag or field name)
	index int    // struct field index for reflect.Value.Field()
}

// Document wraps a document value with its ID. It is the return type
// for [Collection.List], providing the document ID alongside the
// deserialized struct value.
type Document[T any] struct {
	// ID is the document identifier (the string passed to Set/Get/Delete).
	ID string
	// Value is the deserialized struct with fields populated from store keys.
	Value T
}

// ListArgs controls pagination and filtering for [Collection.List].
// All fields mirror the semantics of [storemd.ListArgs] but operate on
// document IDs rather than raw store keys.
type ListArgs struct {
	// Prefix filters documents whose ID starts with this string.
	Prefix string
	// StartAfter is a document ID cursor for pagination. Only documents
	// with IDs lexicographically after this value are returned.
	StartAfter string
	// Limit is the maximum number of documents to return. 0 means no limit.
	Limit int
}

// Option configures a [Collection].
type Option func(*config)

type config struct {
	prefix string
}

// WithPrefix sets a global key prefix prepended before the collection name.
// For example, WithPrefix("myapp/") with collection name "todos" produces
// keys like "myapp/todos/{id}/{field}".
func WithPrefix(p string) Option {
	return func(c *config) { c.prefix = p }
}

// Collection manages documents of type T stored across individual field keys.
// T must be a struct type with exported fields. Create with [New].
//
// A Collection is safe for concurrent use as long as the underlying store is
// (all standard implementations are). The Collection itself holds no mutable
// state — reflection metadata is computed once at construction time.
type Collection[T any] struct {
	store  storemd.Store
	name   string
	prefix string      // prepended before collection name
	fields []fieldInfo // cached reflection metadata
}

// New creates a Collection for the given store and collection name. T must
// be a struct type; New panics if it is not.
//
// Reflection metadata is cached at construction time, so Get/Set/Delete/List
// operations have no repeated reflection overhead.
func New[T any](store storemd.Store, name string, opts ...Option) *Collection[T] {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}

	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("collection.New: T must be a struct, got %s", t.Kind()))
	}

	fields := make([]fieldInfo, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			parts := strings.SplitN(tag, ",", 2)
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
		}
		fields = append(fields, fieldInfo{name: name, index: i})
	}

	return &Collection[T]{
		store:  store,
		name:   name,
		prefix: cfg.prefix,
		fields: fields,
	}
}

// --- Key helpers ---

// idxKey returns the index key for a document (e.g. "todos/%idx%abc123").
func (c *Collection[T]) idxKey(id string) string {
	return c.prefix + c.name + "/" + idxInfix + id
}

// idxPrefix returns the store key prefix for scanning all index keys,
// optionally filtered by a doc ID prefix
// (e.g. "todos/%idx%" or "todos/%idx%user-").
func (c *Collection[T]) idxPrefix(docIDPrefix string) string {
	return c.prefix + c.name + "/" + idxInfix + docIDPrefix
}

// docPrefix returns the store key prefix for all fields of a specific
// document (e.g. "todos/abc123/").
func (c *Collection[T]) docPrefix(id string) string {
	return c.prefix + c.name + "/" + id + "/"
}

// fieldKey returns the full store key for a specific field of a specific
// document (e.g. "todos/abc123/title").
func (c *Collection[T]) fieldKey(id, fieldName string) string {
	return c.prefix + c.name + "/" + id + "/" + fieldName
}

// extractDocID extracts the document ID from an index key by stripping
// the idx prefix.
func (c *Collection[T]) extractDocID(indexKey string) string {
	p := c.prefix + c.name + "/" + idxInfix
	return indexKey[len(p):]
}

// --- CRUD operations ---

// Get retrieves a single document by ID. Returns [storemd.ErrNotFound] if
// the document does not exist. If some fields are missing (e.g. partial
// sync), the corresponding struct fields will have their Go zero values.
func (c *Collection[T]) Get(ctx context.Context, id string) (T, error) {
	var zero T

	// Check index key first (fast existence check).
	_, err := c.store.Get(ctx, c.idxKey(id))
	if err != nil {
		return zero, err
	}

	prefix := c.docPrefix(id)
	list, err := c.store.List(ctx, storemd.ListArgs{Prefix: prefix})
	if err != nil {
		return zero, err
	}

	val := reflect.New(reflect.TypeOf(zero)).Elem()
	for _, kv := range list {
		fieldName := kv.Key[len(prefix):]
		for _, fi := range c.fields {
			if fi.name == fieldName {
				field := val.Field(fi.index)
				if err := json.Unmarshal([]byte(kv.Value), field.Addr().Interface()); err != nil {
					return zero, fmt.Errorf("unmarshal field %q: %w", fieldName, err)
				}
				break
			}
		}
	}

	return val.Interface().(T), nil
}

// Set creates or updates a document. Every exported field of doc is written
// as an individual store key, plus an index key for listing. Zero-value
// fields are written too, so that clearing a field propagates via sync.
//
// Set is not atomic: each field is a separate write to the underlying store.
// In the unlikely event of a partial failure (e.g. the store returns an error
// mid-write), some fields may have been written while others were not. The
// index key is always written first, so a partially-written document will
// still appear in List results (with zero values for unwritten fields).
func (c *Collection[T]) Set(ctx context.Context, id string, doc T) error {
	// Write index key.
	if err := c.store.Set(ctx, c.idxKey(id), "1"); err != nil {
		return err
	}

	// Write each field.
	val := reflect.ValueOf(doc)
	for _, fi := range c.fields {
		field := val.Field(fi.index)
		encoded, err := json.Marshal(field.Interface())
		if err != nil {
			return fmt.Errorf("marshal field %q: %w", fi.name, err)
		}
		if err := c.store.Set(ctx, c.fieldKey(id, fi.name), string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the index key and all field keys for the given document ID.
// Returns [storemd.ErrNotFound] if the document does not exist.
//
// Like [Collection.Set], Delete is not atomic. The index key is removed first,
// so the document will immediately stop appearing in [Collection.List] results
// even if field key cleanup is still in progress.
func (c *Collection[T]) Delete(ctx context.Context, id string) error {
	// Check existence via index key.
	_, err := c.store.Get(ctx, c.idxKey(id))
	if err != nil {
		return err
	}

	// Delete index key.
	if err := c.store.Delete(ctx, c.idxKey(id)); err != nil {
		return err
	}

	// Delete all field keys.
	prefix := c.docPrefix(id)
	list, err := c.store.List(ctx, storemd.ListArgs{Prefix: prefix})
	if err != nil {
		return err
	}
	for _, kv := range list {
		if err := c.store.Delete(ctx, kv.Key); err != nil {
			return err
		}
	}
	return nil
}

// List returns documents in the collection matching the given arguments.
// It supports the same pagination semantics as [storemd.ListArgs]:
//
//   - Prefix: only return documents whose ID starts with this string
//   - StartAfter: return documents with IDs lexicographically after this value
//   - Limit: maximum number of documents to return (0 = no limit)
//
// Documents are returned in lexicographic order by ID. Each document is
// assembled from its field keys after the index scan.
func (c *Collection[T]) List(ctx context.Context, args ...ListArgs) ([]Document[T], error) {
	var la ListArgs
	if len(args) > 0 {
		la = args[0]
	}

	// Scan index keys with translated prefix/startAfter/limit.
	storeArgs := storemd.ListArgs{
		Prefix: c.idxPrefix(la.Prefix),
		Limit:  la.Limit,
	}
	if la.StartAfter != "" {
		storeArgs.StartAfter = c.idxKey(la.StartAfter)
	}

	idxList, err := c.store.List(ctx, storeArgs)
	if err != nil {
		return nil, err
	}
	if len(idxList) == 0 {
		return nil, nil
	}

	// For each doc ID, fetch fields and assemble the document.
	docs := make([]Document[T], 0, len(idxList))
	for _, idx := range idxList {
		docID := c.extractDocID(idx.Key)

		var val T
		rv := reflect.ValueOf(&val).Elem()
		docPrefix := c.docPrefix(docID)

		fieldList, err := c.store.List(ctx, storemd.ListArgs{Prefix: docPrefix})
		if err != nil {
			return nil, err
		}

		for _, kv := range fieldList {
			fieldName := kv.Key[len(docPrefix):]
			for _, fi := range c.fields {
				if fi.name == fieldName {
					field := rv.Field(fi.index)
					if err := json.Unmarshal([]byte(kv.Value), field.Addr().Interface()); err != nil {
						return nil, fmt.Errorf("unmarshal field %q of doc %q: %w", fieldName, docID, err)
					}
					break
				}
			}
		}

		docs = append(docs, Document[T]{ID: docID, Value: val})
	}

	return docs, nil
}
