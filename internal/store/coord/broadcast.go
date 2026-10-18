package coord

import (
	"context"
	"sync"
)

// Broadcaster is an in-process implementation of core.Coordinator's
// Notify/Listen pair. Each listener has its own buffered queue and goroutine,
// so callbacks run in publish order per listener and never block Notify; a
// listener whose queue is full misses notifications (they are best effort).
type Broadcaster struct {
	mu        sync.Mutex
	listeners map[int]chan string
	next      int
}

// NewBroadcaster returns an empty Broadcaster.
func NewBroadcaster() *Broadcaster { return &Broadcaster{listeners: map[int]chan string{}} }

// Notify delivers payload to every listener.
func (b *Broadcaster) Notify(_ context.Context, payload string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.listeners {
		select {
		case ch <- payload:
		default:
		}
	}
	return nil
}

// Listen registers fn; the returned func unregisters it.
func (b *Broadcaster) Listen(fn func(payload string)) func() {
	ch := make(chan string, 1024)
	b.mu.Lock()
	id := b.next
	b.next++
	b.listeners[id] = ch
	b.mu.Unlock()
	go func() {
		for p := range ch {
			fn(p)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.listeners, id)
			close(ch)
			b.mu.Unlock()
		})
	}
}
