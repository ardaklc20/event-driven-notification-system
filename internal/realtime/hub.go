// Package realtime holds the subscribers of real-time status events, their
// bounded queues, the listener generation and the version filter (K24, K25,
// K46). It has no SQL and no network code (K23).
package realtime

import (
	"context"
	"errors"
	"sync"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

var (
	// ErrListenerDown ends a subscription whose listener generation is over:
	// it may have missed events, so its client reconnects for a new snapshot
	// (K46). Subscribe returns it while no generation is live.
	ErrListenerDown = errors.New("realtime: status listener is down")
	// ErrTooSlow ends a subscription whose queue was full when an event
	// arrived (K25).
	ErrTooSlow = errors.New("realtime: subscriber queue is full")
	// ErrUnsubscribed is the cause after Close.
	ErrUnsubscribed = errors.New("realtime: unsubscribed")
)

// Topic is what a subscription follows: one notification or one batch.
type Topic struct {
	batch bool
	id    string
}

// NotificationTopic follows one notification.
func NotificationTopic(id string) Topic { return Topic{id: id} }

// BatchTopic follows every notification of one batch.
func BatchTopic(id string) Topic { return Topic{batch: true, id: id} }

// Hub passes each event to the subscriptions of its notification and of its
// batch. Registration and a listener drop take the same lock, so a
// subscription is never registered under a generation that has ended (K46).
type Hub struct {
	queue int

	mu   sync.Mutex
	gen  uint64 // the current generation; it grows on every Connected
	live bool
	subs map[Topic]map[*Subscription]struct{}
}

// NewHub returns a hub whose subscriptions hold at most queue events each
// (WS_SEND_QUEUE). A queue below 1 counts as 1.
func NewHub(queue int) *Hub {
	return &Hub{queue: max(queue, 1), subs: make(map[Topic]map[*Subscription]struct{})}
}

// Connected starts a new listener generation and returns it. The listener
// calls it once its LISTEN has succeeded.
func (h *Hub) Connected() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.live = true
	return h.gen
}

// Disconnected ends the live generation and closes every subscription with
// ErrListenerDown: any of them may have missed an event.
func (h *Hub) Disconnected() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live = false
	for topic, set := range h.subs {
		for s := range set {
			s.cancel(ErrListenerDown)
		}
		delete(h.subs, topic)
	}
}

// Live returns the live generation, or false while the listener is down.
func (h *Hub) Live() (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen, h.live
}

// Subscribe registers a subscription under generation gen, the one Live
// returned before the upgrade. It returns ErrListenerDown when that
// generation is no longer live, even if a newer one is. Events are queued
// from this moment.
func (h *Hub) Subscribe(t Topic, gen uint64) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.live || h.gen != gen {
		return nil, ErrListenerDown
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Subscription{hub: h, topic: t, events: make(chan domain.StatusEvent, h.queue), ctx: ctx, cancel: cancel}
	set := h.subs[t]
	if set == nil {
		set = make(map[*Subscription]struct{})
		h.subs[t] = set
	}
	set[s] = struct{}{}
	return s, nil
}

// Publish queues ev for the subscriptions of its notification and its batch.
// It never blocks: a subscription whose queue is full is closed with
// ErrTooSlow, so one slow client never delays another (K25).
func (h *Hub) Publish(ev domain.StatusEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliver(NotificationTopic(ev.NotificationID), ev)
	if ev.BatchID != nil {
		h.deliver(BatchTopic(*ev.BatchID), ev)
	}
}

func (h *Hub) deliver(t Topic, ev domain.StatusEvent) {
	for s := range h.subs[t] {
		select {
		case s.events <- ev:
		default:
			s.cancel(ErrTooSlow)
			h.removeLocked(s)
		}
	}
}

// Subscriptions returns how many subscriptions are registered.
func (h *Hub) Subscriptions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

func (h *Hub) removeLocked(s *Subscription) {
	set := h.subs[s.topic]
	delete(set, s)
	if len(set) == 0 {
		delete(h.subs, s.topic)
	}
}

// Subscription is one client's registration. Its queue is the connection's
// send queue; one writer drains it.
type Subscription struct {
	hub    *Hub
	topic  Topic
	events chan domain.StatusEvent
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// Events returns the queue. It is never closed; watch Done.
func (s *Subscription) Events() <-chan domain.StatusEvent { return s.events }

// Done is closed when the subscription ends.
func (s *Subscription) Done() <-chan struct{} { return s.ctx.Done() }

// Context is cancelled when the subscription ends, so a write bound to it
// stops at once.
func (s *Subscription) Context() context.Context { return s.ctx }

// Err returns why the subscription ended, or nil while it is open.
func (s *Subscription) Err() error {
	if s.ctx.Err() == nil {
		return nil
	}
	return context.Cause(s.ctx)
}

// Close removes the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.cancel(ErrUnsubscribed)
	s.hub.removeLocked(s)
}
