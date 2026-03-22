package collection

import (
	"context"
	"errors"
	"strings"
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

func TestSet_MultipleDocumentsIndependent(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "first", Order: 1})
	c.Set(ctx, "t2", Todo{Title: "second", Order: 2})

	got1, _ := c.Get(ctx, "t1")
	got2, _ := c.Get(ctx, "t2")

	if got1.Title != "first" || got1.Order != 1 {
		t.Fatalf("t1: expected first/1, got %+v", got1)
	}
	if got2.Title != "second" || got2.Order != 2 {
		t.Fatalf("t2: expected second/2, got %+v", got2)
	}
}

func TestGet_EmptyStringFieldValue(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Empty string is a valid value, not the same as missing.
	c.Set(ctx, "t1", Todo{Title: "", Done: true, Order: 42})

	got, _ := c.Get(ctx, "t1")
	if got.Title != "" {
		t.Fatalf("expected empty title, got %q", got.Title)
	}
	if got.Done != true || got.Order != 42 {
		t.Fatalf("expected Done=true Order=42, got %+v", got)
	}
}

func TestGet_SpecialCharactersInStringField(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	special := `He said "hello" & <world> 日本語 🎉 line1\nline2`
	c.Set(ctx, "t1", Todo{Title: special})

	got, _ := c.Get(ctx, "t1")
	if got.Title != special {
		t.Fatalf("expected %q, got %q", special, got.Title)
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

func TestDelete_RemovesAllFieldKeys(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 3})
	c.Delete(ctx, "t1")

	// Verify no field keys remain.
	list, _ := mem.List(ctx, storemd.ListArgs{Prefix: "todos/t1/"})
	if len(list) != 0 {
		t.Fatalf("expected 0 remaining field keys, got %d", len(list))
	}

	// Verify no index key remains.
	_, err := mem.Get(ctx, "todos/%idx%t1")
	if !errors.Is(err, storemd.ErrNotFound) {
		t.Fatalf("expected index key to be deleted, got err=%v", err)
	}
}

func TestDelete_ThenResetSameID(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "original", Done: true, Order: 10})
	c.Delete(ctx, "t1")
	c.Set(ctx, "t1", Todo{Title: "recreated", Done: false, Order: 20})

	got, err := c.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get after re-Set: %v", err)
	}
	if got.Title != "recreated" || got.Done != false || got.Order != 20 {
		t.Fatalf("expected recreated doc, got %+v", got)
	}
}

func TestDelete_DoesNotAffectOtherDocs(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "first"})
	c.Set(ctx, "t2", Todo{Title: "second"})
	c.Delete(ctx, "t1")

	// t2 should be unaffected.
	got, err := c.Get(ctx, "t2")
	if err != nil || got.Title != "second" {
		t.Fatalf("t2 should be unaffected, got %+v err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// List (no args)
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

func TestList_SingleDocument(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "only", Todo{Title: "solo", Done: true, Order: 7})

	docs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if docs[0].ID != "only" {
		t.Fatalf("expected ID=only, got %q", docs[0].ID)
	}
	if docs[0].Value.Title != "solo" || docs[0].Value.Order != 7 {
		t.Fatalf("expected solo/7, got %+v", docs[0].Value)
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

func TestList_LexicographicOrdering(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Insert in non-alphabetical order.
	c.Set(ctx, "z", Todo{Title: "last"})
	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "m", Todo{Title: "middle"})

	docs, _ := c.List(ctx)
	if docs[0].ID != "a" || docs[1].ID != "m" || docs[2].ID != "z" {
		t.Fatalf("expected a,m,z got %s,%s,%s", docs[0].ID, docs[1].ID, docs[2].ID)
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

func TestList_ReturnsFullDocumentValues(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "alpha", Done: true, Order: 10})
	c.Set(ctx, "b", Todo{Title: "beta", Done: false, Order: 20})

	docs, _ := c.List(ctx)
	if docs[0].Value != (Todo{Title: "alpha", Done: true, Order: 10}) {
		t.Fatalf("doc a values wrong: %+v", docs[0].Value)
	}
	if docs[1].Value != (Todo{Title: "beta", Done: false, Order: 20}) {
		t.Fatalf("doc b values wrong: %+v", docs[1].Value)
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

func TestPartialDocument_IndexOnlyNoFields(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Index key exists but no field keys (extreme partial sync).
	mem.Set(ctx, "todos/%idx%t1", "1")

	got, err := c.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// All fields should be zero values.
	if got != (Todo{}) {
		t.Fatalf("expected all zero values, got %+v", got)
	}
}

func TestPartialDocument_AppearsInList(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// Partial doc: index + one field.
	mem.Set(ctx, "todos/%idx%t1", "1")
	mem.Set(ctx, "todos/t1/title", `"partial"`)

	// Full doc via collection.
	c.Set(ctx, "t2", Todo{Title: "full", Done: true, Order: 5})

	docs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "t1" || docs[0].Value.Title != "partial" {
		t.Fatalf("first doc wrong: %+v", docs[0])
	}
	if docs[1].ID != "t2" || docs[1].Value.Title != "full" {
		t.Fatalf("second doc wrong: %+v", docs[1])
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

func TestIndexKeyPreservedOnOverwrite(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "v1"})
	c.Set(ctx, "t1", Todo{Title: "v2"})

	// Index key should still be present.
	v, err := mem.Get(ctx, "todos/%idx%t1")
	if err != nil || v != "1" {
		t.Fatalf("expected index key after overwrite, got %q err=%v", v, err)
	}
}

func TestIndexKey_TotalKeyCount(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 1})

	// Should have: 1 index key + 3 field keys = 4 total keys.
	all, _ := mem.List(ctx, storemd.ListArgs{Prefix: "todos/"})
	if len(all) != 4 {
		keys := make([]string, len(all))
		for i, kv := range all {
			keys[i] = kv.Key
		}
		t.Fatalf("expected 4 keys (1 idx + 3 fields), got %d: %v", len(all), keys)
	}
}

// ---------------------------------------------------------------------------
// List with Limit
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

func TestList_LimitLargerThanCount(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})

	docs, err := c.List(ctx, ListArgs{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs (all), got %d", len(docs))
	}
}

func TestList_LimitOne(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})
	c.Set(ctx, "c", Todo{Title: "third"})

	docs, _ := c.List(ctx, ListArgs{Limit: 1})
	if len(docs) != 1 || docs[0].ID != "a" {
		t.Fatalf("expected 1 doc (a), got %v", docs)
	}
}

