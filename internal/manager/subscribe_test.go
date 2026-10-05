package manager

import (
	"context"
	"testing"
	"time"
)

// A subscriber that never reads must not block notify once its buffer is full.
func TestSubscriptionManagerNotifyDoesNotBlock(t *testing.T) {
	m := &subscriptionManager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := m.subscribe(ctx)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			m.notify()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notify blocked on a subscriber that isn't reading")
	}

	if len(c) == 0 {
		t.Error("subscriber should still have pending notifications")
	}
}
