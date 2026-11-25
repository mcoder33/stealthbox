package stealthbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type runnerAdmission struct {
	release func()
	err     error
}

func beginRunnerWait(t *testing.T, queue *runnerQueue, runner string, depth int, ctx context.Context) <-chan runnerAdmission {
	t.Helper()
	done := make(chan runnerAdmission, 1)
	go func() {
		release, err := queue.acquire(ctx, runner)
		done <- runnerAdmission{release, err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		pending := len(queue.waiting[runner])
		queue.mu.Unlock()
		if pending == depth {
			return done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not reach queue depth %d", runner, depth)
	return nil
}

func takeRunnerAdmission(t *testing.T, done <-chan runnerAdmission) runnerAdmission {
	t.Helper()
	select {
	case admission := <-done:
		return admission
	case <-time.After(3 * time.Second):
		t.Fatal("runner wait did not finish")
		return runnerAdmission{}
	}
}

func requireRunnerWaiting(t *testing.T, done <-chan runnerAdmission) {
	t.Helper()
	select {
	case admission := <-done:
		if admission.release != nil {
			admission.release()
		}
		t.Fatalf("runner admitted out of order: %v", admission.err)
	default:
	}
}

func TestRunnerQueueFIFOAndCancellation(t *testing.T) {
	queue := &runnerQueue{waiting: make(map[string][]chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := queue.tryAcquire("runner")
	if release == nil {
		t.Fatal("initial acquire failed")
	}
	headCtx, cancelHead := context.WithCancel(ctx)
	defer cancelHead()
	head := beginRunnerWait(t, queue, "runner", 1, headCtx)
	first := beginRunnerWait(t, queue, "runner", 2, ctx)
	middleCtx, cancelMiddle := context.WithCancel(ctx)
	defer cancelMiddle()
	middle := beginRunnerWait(t, queue, "runner", 3, middleCtx)
	last := beginRunnerWait(t, queue, "runner", 4, ctx)
	cancelHead()
	if admission := takeRunnerAdmission(t, head); !errors.Is(admission.err, context.Canceled) || admission.release != nil {
		t.Fatalf("canceled head received runner: %+v", admission)
	}
	requireRunnerWaiting(t, first)
	cancelMiddle()
	if admission := takeRunnerAdmission(t, middle); !errors.Is(admission.err, context.Canceled) || admission.release != nil {
		t.Fatalf("canceled middle waiter received runner: %+v", admission)
	}
	requireRunnerWaiting(t, first)
	requireRunnerWaiting(t, last)

	release()
	admission := takeRunnerAdmission(t, first)
	if admission.err != nil || admission.release == nil {
		t.Fatalf("first waiter did not acquire runner: %+v", admission)
	}
	requireRunnerWaiting(t, last)
	if other := queue.tryAcquire("runner"); other != nil {
		other()
		t.Fatal("artifact acquired a busy runner")
	}
	admission.release()

	admission = takeRunnerAdmission(t, last)
	if admission.err != nil || admission.release == nil {
		t.Fatalf("last waiter did not acquire runner: %+v", admission)
	}
	admission.release()
	assertEmptyRunnerQueue(t, queue)
}

func TestRunnerQueueBoundsAndExpiry(t *testing.T) {
	queue := &runnerQueue{waiting: make(map[string][]chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var held []func()
	var waiters []<-chan runnerAdmission
	for i := 0; i < maxBridgeWaiters/maxRunnerWaiters; i++ {
		runner := fmt.Sprintf("runner-%d", i)
		held = append(held, queue.tryAcquire(runner))
		for depth := 1; depth <= maxRunnerWaiters; depth++ {
			waiters = append(waiters, beginRunnerWait(t, queue, runner, depth, ctx))
		}
		if _, err := queue.acquire(ctx, runner); !errors.Is(err, errRunnerQueueFull) {
			t.Fatalf("runner capacity overflow: %v", err)
		}
	}
	independent, err := queue.acquire(ctx, "extra")
	if err != nil {
		t.Fatalf("global queue capacity blocked an idle runner: %v", err)
	}
	if _, err := queue.acquire(ctx, "extra"); !errors.Is(err, errRunnerQueueFull) {
		t.Fatalf("global queue capacity overflow: %v", err)
	}
	independent()
	cancel()
	for _, waiter := range waiters {
		if admission := takeRunnerAdmission(t, waiter); !errors.Is(admission.err, context.Canceled) || admission.release != nil {
			t.Fatalf("canceled waiter received runner: %+v", admission)
		}
	}
	for _, release := range held {
		release()
	}
	assertEmptyRunnerQueue(t, queue)

	release := queue.tryAcquire("runner")
	deadline, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, err := queue.acquire(deadline, "runner"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait did not expire: %v", err)
	}
	release()
	assertEmptyRunnerQueue(t, queue)
}

func TestRunnerQueueCancelDuringHandoff(t *testing.T) {
	queue := &runnerQueue{waiting: make(map[string][]chan struct{})}
	if queue.tryAcquire("runner") == nil {
		t.Fatal("initial acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := beginRunnerWait(t, queue, "runner", 1, ctx)
	secondCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	second := beginRunnerWait(t, queue, "runner", 2, secondCtx)

	// The waiter cannot remove itself until the previous owner has
	// handed off the runner to it.
	queue.mu.Lock()
	cancel()
	queue.handoff("runner")
	queue.mu.Unlock()

	admission := takeRunnerAdmission(t, first)
	if !errors.Is(admission.err, context.Canceled) || admission.release != nil {
		t.Fatalf("canceled waiter received runner: %+v", admission)
	}
	next := takeRunnerAdmission(t, second)
	if next.err != nil || next.release == nil {
		t.Fatalf("request after concurrent cancellation did not acquire runner: %+v", next)
	}
	next.release()
	assertEmptyRunnerQueue(t, queue)
}

func TestRunnerQueueDropsUnusedRunners(t *testing.T) {
	queue := &runnerQueue{waiting: make(map[string][]chan struct{})}
	for i := 0; i < 1000; i++ {
		runner := fmt.Sprintf("runner-%d", i)
		release, err := queue.acquire(context.Background(), runner)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := queue.acquire(ctx, runner); !errors.Is(err, context.Canceled) {
			t.Fatalf("already canceled waiter acquired a runner: %v", err)
		}
		release()
	}
	assertEmptyRunnerQueue(t, queue)
}

func assertEmptyRunnerQueue(t *testing.T, queue *runnerQueue) {
	t.Helper()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.waiting) != 0 || queue.total != 0 {
		t.Fatalf("queue leaked: %v, total=%d", queue.waiting, queue.total)
	}
}