func TestList_LimitZeroMeansNoLimit(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		c.Set(ctx, id, Todo{Title: id})
	}

	docs, _ := c.List(ctx, ListArgs{Limit: 0})
	if len(docs) != 5 {
		t.Fatalf("expected 5 docs (Limit=0 means all), got %d", len(docs))
	}
}

// ---------------------------------------------------------------------------
// List with StartAfter
// ---------------------------------------------------------------------------

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

func TestList_StartAfterLastDoc(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "b", Todo{Title: "second"})

	docs, err := c.List(ctx, ListArgs{StartAfter: "b"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil after last doc, got %v", docs)
	}
}

func TestList_StartAfterNonExistentID(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "a", Todo{Title: "first"})
	c.Set(ctx, "c", Todo{Title: "third"})

	// "b" doesn't exist, but StartAfter should still work lexicographically.
	docs, err := c.List(ctx, ListArgs{StartAfter: "b"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "c" {
		t.Fatalf("expected [c], got %v", docs)
	}
}

// ---------------------------------------------------------------------------
// List with StartAfter + Limit (pagination)
// ---------------------------------------------------------------------------

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

func TestList_Pagination_PageSizeOne(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		c.Set(ctx, id, Todo{Title: id})
	}

	// Page through one at a time.
	var all []string
	var cursor string
	for {
		page, _ := c.List(ctx, ListArgs{StartAfter: cursor, Limit: 1})
		if len(page) == 0 {
			break
		}
		all = append(all, page[0].ID)
		cursor = page[0].ID
	}

	if len(all) != 3 || all[0] != "a" || all[1] != "b" || all[2] != "c" {
		t.Fatalf("expected [a,b,c], got %v", all)
	}
}

func TestList_Pagination_ExactPageBoundary(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	// 4 docs, page size 2 — exactly 2 full pages.
	for _, id := range []string{"a", "b", "c", "d"} {
		c.Set(ctx, id, Todo{Title: id})
	}

	page1, _ := c.List(ctx, ListArgs{Limit: 2})
	if len(page1) != 2 || page1[0].ID != "a" || page1[1].ID != "b" {
		t.Fatalf("page1: expected [a,b], got %v", page1)
	}

	page2, _ := c.List(ctx, ListArgs{StartAfter: page1[1].ID, Limit: 2})
	if len(page2) != 2 || page2[0].ID != "c" || page2[1].ID != "d" {
		t.Fatalf("page2: expected [c,d], got %v", page2)
	}

	page3, _ := c.List(ctx, ListArgs{StartAfter: page2[1].ID, Limit: 2})
	if page3 != nil {
		t.Fatalf("page3: expected nil, got %v", page3)
	}
}

