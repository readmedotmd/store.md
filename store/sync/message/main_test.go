package message

import (
	"context"
	"encoding/json"
	"fmt"
	gosync "sync"
	"testing"
	"time"

	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/backend/memory"
	"github.com/readmedotmd/store.md/sync/core"
)

func newTestSyncStore(t *testing.T) *core.StoreSync {
	t.Helper()
	return core.New(memory.New(), int64(100*time.Millisecond))
}

func newTestMessageStore(t *testing.T, ss *core.StoreSync, id string) *StoreMessage {
	t.Helper()
	m := New(ss, id)
	t.Cleanup(func() { m.Close() })
	return m
}

func TestStoreMessageImplementsStoreInterface(t *testing.T) {
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		ss := newTestSyncStore(t)
		return newTestMessageStore(t, ss, "test")
	})
}

func TestSendAndHandle_BasicRoundTrip(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong:" + msg.Data, nil
	})

	ctx := context.Background()
	resp, err := a.Send(ctx, "b", "ping", "hello")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if resp != "pong:hello" {
		t.Fatalf("expected %q, got %q", "pong:hello", resp)
	}
}

func TestSend_Timeout(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	// No store "b" exists, so no handler will respond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := a.Send(ctx, "nobody", "ping", "hello")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestSend_HandlerReturnsError(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("fail", func(msg Envelope) (string, error) {
		return "", fmt.Errorf("something went wrong")
	})

	ctx := context.Background()
	_, err := a.Send(ctx, "b", "fail", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "something went wrong" {
		t.Fatalf("expected %q, got %q", "something went wrong", err.Error())
	}
}

func TestSend_NoHandler(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	_ = newTestMessageStore(t, ss, "b") // b exists but has no handlers

	ctx := context.Background()
	_, err := a.Send(ctx, "b", "unknown", "")
	if err == nil {
		t.Fatal("expected error for missing handler, got nil")
	}
}

func TestOnMessage_FiresForIncomingRequests(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	var received []Envelope
	b.OnMessage(func(msg Envelope) {
		received = append(received, msg)
	})
	b.Handle("greet", func(msg Envelope) (string, error) {
		return "hi", nil
	})

	ctx := context.Background()
	a.Send(ctx, "b", "greet", "hello")

	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if received[0].Type != "greet" || received[0].Data != "hello" {
		t.Fatalf("unexpected message: %+v", received[0])
	}
}

func TestOnMessage_DoesNotFireForRegularData(t *testing.T) {
	ss := newTestSyncStore(t)
	m := newTestMessageStore(t, ss, "a")

	var count int
	m.OnMessage(func(msg Envelope) {
		count++
	})

	m.Set(context.Background(), "normalkey", "value")

	if count != 0 {
		t.Fatalf("expected 0 message listener calls for regular data, got %d", count)
	}
}

func TestOnMessage_Unsubscribe(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	var count int
	unsub := b.OnMessage(func(msg Envelope) {
		count++
	})
	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	ctx := context.Background()
	a.Send(ctx, "b", "ping", "1")
	if count != 1 {
		t.Fatalf("expected 1, got %d", count)
	}

	unsub()

	a.Send(ctx, "b", "ping", "2")
	if count != 1 {
		t.Fatalf("expected still 1 after unsubscribe, got %d", count)
	}
}

func TestMultipleHandlerTypes(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("add", func(msg Envelope) (string, error) {
		return "added:" + msg.Data, nil
	})
	b.Handle("sub", func(msg Envelope) (string, error) {
		return "subtracted:" + msg.Data, nil
	})

	ctx := context.Background()
	r1, err := a.Send(ctx, "b", "add", "5")
	if err != nil {
		t.Fatalf("Send add failed: %v", err)
	}
	if r1 != "added:5" {
		t.Fatalf("expected %q, got %q", "added:5", r1)
	}

	r2, err := a.Send(ctx, "b", "sub", "3")
	if err != nil {
		t.Fatalf("Send sub failed: %v", err)
	}
	if r2 != "subtracted:3" {
		t.Fatalf("expected %q, got %q", "subtracted:3", r2)
	}
}

func TestConcurrentSends(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("echo", func(msg Envelope) (string, error) {
		return msg.Data, nil
	})

	ctx := context.Background()
	n := 10
	results := make([]string, n)
	errs := make([]error, n)
	var wg gosync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := fmt.Sprintf("msg-%d", i)
			results[i], errs[i] = a.Send(ctx, "b", "echo", data)
		}(i)
	}

	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Send %d failed: %v", i, errs[i])
		}
		expected := fmt.Sprintf("msg-%d", i)
		if results[i] != expected {
			t.Fatalf("Send %d: expected %q, got %q", i, expected, results[i])
		}
	}
}

