package storemd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// StoreFactory is a function that returns a fresh Store instance for testing.
type StoreFactory func(t *testing.T) Store

// Clearable is implemented by backends that support clearing all data.
type Clearable interface {
	Store
	Clear(ctx context.Context) error
}

// ClearableFactory is a function that returns a fresh Clearable instance for testing.
type ClearableFactory func(t *testing.T) Clearable

// RunClearTests runs tests for the Clear method on backends that support it.
func RunClearTests(t *testing.T, factory ClearableFactory) {
	ctx := context.Background()

	t.Run("Clear", func(t *testing.T) {
		s := factory(t)
		// Insert some data.
		for _, p := range []KeyValuePair{
			{"a", "v1"},
			{"b", "v2"},
			{"c", "v3"},
		} {
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		// Clear all data.
		if err := s.Clear(ctx); err != nil {
			t.Fatalf("Clear failed: %v", err)
		}
		// Verify store is empty.
		result, err := s.List(ctx, ListArgs{})
		if err != nil {
			t.Fatalf("List after Clear failed: %v", err)
		}
		if len(result) != 0 {
			t.Fatalf("expected empty store after Clear, got %d items", len(result))
		}
	})

	t.Run("Clear_Empty", func(t *testing.T) {
		s := factory(t)
		// Clear on an already-empty store should not error.
		if err := s.Clear(ctx); err != nil {
			t.Fatalf("Clear on empty store failed: %v", err)
		}
	})

	t.Run("Clear_ThenReuse", func(t *testing.T) {
		s := factory(t)
		// Insert, clear, then insert again.
		if err := s.Set(ctx, "key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if err := s.Clear(ctx); err != nil {
			t.Fatalf("Clear failed: %v", err)
		}
		if err := s.Set(ctx, "key2", "value2"); err != nil {
			t.Fatalf("Set after Clear failed: %v", err)
		}
		val, err := s.Get(ctx, "key2")
		if err != nil {
			t.Fatalf("Get after Clear failed: %v", err)
		}
		if val != "value2" {
			t.Fatalf("expected %q, got %q", "value2", val)
		}
		// Original key should still be gone.
		_, err = s.Get(ctx, "key1")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for cleared key, got %v", err)
		}
	})
}

// RunSetIfNotExistsTests runs tests for the SetIfNotExists method.
func RunSetIfNotExistsTests(t *testing.T, factory StoreFactory) {
	ctx := context.Background()

	t.Run("SetIfNotExists_Basic", func(t *testing.T) {
		s := factory(t)
		ok, err := s.SetIfNotExists(ctx, "newkey", "value1")
		if err != nil {
			t.Fatalf("SetIfNotExists failed: %v", err)
		}
		if !ok {
			t.Fatal("expected true for new key, got false")
		}
		val, err := s.Get(ctx, "newkey")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "value1" {
			t.Fatalf("expected %q, got %q", "value1", val)
		}
	})

	t.Run("SetIfNotExists_Duplicate", func(t *testing.T) {
		s := factory(t)
		ok, err := s.SetIfNotExists(ctx, "dupkey", "first")
		if err != nil {
			t.Fatalf("first SetIfNotExists failed: %v", err)
		}
		if !ok {
			t.Fatal("expected true for first call")
		}

		ok, err = s.SetIfNotExists(ctx, "dupkey", "second")
		if err != nil {
			t.Fatalf("second SetIfNotExists failed: %v", err)
		}
		if ok {
			t.Fatal("expected false for duplicate key, got true")
		}

		val, err := s.Get(ctx, "dupkey")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "first" {
			t.Fatalf("expected original value %q, got %q", "first", val)
		}
	})

	t.Run("SetIfNotExists_Concurrent", func(t *testing.T) {
		s := factory(t)
		const n = 10
		results := make(chan bool, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := s.SetIfNotExists(ctx, "race-key", fmt.Sprintf("writer-%d", i))
				if err != nil {
					t.Errorf("SetIfNotExists from goroutine %d failed: %v", i, err)
					return
				}
				results <- ok
			}(i)
		}
		wg.Wait()
		close(results)

		wins := 0
		for ok := range results {
			if ok {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("expected exactly 1 winner, got %d", wins)
		}

		// Verify the key is readable.
		_, err := s.Get(ctx, "race-key")
		if err != nil {
			t.Fatalf("Get after concurrent SetIfNotExists failed: %v", err)
		}
	})

	t.Run("SetIfNotExists_DifferentKeys", func(t *testing.T) {
		s := factory(t)
		for i := range 5 {
			key := fmt.Sprintf("diffkey-%d", i)
			ok, err := s.SetIfNotExists(ctx, key, fmt.Sprintf("val-%d", i))
			if err != nil {
				t.Fatalf("SetIfNotExists %q failed: %v", key, err)
			}
			if !ok {
				t.Fatalf("expected true for unique key %q", key)
			}
		}
		// Verify all are readable.
		for i := range 5 {
			key := fmt.Sprintf("diffkey-%d", i)
			val, err := s.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get %q failed: %v", key, err)
			}
			expected := fmt.Sprintf("val-%d", i)
			if val != expected {
				t.Fatalf("key %q: expected %q, got %q", key, expected, val)
			}
		}
	})
}

