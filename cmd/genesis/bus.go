package main

import (
	"sync"
)

const subscriberQueueSize = 1024

type eventBus struct {
	mu   sync.Mutex
	next uint64
	subs map[uint64]*subscription
}

type subscription struct {
	ch chan lifecycleEvent
}

func newEventBus() *eventBus {
	return &eventBus{subs: map[uint64]*subscription{}}
}

func (b *eventBus) Subscribe() (<-chan lifecycleEvent, func()) {
	if b == nil {
		ch := make(chan lifecycleEvent)
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	sub := &subscription{ch: make(chan lifecycleEvent, subscriberQueueSize)}
	b.subs[id] = sub
	return sub.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(current.ch)
		}
	}
}

func (b *eventBus) publish(event lifecycleEvent) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subs {
		select {
		case sub.ch <- event:
		default:
			// The journal already has the event. Close this subscriber so it
			// reconnects and catches up from the file instead of observing a gap.
			delete(b.subs, id)
			close(sub.ch)
		}
	}
}