func TestClose_CancelsInFlightSends(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	// No target "b" exists

	done := make(chan error, 1)
	go func() {
		_, err := a.Send(context.Background(), "nobody", "ping", "")
		done <- err
	}()

	// Give Send time to register the pending channel and write
	time.Sleep(10 * time.Millisecond)
	a.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after close, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not unblock after Close")
	}
}

func TestSyncRoundTrip_CrossStore(t *testing.T) {
	ss1 := newTestSyncStore(t)
	ss2 := newTestSyncStore(t)
	a := newTestMessageStore(t, ss1, "a")
	b := newTestMessageStore(t, ss2, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	// A sends a message — it writes to ss1
	env := Envelope{
		MessageID: "cross-1",
		SenderID:  "a",
		Type:      "ping",
		Data:      "hello",
	}
	encoded, _ := json.Marshal(env)
	ctx := context.Background()
	ss1.SetItem(ctx, "msg", reqKey("b", "cross-1"), string(encoded))

	// Register pending channel on A
	ch := make(chan Envelope, 1)
	a.pendingMu.Lock()
	a.pending["cross-1"] = ch
	a.pendingMu.Unlock()

	// Sync ss1 -> ss2 (delivers the request to B)
	payload, err := ss1.SyncOut(ctx, "ss2", 0)
	if err != nil {
		t.Fatalf("SyncOut failed: %v", err)
	}
	if err := ss2.SyncIn(ctx, "ss1", *payload); err != nil {
		t.Fatalf("SyncIn failed: %v", err)
	}

	// B should have handled and written a response to ss2
	// Sync ss2 -> ss1 (delivers the response back to A)
	time.Sleep(150 * time.Millisecond) // wait for writeTimestamp to pass
	payload2, err := ss2.SyncOut(ctx, "ss1", 0)
	if err != nil {
		t.Fatalf("SyncOut 2 failed: %v", err)
	}
	if err := ss1.SyncIn(ctx, "ss2", *payload2); err != nil {
		t.Fatalf("SyncIn 2 failed: %v", err)
	}

	// A should receive the response
	select {
	case resp := <-ch:
		if resp.Data != "pong" {
			t.Fatalf("expected %q, got %q", "pong", resp.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("response not received")
	}
}

func TestGC_DeletesExpiredKeys(t *testing.T) {
	ss := newTestSyncStore(t)
	ttl := 50 * time.Millisecond
	a := New(ss, "a", WithTTL(ttl), WithGCInterval(20*time.Millisecond))
	t.Cleanup(func() { a.Close() })
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	ctx := context.Background()
	if _, err := a.Send(ctx, "b", "ping", "hello"); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Keys should exist immediately after Send.
	pairs, err := ss.List(ctx, storemd.ListArgs{Prefix: "%msg%"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected msg keys to exist before TTL expires")
	}

	// Wait for TTL + at least one GC tick.
	time.Sleep(ttl + 60*time.Millisecond)

	pairs, err = ss.List(ctx, storemd.ListArgs{Prefix: "%msg%"})
	if err != nil {
		t.Fatalf("List after GC failed: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("expected all msg keys to be deleted by GC, got %d", len(pairs))
	}
}

func TestGC_SkipsZeroCreatedAt(t *testing.T) {
	ss := newTestSyncStore(t)
	ttl := 20 * time.Millisecond
	a := New(ss, "a", WithTTL(ttl), WithGCInterval(10*time.Millisecond))
	t.Cleanup(func() { a.Close() })

	// Write a legacy envelope (no CreatedAt) directly.
	env := Envelope{MessageID: "legacy-1", SenderID: "x", Type: "ping", Data: "d"}
	encoded, _ := json.Marshal(env)
	ctx := context.Background()
	ss.SetItem(ctx, "msg", reqKey("a", "legacy-1"), string(encoded))

	// Wait past TTL.
	time.Sleep(ttl + 40*time.Millisecond)

	pairs, err := ss.List(ctx, storemd.ListArgs{Prefix: "%msg%"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected legacy key to be preserved (zero CreatedAt must not be GC'd)")
	}
}

func TestGC_OffByDefault(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a") // no options — GC off
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	ctx := context.Background()
	if _, err := a.Send(ctx, "b", "ping", "hello"); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	pairs, err := ss.List(ctx, storemd.ListArgs{Prefix: "%msg%"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected msg keys to persist when GC is off")
	}
}

func TestID(t *testing.T) {
	ss := newTestSyncStore(t)
	m := newTestMessageStore(t, ss, "my-store-id")

	if m.ID() != "my-store-id" {
		t.Fatalf("expected %q, got %q", "my-store-id", m.ID())
	}
}

func TestOnSendComplete_Hook(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	var mu gosync.Mutex
	var completedEnv Envelope
	var completedResponse string
	a.OnSendComplete(func(env Envelope, response string) string {
		mu.Lock()
		completedEnv = env
		completedResponse = response
		mu.Unlock()
		return response
	})

	ctx := context.Background()
	resp, err := a.Send(ctx, "b", "ping", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp != "pong" {
		t.Fatalf("expected pong, got %q", resp)
	}

	mu.Lock()
	if completedEnv.Type != "ping" {
		t.Fatalf("expected type ping, got %q", completedEnv.Type)
	}
	if completedResponse != "pong" {
		t.Fatalf("expected response pong, got %q", completedResponse)
	}
	mu.Unlock()
}

func TestOnSendError_Hook(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("fail", func(msg Envelope) (string, error) {
		return "", fmt.Errorf("handler error")
	})

	var mu gosync.Mutex
	var errorEnv Envelope
	var sendErr error
	a.OnSendError(func(env Envelope, err error) error {
		mu.Lock()
		errorEnv = env
		sendErr = err
		mu.Unlock()
		return err
	})

	ctx := context.Background()
	_, err := a.Send(ctx, "b", "fail", "data")
	if err == nil {
		t.Fatal("expected error")
	}

	mu.Lock()
	if errorEnv.Type != "fail" {
		t.Fatalf("expected type fail, got %q", errorEnv.Type)
	}
	if sendErr == nil {
		t.Fatal("expected error in OnSendError")
	}
	mu.Unlock()
}

func TestOnSendComplete_TransformsResponse(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	// Hook transforms the response.
	a.OnSendComplete(func(env Envelope, response string) string {
		return "transformed:" + response
	})

	ctx := context.Background()
	resp, err := a.Send(ctx, "b", "ping", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp != "transformed:pong" {
		t.Fatalf("expected transformed:pong, got %q", resp)
	}
}

func TestOnSendComplete_ChainsMultipleHooks(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	// Two hooks chaining transformations.
	a.OnSendComplete(func(env Envelope, response string) string {
		return "[1:" + response + "]"
	})
	a.OnSendComplete(func(env Envelope, response string) string {
		return "[2:" + response + "]"
	})

	ctx := context.Background()
	resp, err := a.Send(ctx, "b", "ping", "hello")
	if err != nil {
		t.Fatal(err)
	}
	// Second hook receives output of first.
	if resp != "[2:[1:pong]]" {
		t.Fatalf("expected chained transformation, got %q", resp)
	}
}

func TestOnSendError_SuppressesError(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("fail", func(msg Envelope) (string, error) {
		return "", fmt.Errorf("handler error")
	})

	// Hook suppresses the error by returning nil.
	a.OnSendError(func(env Envelope, err error) error {
		return nil
	})

	ctx := context.Background()
	_, err := a.Send(ctx, "b", "fail", "data")
	if err != nil {
		t.Fatalf("expected error to be suppressed, got: %v", err)
	}
}

func TestOnSendError_TransformsError(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("fail", func(msg Envelope) (string, error) {
		return "", fmt.Errorf("original error")
	})

	// Hook transforms the error.
	a.OnSendError(func(env Envelope, err error) error {
		return fmt.Errorf("wrapped: %w", err)
	})

	ctx := context.Background()
	_, err := a.Send(ctx, "b", "fail", "data")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "wrapped: original error" {
		t.Fatalf("expected wrapped error, got %q", err.Error())
	}
}

func TestOnSendComplete_Unsubscribe(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("ping", func(msg Envelope) (string, error) {
		return "pong", nil
	})

	unsub := a.OnSendComplete(func(env Envelope, response string) string {
		return "modified"
	})

	ctx := context.Background()
	resp1, _ := a.Send(ctx, "b", "ping", "1")
	if resp1 != "modified" {
		t.Fatalf("expected modified, got %q", resp1)
	}

	unsub()

	resp2, _ := a.Send(ctx, "b", "ping", "2")
	if resp2 != "pong" {
		t.Fatalf("expected pong after unsubscribe, got %q", resp2)
	}
}

func TestOnSendError_Unsubscribe(t *testing.T) {
	ss := newTestSyncStore(t)
	a := newTestMessageStore(t, ss, "a")
	b := newTestMessageStore(t, ss, "b")

	b.Handle("fail", func(msg Envelope) (string, error) {
		return "", fmt.Errorf("handler error")
	})

	unsub := a.OnSendError(func(env Envelope, err error) error {
		return nil // suppress
	})

	ctx := context.Background()
	_, err1 := a.Send(ctx, "b", "fail", "1")
	if err1 != nil {
		t.Fatalf("expected suppressed error, got %v", err1)
	}

	unsub()

	_, err2 := a.Send(ctx, "b", "fail", "2")
	if err2 == nil {
		t.Fatal("expected error after unsubscribe")
	}
}
