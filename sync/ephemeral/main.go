// Package ephemeral provides targeted, non-persistent message passing built on
// top of StoreSync hooks. It is designed for fire-and-forget communication where
// messages should exist in as few places as possible and for as short a time as
// possible.
//
// # Properties
//
//   - Point-to-point: each message targets a specific peer by ID.
//   - Never persisted on the receiver: the SyncInFilter hook dispatches to an
//     in-memory handler and skips the normal setItem persistence path.
//   - Deleted after delivery: the PostSyncOut hook removes the item from the
//     sender's underlying store once SyncOut has included it in an outgoing payload.
//   - Offline-first: the StoreSync queue acts as a durable outbox — if the sender
//     is disconnected, messages persist locally until the target peer syncs.
//   - Deduplication: a bounded seen-set (configurable via WithSeenCapacity)
//     prevents duplicate handler invocations if a message is re-delivered.
//
// # Key convention
//
// Ephemeral messages use the key prefix "%eph%" followed by the target peer ID
// and a unique message ID: %eph%{targetPeerID}%{messageID}. This convention
// is parsed by the hook callbacks to determine routing and cleanup behavior.
// Non-ephemeral keys (those without the %eph% prefix) pass through all hooks
// unmodified.
//
// # Backing store
//
// The choice of backing store controls durability. Use an in-memory store
// (e.g. memory.New()) for truly ephemeral, no-disk messaging — messages
// live only in RAM until delivered. Use a persistent store (e.g. SQL) when
// the sender's outbox must survive process restarts.
//
// # Setup
//
// The Layer must be wired into a StoreSync in two phases because hooks are
// registered at construction time via functional options:
//
//	eph := ephemeral.New("my-node-id")
//	ss := core.NewWithOptions(store, eph.Options()...)
//	eph.Bind(ss)
//
// # Usage
//
//	// Sender side
//	eph.Send(ctx, "target-node", "chat", "hello world")
//
//	// Receiver side
//	eph.Handle("chat", func(msg ephemeral.Envelope) {
//	    fmt.Printf("got %q from %s\n", msg.Data, msg.From)
//	})
package ephemeral

import (
	"context"
	"encoding/json"
	"strings"
	gosync "sync"
	"time"

	"github.com/google/uuid"
	"github.com/readmedotmd/store.md/sync/core"
)

const keyPrefix = "%eph%"

func ephKey(targetPeerID, messageID string) string {
	return keyPrefix + targetPeerID + "%" + messageID
}

// parseEphKey extracts the target peer ID and message ID from an ephemeral key.
func parseEphKey(key string) (targetPeerID, messageID string, ok bool) {
	if !strings.HasPrefix(key, keyPrefix) {
		return "", "", false
	}
	rest := key[len(keyPrefix):]
	idx := strings.Index(rest, "%")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// Envelope is the payload delivered to handlers.
type Envelope struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	MsgType   string `json:"msgType"`
	Data      string `json:"data"`
	CreatedAt int64  `json:"createdAt"`
}

// Handler processes an incoming ephemeral message.
type Handler func(msg Envelope)

// Option configures a Layer.
type Option func(*Layer)

// WithSeenCapacity sets the size of the deduplication cache. Default is 10000.
func WithSeenCapacity(n int) Option {
	return func(l *Layer) { l.seenCap = n }
}

// Layer provides ephemeral (non-persisted) targeted message passing on top
// of StoreSync. Install its hooks via Options() when constructing the StoreSync,
// then call Bind() to complete setup.
type Layer struct {
	ss     *core.StoreSync
	nodeID string

	handlersMu gosync.RWMutex
	handlers   map[string]Handler

	seenMu    gosync.Mutex
	seen      map[string]struct{}
	seenOrder []string
	seenCap   int
}

