package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"

	"github.com/google/uuid"
	storemd "github.com/readmedotmd/store.md"
	"github.com/readmedotmd/store.md/sync/core"
)

// Envelope is the wire format for messages and responses.
type Envelope struct {
	MessageID string `json:"messageID"`
	SenderID  string `json:"senderID"`
	Type      string `json:"type"`
	Data      string `json:"data"`
	Error     string `json:"error,omitempty"`
}

// Handler processes an incoming message and returns a response payload.
type Handler func(msg Envelope) (responseData string, err error)

// MessageListener is a callback invoked when a message addressed to this store arrives.
type MessageListener func(msg Envelope)

func reqKey(targetID, messageID string) string {
	return "%msg%req%" + targetID + "%" + messageID
}

func resKey(messageID string) string {
	return "%msg%res%" + messageID
}

const (
	reqPrefix = "%msg%req%"
	resPrefix = "%msg%res%"
)

// StoreMessage adds request/response messaging on top of a sync store.
type StoreMessage struct {
	ss *core.StoreSync
	id string

	handlersMu gosync.RWMutex
	handlers   map[string]Handler

	pendingMu gosync.Mutex
	pending   map[string]chan Envelope

	listenersMu   gosync.RWMutex
	listeners      map[uint64]MessageListener
	nextListenerID atomic.Uint64

	unsub func()
}

// New creates a StoreMessage with the given unique ID.
func New(syncStore *core.StoreSync, id string) *StoreMessage {
	m := &StoreMessage{
		ss:        syncStore,
		id:        id,
		handlers:  make(map[string]Handler),
		pending:   make(map[string]chan Envelope),
		listeners: make(map[uint64]MessageListener),
	}
	m.unsub = syncStore.OnUpdate(m.onSyncUpdate)
	return m
}

// ID returns this store's unique identifier.
func (m *StoreMessage) ID() string {
	return m.id
}

// SyncStore returns the underlying StoreSync.
func (m *StoreMessage) SyncStore() *core.StoreSync {
	return m.ss
}

// Close unsubscribes from the sync store and cancels any pending sends.
func (m *StoreMessage) Close() error {
	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}
	m.pendingMu.Lock()
	for id, ch := range m.pending {
		close(ch)
		delete(m.pending, id)
	}
	m.pendingMu.Unlock()
	return nil
}

// --- Store interface ---

func (m *StoreMessage) Get(ctx context.Context, key string) (string, error)                              { return m.ss.Get(ctx, key) }
func (m *StoreMessage) Set(ctx context.Context, key, value string) error                                 { return m.ss.Set(ctx, key, value) }
func (m *StoreMessage) Delete(ctx context.Context, key string) error                                     { return m.ss.Delete(ctx, key) }
func (m *StoreMessage) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error)  { return m.ss.List(ctx, args) }

// --- Sync store methods ---

func (m *StoreMessage) GetItem(ctx context.Context, key string) (*core.SyncStoreItem, error)               { return m.ss.GetItem(ctx, key) }
func (m *StoreMessage) SetItem(ctx context.Context, app, key, value string) error                          { return m.ss.SetItem(ctx, app, key, value) }
func (m *StoreMessage) ListItems(ctx context.Context, prefix, startAfter string, limit int) ([]core.SyncStoreItem, error) { return m.ss.ListItems(ctx, prefix, startAfter, limit) }
func (m *StoreMessage) OnUpdate(fn core.UpdateListener) func()                        { return m.ss.OnUpdate(fn) }
func (m *StoreMessage) Sync(ctx context.Context, peerID string, incoming *core.SyncPayload) (*core.SyncPayload, error) { return m.ss.Sync(ctx, peerID, incoming) }

// --- Messaging API ---

// Handle registers a handler for the given message type.
func (m *StoreMessage) Handle(msgType string, handler Handler) {
	m.handlersMu.Lock()
	m.handlers[msgType] = handler
	m.handlersMu.Unlock()
}

// OnMessage registers a listener that fires for incoming requests only.
// Returns an unsubscribe function.
func (m *StoreMessage) OnMessage(fn MessageListener) func() {
	id := m.nextListenerID.Add(1)
	m.listenersMu.Lock()
	m.listeners[id] = fn
	m.listenersMu.Unlock()
	return func() {
		m.listenersMu.Lock()
		delete(m.listeners, id)
		m.listenersMu.Unlock()
	}
}

// Send sends a message to targetID and blocks until a response arrives or ctx is cancelled.
func (m *StoreMessage) Send(ctx context.Context, targetID, msgType, data string) (string, error) {
	messageID := uuid.New().String()

	ch := make(chan Envelope, 1)
	m.pendingMu.Lock()
	m.pending[messageID] = ch
	m.pendingMu.Unlock()

	env := Envelope{
		MessageID: messageID,
		SenderID:  m.id,
		Type:      msgType,
		Data:      data,
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		m.removePending(messageID)
		return "", err
	}

	if err := m.ss.SetItem(ctx, "msg", reqKey(targetID, messageID), string(encoded)); err != nil {
		m.removePending(messageID)
		return "", err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return "", errors.New("store closed")
		}
		if resp.Error != "" {
			return "", errors.New(resp.Error)
		}
		return resp.Data, nil
	case <-ctx.Done():
		m.removePending(messageID)
		return "", ctx.Err()
	}
}

func (m *StoreMessage) removePending(messageID string) {
	m.pendingMu.Lock()
	delete(m.pending, messageID)
	m.pendingMu.Unlock()
}

func (m *StoreMessage) sendResponse(messageID, data, errMsg string) error {
	env := Envelope{
		MessageID: messageID,
		SenderID:  m.id,
		Data:      data,
		Error:     errMsg,
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return m.ss.SetItem(context.Background(), "msg", resKey(messageID), string(encoded))
}

func (m *StoreMessage) onSyncUpdate(item core.SyncStoreItem) {
	key := item.Key

	// Incoming request addressed to us
	myPrefix := reqPrefix + m.id + "%"
	if len(key) > len(myPrefix) && key[:len(myPrefix)] == myPrefix {
		var env Envelope
		if err := json.Unmarshal([]byte(item.Value), &env); err != nil {
			return
		}

		m.notifyMessageListeners(env)

		m.handlersMu.RLock()
		handler, ok := m.handlers[env.Type]
		m.handlersMu.RUnlock()

		if !ok {
			m.sendResponse(env.MessageID, "", fmt.Sprintf("no handler for type %q", env.Type))
			return
		}

		responseData, err := handler(env)
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		m.sendResponse(env.MessageID, responseData, errMsg)
		return
	}

	// Response to a message we sent
	if len(key) > len(resPrefix) && key[:len(resPrefix)] == resPrefix {
		var env Envelope
		if err := json.Unmarshal([]byte(item.Value), &env); err != nil {
			return
		}

		m.pendingMu.Lock()
		ch, ok := m.pending[env.MessageID]
		if ok {
			delete(m.pending, env.MessageID)
		}
		m.pendingMu.Unlock()

		if ok {
			ch <- env
		}
	}
}

func (m *StoreMessage) notifyMessageListeners(env Envelope) {
	m.listenersMu.RLock()
	listeners := make([]MessageListener, 0, len(m.listeners))
	for _, fn := range m.listeners {
		listeners = append(listeners, fn)
	}
	m.listenersMu.RUnlock()
	for _, fn := range listeners {
		fn(env)
	}
}

var _ core.SyncStore = (*StoreMessage)(nil)
