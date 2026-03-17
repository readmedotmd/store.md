package message

import (
	"context"
	"encoding/json"
	"errors"
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
//
// # Hooks
//
// StoreMessage supports interceptor hooks that can transform or suppress
// Send results:
//
//   - [StoreMessage.OnSendComplete]: Called when Send receives a successful
//     response. The hook returns a (possibly modified) response string that
//     replaces what Send returns to the caller. Use for decryption, logging,
//     or response post-processing.
//   - [StoreMessage.OnSendError]: Called when Send fails. The hook returns a
//     (possibly modified) error. Return nil to suppress the error entirely —
//     Send will return ("", nil) instead of an error.
//   - [StoreMessage.OnMessage]: Called when an incoming request arrives.
//     This is a notification-only hook (no return value).
//
// Multiple hooks are called in registration order. Each hook receives the
// output of the previous hook (chained transformation).
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

	sendCompleteMu   gosync.RWMutex
	sendCompleteList  map[uint64]func(env Envelope, response string) string
	sendErrorMu      gosync.RWMutex
	sendErrorList     map[uint64]func(env Envelope, err error) error
	nextHookID       atomic.Uint64

	unsub func()
}

// New creates a StoreMessage with the given unique ID.
func New(syncStore *core.StoreSync, id string) *StoreMessage {
	m := &StoreMessage{
		ss:               syncStore,
		id:               id,
		handlers:         make(map[string]Handler),
		pending:          make(map[string]chan Envelope),
		listeners:        make(map[uint64]MessageListener),
		sendCompleteList: make(map[uint64]func(env Envelope, response string) string),
		sendErrorList:    make(map[uint64]func(env Envelope, err error) error),
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
func (m *StoreMessage) SetIfNotExists(ctx context.Context, key, value string) (bool, error)              { return m.ss.SetIfNotExists(ctx, key, value) }
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

// OnSendComplete registers an interceptor invoked when Send receives a
// successful response. The interceptor receives the original envelope and
// response data, and returns a (possibly modified) response string that
// will be returned to the caller of Send. Returns an unsubscribe function.
func (m *StoreMessage) OnSendComplete(fn func(env Envelope, response string) string) func() {
	id := m.nextHookID.Add(1)
	m.sendCompleteMu.Lock()
	m.sendCompleteList[id] = fn
	m.sendCompleteMu.Unlock()
	return func() {
		m.sendCompleteMu.Lock()
		delete(m.sendCompleteList, id)
		m.sendCompleteMu.Unlock()
	}
}

// OnSendError registers an interceptor invoked when Send fails (context
// cancelled, remote error, or store closed). The interceptor receives the
// original envelope and error, and returns a (possibly modified) error.
// Return nil to suppress the error entirely. Returns an unsubscribe function.
func (m *StoreMessage) OnSendError(fn func(env Envelope, err error) error) func() {
	id := m.nextHookID.Add(1)
	m.sendErrorMu.Lock()
	m.sendErrorList[id] = fn
	m.sendErrorMu.Unlock()
	return func() {
		m.sendErrorMu.Lock()
		delete(m.sendErrorList, id)
		m.sendErrorMu.Unlock()
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
			err := m.notifySendError(env, errors.New("store closed"))
			if err == nil {
				return "", nil
			}
			return "", err
		}
		if resp.Error != "" {
			err := m.notifySendError(env, errors.New(resp.Error))
			if err == nil {
				return "", nil
			}
			return "", err
		}
		data := m.notifySendComplete(env, resp.Data)
		return data, nil
	case <-ctx.Done():
		m.removePending(messageID)
		err := m.notifySendError(env, ctx.Err())
		if err == nil {
			return "", nil
		}
		return "", err
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
			m.sendResponse(env.MessageID, "", "unsupported message type")
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

func (m *StoreMessage) notifySendComplete(env Envelope, response string) string {
	m.sendCompleteMu.RLock()
	fns := make([]func(Envelope, string) string, 0, len(m.sendCompleteList))
	for _, fn := range m.sendCompleteList {
		fns = append(fns, fn)
	}
	m.sendCompleteMu.RUnlock()
	for _, fn := range fns {
		response = fn(env, response)
	}
	return response
}

func (m *StoreMessage) notifySendError(env Envelope, err error) error {
	m.sendErrorMu.RLock()
	fns := make([]func(Envelope, error) error, 0, len(m.sendErrorList))
	for _, fn := range m.sendErrorList {
		fns = append(fns, fn)
	}
	m.sendErrorMu.RUnlock()
	for _, fn := range fns {
		err = fn(env, err)
	}
	return err
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
