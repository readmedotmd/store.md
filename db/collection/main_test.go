package collection

import (
	"context"
	"errors"
	"testing"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
)

type Todo struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
	Order int    `json:"order"`
}

type Profile struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

func newCollection[T any](t *testing.T, name string, opts ...Option) (*Collection[T], storemd.Store) {
	t.Helper()
	mem := memory.New()
	c := New[T](mem, name, opts...)
	return c, mem
}

// ---------------------------------------------------------------------------
// Set and Get
// ---------------------------------------------------------------------------

func TestSetAndGet(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	todo := Todo{Title: "Buy groceries", Done: false, Order: 1}
	if err := c.Set(ctx, "t1", todo); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := c.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != todo {
		t.Fatalf("expected %+v, got %+v", todo, got)
	}
}

func TestGet_NotFound(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	_, err := c.Get(ctx, "nonexistent")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSet_OverwritesExisting(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "v1", Done: false, Order: 1})
	c.Set(ctx, "t1", Todo{Title: "v2", Done: true, Order: 2})

	got, _ := c.Get(ctx, "t1")
	expected := Todo{Title: "v2", Done: true, Order: 2}
	if got != expected {
		t.Fatalf("expected %+v, got %+v", expected, got)
	}
}

func TestSet_ZeroValuesWritten(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Write non-zero, then overwrite with zero.
	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 5})
	c.Set(ctx, "t1", Todo{})

	got, _ := c.Get(ctx, "t1")
	expected := Todo{} // all zero values
	if got != expected {
		t.Fatalf("expected zero values %+v, got %+v", expected, got)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDelete(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello"})

	if err := c.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := c.Get(ctx, "t1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	err := c.Delete(ctx, "nonexistent")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete_RemovesAllFields(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 3})
	c.Delete(ctx, "t1")

	// Verify no keys remain.
	list, _ := mem.List(ctx, storemd.ListArgs{Prefix: "todos/t1/"})
	if len(list) != 0 {
		t.Fatalf("expected 0 remaining keys, got %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestList_Empty(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	docs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil, got %v", docs)
	}
}

func TestList_MultipleDocuments(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first", Order: 1})
	c.Set(ctx, "b", Todo{Title: "second", Order: 2})
	c.Set(ctx, "c", Todo{Title: "third", Order: 3})

	docs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(docs))
	}

	// Should be in lexicographic order by ID.
	if docs[0].ID != "a" || docs[1].ID != "b" || docs[2].ID != "c" {
		t.Fatalf("unexpected order: %v, %v, %v", docs[0].ID, docs[1].ID, docs[2].ID)
	}
	if docs[0].Value.Title != "first" {
		t.Fatalf("expected first, got %q", docs[0].Value.Title)
	}
	if docs[2].Value.Order != 3 {
		t.Fatalf("expected Order=3, got %d", docs[2].Value.Order)
	}
}

func TestList_AfterDelete(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})
	c.Delete(ctx, "a")

	docs, _ := c.List(ctx)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if docs[0].ID != "b" {
		t.Fatalf("expected doc b, got %q", docs[0].ID)
	}
}

// ---------------------------------------------------------------------------
// Field-level independence (key per field)
// ---------------------------------------------------------------------------

func TestFieldsStoredAsIndependentKeys(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 5})

	// Each field should be a separate key.
	title, err := mem.Get(ctx, "todos/t1/title")
	if err != nil || title != `"hello"` {
		t.Fatalf("title: got %q err=%v", title, err)
	}

	done, err := mem.Get(ctx, "todos/t1/done")
	if err != nil || done != `true` {
		t.Fatalf("done: got %q err=%v", done, err)
	}

	order, err := mem.Get(ctx, "todos/t1/order")
	if err != nil || order != `5` {
		t.Fatalf("order: got %q err=%v", order, err)
	}
}

