package stealthbox

import (
	"context"
	"errors"
	"slices"
	"sync"
)

const (
	maxRunnerWaiters = 8
	maxBridgeWaiters = 64
)

var errRunnerQueueFull = errors.New("runner queue is full")

// Each key represents an occupied runner and holds its waiting requests in
// arrival order. Only the request holding the runner can hand it off.
type runnerQueue struct {
	mu      sync.Mutex
	waiting map[string][]chan struct{}
	total   int
}

func (q *runnerQueue) acquire(ctx context.Context, runner string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ready, ok := q.admit(runner, true)
	if !ok {
		return nil, errRunnerQueueFull
	}
	if ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
		}
	}
	release := func() { q.release(runner) }
	if err := ctx.Err(); err != nil {
		if ready == nil {
			release()
		} else {
			q.cancel(runner, ready)
		}
		return nil, err
	}
	return release, nil
}

func (q *runnerQueue) tryAcquire(runner string) func() {
	if _, ok := q.admit(runner, false); !ok {
		return nil
	}
	return func() { q.release(runner) }
}

func (q *runnerQueue) admit(runner string, wait bool) (chan struct{}, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending, occupied := q.waiting[runner]
	if !occupied {
		q.waiting[runner] = nil
		return nil, true
	}
	if !wait || len(pending) >= maxRunnerWaiters || q.total >= maxBridgeWaiters {
		return nil, false
	}
	ready := make(chan struct{})
	q.waiting[runner] = append(pending, ready)
	q.total++
	return ready, true
}

func (q *runnerQueue) cancel(runner string, ready chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case <-ready:
		// Handoff preceded cancellation, so this request owns the runner.
		q.handoff(runner)
	default:
		pending := q.waiting[runner]
		i := slices.Index(pending, ready)
		q.waiting[runner] = slices.Delete(pending, i, i+1)
		q.total--
	}
}

func (q *runnerQueue) release(runner string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handoff(runner)
}

func (q *runnerQueue) handoff(runner string) {
	pending := q.waiting[runner]
	if len(pending) == 0 {
		delete(q.waiting, runner)
		return
	}
	ready := pending[0]
	q.waiting[runner] = slices.Delete(pending, 0, 1)
	q.total--
	close(ready)
}