// RunStoreTests runs the full generic test suite against any Store implementation.
func RunStoreTests(t *testing.T, factory StoreFactory) {
	ctx := context.Background()

	t.Run("Get_NotFound", func(t *testing.T) {
		s := factory(t)
		_, err := s.Get(ctx, "nonexistent")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("SetAndGet", func(t *testing.T) {
		s := factory(t)
		if err := s.Set(ctx, "key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		val, err := s.Get(ctx, "key1")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "value1" {
			t.Fatalf("expected %q, got %q", "value1", val)
		}
	})

	t.Run("Set_Overwrite", func(t *testing.T) {
		s := factory(t)
		if err := s.Set(ctx, "key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if err := s.Set(ctx, "key1", "value2"); err != nil {
			t.Fatalf("Set overwrite failed: %v", err)
		}
		val, err := s.Get(ctx, "key1")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if val != "value2" {
			t.Fatalf("expected %q, got %q", "value2", val)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		s := factory(t)
		if err := s.Set(ctx, "key1", "value1"); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if err := s.Delete(ctx, "key1"); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		_, err := s.Get(ctx, "key1")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound after delete, got %v", err)
		}
	})

	t.Run("Delete_NotFound", func(t *testing.T) {
		s := factory(t)
		err := s.Delete(ctx, "nonexistent")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("List_Empty", func(t *testing.T) {
		s := factory(t)
		result, err := s.List(ctx, ListArgs{})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{Prefix: "a/"})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{StartAfter: "a"})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{Limit: 2})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}

		// First page
		page1, err := s.List(ctx, ListArgs{Limit: 2})
		if err != nil {
			t.Fatalf("List page1 failed: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2 items in page1, got %d", len(page1))
		}

		// Second page using StartAfter with last key from page1
		lastKey := page1[len(page1)-1].Key
		page2, err := s.List(ctx, ListArgs{Limit: 2, StartAfter: lastKey})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{})
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
			if err := s.Set(ctx, p.Key, p.Value); err != nil {
				t.Fatalf("Set %q failed: %v", p.Key, err)
			}
		}
		result, err := s.List(ctx, ListArgs{Prefix: "x/", StartAfter: "x/b", Limit: 2})
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

	t.Run("Concurrent_ReadWrite", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := fmt.Sprintf("concurrent-%d", i)
				if err := s.Set(ctx, key, "value"); err != nil {
					t.Errorf("Set %q failed: %v", key, err)
				}
				if _, err := s.Get(ctx, key); err != nil {
					t.Errorf("Get %q failed: %v", key, err)
				}
			}()
		}
		wg.Wait()
		result, err := s.List(ctx, ListArgs{Prefix: "concurrent-"})
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(result) != 10 {
			t.Fatalf("expected 10 items, got %d", len(result))
		}
	})

	t.Run("EmptyKey", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.Set(ctx, "", "value"); err != nil {
			t.Skipf("backend does not support empty keys: %v", err)
		}
		val, err := s.Get(ctx, "")
		if err != nil {
			t.Fatalf("Get empty key failed: %v", err)
		}
		if val != "value" {
			t.Fatalf("expected %q, got %q", "value", val)
		}
	})

	t.Run("SpecialCharacters", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		keys := []string{"key/with/slashes", "key with spaces", "key%percent", "key🔑emoji", "a/b/c/d/e"}
		for _, key := range keys {
			if err := s.Set(ctx, key, "v"); err != nil {
				t.Errorf("Set %q failed: %v", key, err)
				continue
			}
			val, err := s.Get(ctx, key)
			if err != nil {
				t.Errorf("Get %q failed: %v", key, err)
				continue
			}
			if val != "v" {
				t.Errorf("key %q: expected %q, got %q", key, "v", val)
			}
		}
	})

	t.Run("LargeValue", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		large := string(make([]byte, 1024*1024)) // 1MB of null bytes
		if err := s.Set(ctx, "large", large); err != nil {
			t.Skipf("backend does not support 1MB values: %v", err)
		}
		val, err := s.Get(ctx, "large")
		if err != nil {
			t.Fatalf("Get large value failed: %v", err)
		}
		if len(val) != len(large) {
			t.Fatalf("expected %d bytes, got %d", len(large), len(val))
		}
	})
}