func TestPartialDocument_MissingFieldsAreZeroValues(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Write only the title field and index key directly (simulating partial sync).
	mem.Set(ctx, "todos/%idx%t1", "1")
	mem.Set(ctx, "todos/t1/title", `"partial"`)

	got, err := c.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "partial" {
		t.Fatalf("expected partial, got %q", got.Title)
	}
	if got.Done != false {
		t.Fatal("expected Done=false for missing field")
	}
	if got.Order != 0 {
		t.Fatalf("expected Order=0 for missing field, got %d", got.Order)
	}
}

func TestFieldLevelUpdate(t *testing.T) {
	_, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Simulate two nodes each writing one field independently.
	mem.Set(ctx, "todos/%idx%t1", "1")
	mem.Set(ctx, "todos/t1/title", `"from node A"`)
	mem.Set(ctx, "todos/t1/done", `true`)

	// Read via collection — both fields present.
	c := New[Todo](mem, "todos")
	got, _ := c.Get(ctx, "t1")
	if got.Title != "from node A" || got.Done != true {
		t.Fatalf("expected merged fields, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// WithPrefix option
// ---------------------------------------------------------------------------

func TestWithPrefix(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos", WithPrefix("app/"))
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello"})

	// Key should include prefix.
	title, err := mem.Get(ctx, "app/todos/t1/title")
	if err != nil || title != `"hello"` {
		t.Fatalf("expected key with prefix, got %q err=%v", title, err)
	}

	// Get should work.
	got, _ := c.Get(ctx, "t1")
	if got.Title != "hello" {
		t.Fatalf("expected hello, got %q", got.Title)
	}

	// List should work.
	docs, _ := c.List(ctx)
	if len(docs) != 1 || docs[0].ID != "t1" {
		t.Fatalf("expected 1 doc t1, got %v", docs)
	}
}

func TestMultipleCollections_Isolated(t *testing.T) {
	mem := memory.New()
	todos := New[Todo](mem, "todos")
	profiles := New[Profile](mem, "profiles")
	ctx := context.Background()

	todos.Set(ctx, "t1", Todo{Title: "task"})
	profiles.Set(ctx, "p1", Profile{Name: "Alice"})

	todoList, _ := todos.List(ctx)
	profileList, _ := profiles.List(ctx)

	if len(todoList) != 1 || todoList[0].ID != "t1" {
		t.Fatalf("todos: expected 1, got %v", todoList)
	}
	if len(profileList) != 1 || profileList[0].ID != "p1" {
		t.Fatalf("profiles: expected 1, got %v", profileList)
	}
}

// ---------------------------------------------------------------------------
// JSON struct tags
// ---------------------------------------------------------------------------

type Tagged struct {
	Name    string `json:"full_name"`
	Skipped string `json:"-"`
	Plain   string
}

func TestJsonTagFieldNames(t *testing.T) {
	c, mem := newCollection[Tagged](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", Tagged{Name: "Alice", Skipped: "secret", Plain: "visible"})

	// "full_name" should be used (from json tag).
	v, err := mem.Get(ctx, "items/d1/full_name")
	if err != nil || v != `"Alice"` {
		t.Fatalf("full_name: got %q err=%v", v, err)
	}

	// "Skipped" should not exist (json:"-").
	_, err = mem.Get(ctx, "items/d1/Skipped")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("Skipped field should not be stored, got err=%v", err)
	}
	_, err = mem.Get(ctx, "items/d1/-")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("'-' key should not exist, got err=%v", err)
	}

	// "Plain" should use Go field name.
	v, err = mem.Get(ctx, "items/d1/Plain")
	if err != nil || v != `"visible"` {
		t.Fatalf("Plain: got %q err=%v", v, err)
	}
}

func TestSkippedFieldNotRestored(t *testing.T) {
	c, _ := newCollection[Tagged](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", Tagged{Name: "Alice", Skipped: "secret", Plain: "visible"})

	got, _ := c.Get(ctx, "d1")
	if got.Skipped != "" {
		t.Fatalf("expected Skipped to be empty, got %q", got.Skipped)
	}
	if got.Name != "Alice" || got.Plain != "visible" {
		t.Fatalf("unexpected: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Various field types
// ---------------------------------------------------------------------------

type TypedFields struct {
	Str     string  `json:"str"`
	Int     int     `json:"int"`
	Float   float64 `json:"float"`
	Bool    bool    `json:"bool"`
	Uint    uint64  `json:"uint"`
	Nested  Inner   `json:"nested"`
}

type Inner struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func TestVariousFieldTypes(t *testing.T) {
	c, _ := newCollection[TypedFields](t, "typed")
	ctx := context.Background()

	doc := TypedFields{
		Str:   "hello",
		Int:   -42,
		Float: 3.14,
		Bool:  true,
		Uint:  999,
		Nested: Inner{X: 1, Y: 2},
	}

	c.Set(ctx, "d1", doc)
	got, err := c.Get(ctx, "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != doc {
		t.Fatalf("expected %+v, got %+v", doc, got)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestNew_PanicsOnNonStruct(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for non-struct type")
		}
	}()
	New[string](memory.New(), "bad")
}

// ---------------------------------------------------------------------------
// Index keys
// ---------------------------------------------------------------------------

func TestIndexKeyCreatedOnSet(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello"})

	// Index key should exist.
	v, err := mem.Get(ctx, "todos/%idx%t1")
	if err != nil || v != "1" {
		t.Fatalf("expected index key, got %q err=%v", v, err)
	}
}

func TestIndexKeyRemovedOnDelete(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello"})
	c.Delete(ctx, "t1")

	_, err := mem.Get(ctx, "todos/%idx%t1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected index key to be deleted, got err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// List with pagination (ListArgs)
// ---------------------------------------------------------------------------

func TestList_Limit(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})
	c.Set(ctx, "c", Todo{Title: "third"})

	docs, err := c.List(ctx, ListArgs{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "a" || docs[1].ID != "b" {
		t.Fatalf("expected a,b got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_StartAfter(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})
	c.Set(ctx, "c", Todo{Title: "third"})

	docs, err := c.List(ctx, ListArgs{StartAfter: "a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "b" || docs[1].ID != "c" {
		t.Fatalf("expected b,c got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_StartAfterAndLimit(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		c.Set(ctx, id, Todo{Title: id})
	}

	docs, err := c.List(ctx, ListArgs{StartAfter: "b", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "c" || docs[1].ID != "d" {
		t.Fatalf("expected c,d got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_Prefix(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})
	c.Set(ctx, "user-bob", Todo{Title: "bob"})
	c.Set(ctx, "task-1", Todo{Title: "task"})

	docs, err := c.List(ctx, ListArgs{Prefix: "user-"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "user-alice" || docs[1].ID != "user-bob" {
		t.Fatalf("expected user-alice,user-bob got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_PrefixWithLimit(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})
	c.Set(ctx, "user-bob", Todo{Title: "bob"})
	c.Set(ctx, "user-charlie", Todo{Title: "charlie"})

	docs, err := c.List(ctx, ListArgs{Prefix: "user-", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "user-alice" || docs[1].ID != "user-bob" {
		t.Fatalf("expected user-alice,user-bob got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_Pagination_FullTraversal(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		c.Set(ctx, id, Todo{Title: id})
	}

	// Page through all docs 2 at a time.
	var all []Document[Todo]
	var cursor string
	for {
		page, err := c.List(ctx, ListArgs{StartAfter: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		cursor = page[len(page)-1].ID
	}

	if len(all) != 5 {
		t.Fatalf("expected 5 docs total, got %d", len(all))
	}
	for i, id := range ids {
		if all[i].ID != id {
			t.Fatalf("doc %d: expected %q, got %q", i, id, all[i].ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestDocIDWithSpecialChars(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// UUIDs with hyphens are common doc IDs.
	id := "550e8400-e29b-41d4-a716-446655440000"
	c.Set(ctx, id, Todo{Title: "uuid doc"})

	got, err := c.Get(ctx, id)
	if err != nil || got.Title != "uuid doc" {
		t.Fatalf("expected uuid doc, got %+v err=%v", got, err)
	}

	docs, _ := c.List(ctx)
	if len(docs) != 1 || docs[0].ID != id {
		t.Fatalf("List: expected 1 doc with UUID id, got %v", docs)
	}
}