// ---------------------------------------------------------------------------
// List with Prefix
// ---------------------------------------------------------------------------

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

func TestList_PrefixNoMatch(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})

	docs, err := c.List(ctx, ListArgs{Prefix: "admin-"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil for non-matching prefix, got %v", docs)
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

func TestList_PrefixWithStartAfter(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})
	c.Set(ctx, "user-bob", Todo{Title: "bob"})
	c.Set(ctx, "user-charlie", Todo{Title: "charlie"})
	c.Set(ctx, "task-1", Todo{Title: "task"})

	docs, err := c.List(ctx, ListArgs{Prefix: "user-", StartAfter: "user-alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "user-bob" || docs[1].ID != "user-charlie" {
		t.Fatalf("expected user-bob,user-charlie got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_PrefixWithStartAfterAndLimit(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})
	c.Set(ctx, "user-bob", Todo{Title: "bob"})
	c.Set(ctx, "user-charlie", Todo{Title: "charlie"})
	c.Set(ctx, "user-diana", Todo{Title: "diana"})

	docs, err := c.List(ctx, ListArgs{Prefix: "user-", StartAfter: "user-alice", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if docs[0].ID != "user-bob" || docs[1].ID != "user-charlie" {
		t.Fatalf("expected user-bob,user-charlie got %s,%s", docs[0].ID, docs[1].ID)
	}
}

func TestList_PrefixPagination_FullTraversal(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	c.Set(ctx, "other-1", Todo{Title: "other"})
	for _, name := range []string{"alice", "bob", "charlie", "diana", "eve"} {
		c.Set(ctx, "user-"+name, Todo{Title: name})
	}
	c.Set(ctx, "zzz", Todo{Title: "zzz"})

	// Page through user- prefix 2 at a time.
	var all []string
	var cursor string
	for {
		args := ListArgs{Prefix: "user-", Limit: 2}
		if cursor != "" {
			args.StartAfter = cursor
		}
		page, _ := c.List(ctx, args)
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			all = append(all, d.ID)
		}
		cursor = page[len(page)-1].ID
	}

	expected := []string{"user-alice", "user-bob", "user-charlie", "user-diana", "user-eve"}
	if len(all) != len(expected) {
		t.Fatalf("expected %d docs, got %d: %v", len(expected), len(all), all)
	}
	for i, id := range expected {
		if all[i] != id {
			t.Fatalf("doc %d: expected %q, got %q", i, id, all[i])
		}
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

	// Index key should include prefix.
	idx, err := mem.Get(ctx, "app/todos/%idx%t1")
	if err != nil || idx != "1" {
		t.Fatalf("expected index key with prefix, got %q err=%v", idx, err)
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

func TestWithPrefix_Delete(t *testing.T) {
	c, mem := newCollection[Todo](t, "todos", WithPrefix("app/"))
	ctx := context.Background()

	c.Set(ctx, "t1", Todo{Title: "hello", Done: true, Order: 1})
	c.Delete(ctx, "t1")

	// All keys with prefix should be gone.
	all, _ := mem.List(ctx, storemd.ListArgs{Prefix: "app/todos/"})
	if len(all) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(all))
	}
}

func TestWithPrefix_ListArgs(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos", WithPrefix("app/"))
	ctx := context.Background()

	c.Set(ctx, "user-alice", Todo{Title: "alice"})
	c.Set(ctx, "user-bob", Todo{Title: "bob"})
	c.Set(ctx, "task-1", Todo{Title: "task"})

	docs, _ := c.List(ctx, ListArgs{Prefix: "user-", Limit: 1})
	if len(docs) != 1 || docs[0].ID != "user-alice" {
		t.Fatalf("expected [user-alice], got %v", docs)
	}

	docs, _ = c.List(ctx, ListArgs{StartAfter: "user-alice"})
	if len(docs) != 1 || docs[0].ID != "user-bob" {
		t.Fatalf("expected [user-bob], got %v", docs)
	}
}

func TestWithPrefix_Isolation(t *testing.T) {
	mem := memory.New()
	ctx := context.Background()

	// Two collections with different prefixes sharing the same store.
	c1 := New[Todo](mem, "todos", WithPrefix("tenant1/"))
	c2 := New[Todo](mem, "todos", WithPrefix("tenant2/"))

	c1.Set(ctx, "t1", Todo{Title: "tenant1 task"})
	c2.Set(ctx, "t1", Todo{Title: "tenant2 task"})

	got1, _ := c1.Get(ctx, "t1")
	got2, _ := c2.Get(ctx, "t1")

	if got1.Title != "tenant1 task" {
		t.Fatalf("tenant1 got wrong value: %+v", got1)
	}
	if got2.Title != "tenant2 task" {
		t.Fatalf("tenant2 got wrong value: %+v", got2)
	}

	list1, _ := c1.List(ctx)
	list2, _ := c2.List(ctx)
	if len(list1) != 1 || len(list2) != 1 {
		t.Fatalf("expected 1 doc each, got %d and %d", len(list1), len(list2))
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

func TestMultipleCollections_SameDocID(t *testing.T) {
	mem := memory.New()
	todos := New[Todo](mem, "todos")
	profiles := New[Profile](mem, "profiles")
	ctx := context.Background()

	// Same ID in different collections should not collide.
	todos.Set(ctx, "id1", Todo{Title: "task"})
	profiles.Set(ctx, "id1", Profile{Name: "Alice"})

	todo, _ := todos.Get(ctx, "id1")
	profile, _ := profiles.Get(ctx, "id1")

	if todo.Title != "task" {
		t.Fatalf("todo wrong: %+v", todo)
	}
	if profile.Name != "Alice" {
		t.Fatalf("profile wrong: %+v", profile)
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

type OmitemptyTag struct {
	Name  string `json:"name,omitempty"`
	Count int    `json:"count,omitempty"`
}

func TestJsonTag_OmitemptyIgnoredForNaming(t *testing.T) {
	c, mem := newCollection[OmitemptyTag](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", OmitemptyTag{Name: "Alice", Count: 42})

	// The key should be "name", not "name,omitempty".
	v, err := mem.Get(ctx, "items/d1/name")
	if err != nil || v != `"Alice"` {
		t.Fatalf("name: got %q err=%v", v, err)
	}
	v, err = mem.Get(ctx, "items/d1/count")
	if err != nil || v != `42` {
		t.Fatalf("count: got %q err=%v", v, err)
	}
}

type UnexportedFields struct {
	Public  string `json:"public"`
	private string //nolint:unused
}

func TestUnexportedFieldsIgnored(t *testing.T) {
	c, mem := newCollection[UnexportedFields](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", UnexportedFields{Public: "hello"})

	// Only the public field + index key should exist.
	all, _ := mem.List(ctx, storemd.ListArgs{Prefix: "items/"})
	if len(all) != 2 { // 1 index + 1 field
		keys := make([]string, len(all))
		for i, kv := range all {
			keys[i] = kv.Key
		}
		t.Fatalf("expected 2 keys (idx + public), got %d: %v", len(all), keys)
	}
}

// ---------------------------------------------------------------------------
// Various field types
// ---------------------------------------------------------------------------

type TypedFields struct {
	Str    string  `json:"str"`
	Int    int     `json:"int"`
	Float  float64 `json:"float"`
	Bool   bool    `json:"bool"`
	Uint   uint64  `json:"uint"`
	Nested Inner   `json:"nested"`
}

type Inner struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func TestVariousFieldTypes(t *testing.T) {
	c, _ := newCollection[TypedFields](t, "typed")
	ctx := context.Background()

	doc := TypedFields{
		Str:    "hello",
		Int:    -42,
		Float:  3.14,
		Bool:   true,
		Uint:   999,
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

type SliceField struct {
	Tags []string `json:"tags"`
}

func TestSliceField(t *testing.T) {
	c, mem := newCollection[SliceField](t, "items")
	ctx := context.Background()

	doc := SliceField{Tags: []string{"go", "sync", "lww"}}
	c.Set(ctx, "d1", doc)

	// Slice is serialized as a single JSON value.
	v, _ := mem.Get(ctx, "items/d1/tags")
	if v != `["go","sync","lww"]` {
		t.Fatalf("expected JSON array, got %q", v)
	}

	got, _ := c.Get(ctx, "d1")
	if len(got.Tags) != 3 || got.Tags[0] != "go" || got.Tags[2] != "lww" {
		t.Fatalf("expected [go,sync,lww], got %v", got.Tags)
	}
}

type MapField struct {
	Meta map[string]string `json:"meta"`
}

func TestMapField(t *testing.T) {
	c, _ := newCollection[MapField](t, "items")
	ctx := context.Background()

	doc := MapField{Meta: map[string]string{"env": "prod", "region": "us"}}
	c.Set(ctx, "d1", doc)

	got, _ := c.Get(ctx, "d1")
	if got.Meta["env"] != "prod" || got.Meta["region"] != "us" {
		t.Fatalf("expected {env:prod, region:us}, got %v", got.Meta)
	}
}

type PointerField struct {
	Name  string  `json:"name"`
	Score *int    `json:"score"`
	Label *string `json:"label"`
}

func TestPointerField_NonNil(t *testing.T) {
	c, _ := newCollection[PointerField](t, "items")
	ctx := context.Background()

	score := 100
	label := "gold"
	doc := PointerField{Name: "Alice", Score: &score, Label: &label}
	c.Set(ctx, "d1", doc)

	got, _ := c.Get(ctx, "d1")
	if got.Name != "Alice" || got.Score == nil || *got.Score != 100 {
		t.Fatalf("expected Alice/100, got %+v", got)
	}
	if got.Label == nil || *got.Label != "gold" {
		t.Fatalf("expected label=gold, got %+v", got)
	}
}

func TestPointerField_Nil(t *testing.T) {
	c, _ := newCollection[PointerField](t, "items")
	ctx := context.Background()

	doc := PointerField{Name: "Bob", Score: nil, Label: nil}
	c.Set(ctx, "d1", doc)

	got, _ := c.Get(ctx, "d1")
	if got.Name != "Bob" {
		t.Fatalf("expected Bob, got %q", got.Name)
	}
	if got.Score != nil || got.Label != nil {
		t.Fatalf("expected nil pointers, got Score=%v Label=%v", got.Score, got.Label)
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

func TestNew_PanicsOnPointerToStruct(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for pointer-to-struct type")
		}
	}()
	New[*Todo](memory.New(), "bad")
}

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

func TestDocIDWithColons(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	id := "user:alice:task:1"
	c.Set(ctx, id, Todo{Title: "colon id"})

	got, _ := c.Get(ctx, id)
	if got.Title != "colon id" {
		t.Fatalf("expected colon id, got %q", got.Title)
	}
}

func TestDocIDWithDots(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	id := "com.example.task.1"
	c.Set(ctx, id, Todo{Title: "dotted"})

	got, _ := c.Get(ctx, id)
	if got.Title != "dotted" {
		t.Fatalf("expected dotted, got %q", got.Title)
	}
}

func TestLargeNumberOfDocuments(t *testing.T) {
	c, _ := newCollection[Todo](t, "todos")
	ctx := context.Background()

	n := 100
	for i := range n {
		id := strings.Repeat("0", 3-len(itoa(i))) + itoa(i) // zero-padded: 000, 001, ..., 099
		c.Set(ctx, id, Todo{Title: id, Order: i})
	}

	docs, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != n {
		t.Fatalf("expected %d docs, got %d", n, len(docs))
	}

	// Verify lexicographic order.
	for i := 1; i < len(docs); i++ {
		if docs[i].ID <= docs[i-1].ID {
			t.Fatalf("docs not in order at %d: %q <= %q", i, docs[i].ID, docs[i-1].ID)
		}
	}

	// Verify pagination still works.
	page, _ := c.List(ctx, ListArgs{Limit: 10})
	if len(page) != 10 {
		t.Fatalf("expected page of 10, got %d", len(page))
	}
	if page[0].ID != "000" || page[9].ID != "009" {
		t.Fatalf("expected 000-009, got %s-%s", page[0].ID, page[9].ID)
	}
}

// itoa is a minimal int-to-string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

type EmptyStruct struct{}

func TestEmptyStruct(t *testing.T) {
	c, mem := newCollection[EmptyStruct](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", EmptyStruct{})

	// Should only have the index key.
	all, _ := mem.List(ctx, storemd.ListArgs{Prefix: "items/"})
	if len(all) != 1 {
		t.Fatalf("expected 1 key (index only), got %d", len(all))
	}

	got, err := c.Get(ctx, "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != (EmptyStruct{}) {
		t.Fatalf("expected empty struct, got %+v", got)
	}

	docs, _ := c.List(ctx)
	if len(docs) != 1 || docs[0].ID != "d1" {
		t.Fatalf("expected 1 doc, got %v", docs)
	}
}

type SingleField struct {
	Value string `json:"value"`
}

func TestSingleFieldStruct(t *testing.T) {
	c, _ := newCollection[SingleField](t, "items")
	ctx := context.Background()

	c.Set(ctx, "d1", SingleField{Value: "only"})

	got, _ := c.Get(ctx, "d1")
	if got.Value != "only" {
		t.Fatalf("expected only, got %q", got.Value)
	}
}