// New creates an ephemeral Layer for the given node ID.
// Call Options() to get the core.Option hooks, pass them to
// core.NewWithOptions, then call Bind() with the resulting StoreSync.
func New(nodeID string, opts ...Option) *Layer {
	l := &Layer{
		nodeID:   nodeID,
		handlers: make(map[string]Handler),
		seen:     make(map[string]struct{}),
		seenCap:  10000,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Options returns the core.Option values that wire this layer's hooks into
// a StoreSync. Pass these when calling core.NewWithOptions.
func (l *Layer) Options() []core.Option {
	return []core.Option{
		core.WithSyncOutFilter(l.syncOutFilter),
		core.WithSyncInFilter(l.syncInFilter),
		core.WithPostSyncOut(l.postSyncOut),
	}
}

// Bind sets the StoreSync reference after construction. This must be called
// after core.NewWithOptions with the Options() returned by this Layer, and
// before calling Send. Bind completes the two-phase initialization required
// because hooks are registered at StoreSync construction time.
func (l *Layer) Bind(ss *core.StoreSync) {
	l.ss = ss
}

// Handle registers a handler for the given message type.
func (l *Layer) Handle(msgType string, fn Handler) {
	l.handlersMu.Lock()
	l.handlers[msgType] = fn
	l.handlersMu.Unlock()
}

// Send writes an ephemeral message targeted at a specific peer. The message
// enters the sender's sync queue and is delivered on the next sync cycle.
// If the peer is offline, the message remains in the queue (backed by
// whatever store underlies the StoreSync) until the peer connects.
// After delivery, the PostSyncOut hook deletes it from the sender's store.
func (l *Layer) Send(ctx context.Context, targetNode, msgType, data string) error {
	messageID := uuid.New().String()
	env := Envelope{
		ID:        messageID,
		From:      l.nodeID,
		MsgType:   msgType,
		Data:      data,
		CreatedAt: time.Now().UnixNano(),
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return l.ss.SetItem(ctx, "eph", ephKey(targetNode, messageID), string(encoded))
}

// syncOutFilter only includes ephemeral items when syncing to the target peer.
// Non-ephemeral items always pass through.
func (l *Layer) syncOutFilter(item core.SyncStoreItem, peerID string) bool {
	target, _, ok := parseEphKey(item.Key)
	if !ok {
		return true // not ephemeral, include normally
	}
	return target == peerID
}

// syncInFilter prevents ephemeral items from being persisted on the receiver.
// Instead, it dispatches to the registered handler directly.
func (l *Layer) syncInFilter(item core.SyncStoreItem, peerID string) bool {
	_, messageID, ok := parseEphKey(item.Key)
	if !ok {
		return true // not ephemeral, persist normally
	}

	// Dedup check
	l.seenMu.Lock()
	if _, dup := l.seen[messageID]; dup {
		l.seenMu.Unlock()
		return false
	}
	l.seen[messageID] = struct{}{}
	l.seenOrder = append(l.seenOrder, messageID)
	// Evict oldest if over capacity
	for len(l.seenOrder) > l.seenCap {
		oldest := l.seenOrder[0]
		l.seenOrder = l.seenOrder[1:]
		delete(l.seen, oldest)
	}
	l.seenMu.Unlock()

	var env Envelope
	if err := json.Unmarshal([]byte(item.Value), &env); err != nil {
		return false
	}

	l.handlersMu.RLock()
	handler, exists := l.handlers[env.MsgType]
	l.handlersMu.RUnlock()

	if exists {
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Handler panicked — silently recover to protect the sync cycle.
					_ = r
				}
			}()
			handler(env)
		}()
	}

	return false // never persist ephemeral items
}

// postSyncOut deletes delivered ephemeral items from the sender's store.
func (l *Layer) postSyncOut(ctx context.Context, items []core.SyncStoreItem, peerID string) {
	raw := l.ss.RawStore()
	for _, item := range items {
		if _, _, ok := parseEphKey(item.Key); !ok {
			continue
		}
		// Delete directly from underlying store — no tombstone, no sync queue entry.
		raw.Delete(ctx, core.ViewKey(item.Key))
		raw.Delete(ctx, core.ValueKey(item.ID))
		queueID := core.QueueID(item.WriteTimestamp, item.ID, item.Key)
		raw.Delete(ctx, core.QueueKey(queueID))
	}
}
