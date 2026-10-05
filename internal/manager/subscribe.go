package manager

import (
	"context"
	"sync"
)

type subscriptionManager struct {
	subscriptions []chan bool
	mutex         sync.Mutex
}

func (m *subscriptionManager) subscribe(ctx context.Context) <-chan bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	c := make(chan bool, 10)
	m.subscriptions = append(m.subscriptions, c)

	go func() {
		<-ctx.Done()
		m.mutex.Lock()
		defer m.mutex.Unlock()
		close(c)

		for i, s := range m.subscriptions {
			if s == c {
				m.subscriptions = append(m.subscriptions[:i], m.subscriptions[i+1:]...)
				break
			}
		}
	}()

	return c
}

func (m *subscriptionManager) notify() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, s := range m.subscriptions {
		// Don't block if a subscriber isn't reading: notify is called while
		// holding the mutex at the end of every scan/clean job, so one stalled
		// subscriber would otherwise hang the job (and the job queue) forever
		// once its buffer is full. A full buffer already has a pending
		// notification, so dropping this one loses nothing.
		select {
		case s <- true:
		default:
		}
	}
}
