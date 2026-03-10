package storemd

import (
	"testing"
)

// StoreFactory is a function that returns a fresh Store instance for testing.
type StoreFactory func(t *testing.T) Store

// RunStoreTests runs the full generic test suite against any Store implementation.
func RunStoreTests(t *testing.T, factory StoreFactory) {
	t.Run("Get_NotFound", func(t *testing.T) {
		s := factory(t)
		_, err := s.Get("nonexistent")
		if err != NotFoundError {
			t.Fatalf("expected NotFoundError, got %v", err)
		}
	})

	t.Run("SetAndGet", func(t *testing.T) {
		s := factory(t)
		if err := s.Set("key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		val, err := s.Get("key1")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "value1" {
			t.Fatalf("expected %q, got %q", "value1", val)
		}
	})

	t.Run("Set_Overwrite", func(t *testing.T) {
		s := factory(t)
		if err := s.Set("key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if err := s.Set("key1", "value2"); err != nil {
			t.Fatalf("Set overwrite failed: %v", err)
		}
		val, err := s.Get("key1")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "value2" {
			t.Fatalf("expected %q, got %q", "value2", val)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		s := factory(t)
		if err := s.Set("key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if err := s.Delete("key1"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		_, err := s.Get("key1")
		if err != NotFoundError {
			t.Fatalf("expected NotFoundError after delete, got %v", err)
		}
	})

	t.Run("Delete_NotFound", func(t *testing.T) {
		s := factory(t)
		err := s.Delete("nonexistent")
		if err != NotFoundError {
			t.Fatalf("expected NotFoundError, got %v", err)
		}
	})

	t.Run("List_Empty", func(t *testing.T) {
		s := factory(t)
		result, err := s.List(ListArgs{})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 0 {
			t.Fatalf("expected empty list, got %d items", len(result))
		}
	})

	t.Run("List_All", func(t *testing.T) {
		s := factory(t)
		pairs := []KeyValuePair{
			{"a/1", "v1"},
			{"a/2", "v2"},
			{"b/1", "v3"},
		}
		for _, p := range pairs {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 3 {
			t.Fatalf("expected 3 items, got %d", len(result))
		}
	})

	t.Run("List_Prefix", func(t *testing.T) {
		s := factory(t)
		for _, p := range []KeyValuePair{
			{"a/1", "v1"},
			{"a/2", "v2"},
			{"b/1", "v3"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{Prefix: "a/"})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 items with prefix 'a/', got %d", len(result))
		}
		for _, r := range result {
			if r.Key != "a/1" && r.Key != "a/2" {
				t.Fatalf("unexpected key %q in prefix results", r.Key)
			}
		}
	})

	t.Run("List_StartAfter", func(t *testing.T) {
		s := factory(t)
		for _, p := range []KeyValuePair{
			{"a", "v1"},
			{"b", "v2"},
			{"c", "v3"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{StartAfter: "a"})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 items after 'a', got %d", len(result))
		}
		for _, r := range result {
			if r.Key <= "a" {
				t.Fatalf("expected keys after 'a', got %q", r.Key)
			}
		}
	})

	t.Run("List_Limit", func(t *testing.T) {
		s := factory(t)
		for _, p := range []KeyValuePair{
			{"a", "v1"},
			{"b", "v2"},
			{"c", "v3"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{Limit: 2})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 items, got %d", len(result))
		}
	})

	t.Run("List_Pagination", func(t *testing.T) {
		s := factory(t)
		for _, p := range []KeyValuePair{
			{"a", "v1"},
			{"b", "v2"},
			{"c", "v3"},
			{"d", "v4"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}

		// First page
		page1, err := s.List(ListArgs{Limit: 2})
		if err != nil {
			t.Fatalf("List page1 failed: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2 items in page1, got %d", len(page1))
		}

		// Second page using StartAfter with last key from page1
		lastKey := page1[len(page1)-1].Key
		page2, err := s.List(ListArgs{Limit: 2, StartAfter: lastKey})
		if err != nil {
			t.Fatalf("List page2 failed: %v", err)
		}
		if len(page2) != 2 {
			t.Fatalf("expected 2 items in page2, got %d", len(page2))
		}

		// Ensure no overlap
		for _, p1 := range page1 {
			for _, p2 := range page2 {
				if p1.Key == p2.Key {
					t.Fatalf("overlapping key %q between pages", p1.Key)
				}
			}
		}
	})

	t.Run("List_OrderedByKey", func(t *testing.T) {
		s := factory(t)
		// Insert out of order
		for _, p := range []KeyValuePair{
			{"c", "v3"},
			{"a", "v1"},
			{"b", "v2"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		for i := 1; i < len(result); i++ {
			if result[i].Key <= result[i-1].Key {
				t.Fatalf("results not sorted: %q should come after %q", result[i].Key, result[i-1].Key)
			}
		}
	})

	t.Run("List_PrefixWithStartAfterAndLimit", func(t *testing.T) {
		s := factory(t)
		for _, p := range []KeyValuePair{
			{"x/a", "v1"},
			{"x/b", "v2"},
			{"x/c", "v3"},
			{"x/d", "v4"},
			{"y/a", "v5"},
		} {
			if err := s.Set(p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ListArgs{Prefix: "x/", StartAfter: "x/b", Limit: 2})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 2 {
			t.Fatalf("expected 2 items, got %d", len(result))
		}
		if result[0].Key != "x/c" || result[1].Key != "x/d" {
			t.Fatalf("expected [x/c, x/d], got [%s, %s]", result[0].Key, result[1].Key)
		}
	})
}
