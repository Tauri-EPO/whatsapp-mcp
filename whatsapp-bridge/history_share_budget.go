package main

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

type historyShareJob struct {
	bundle   *waE2E.MessageHistoryBundle
	chat     string
	deadline time.Time
}

// One worker and one waiting job. The first job is handed directly to the
// worker so even before it is scheduled the backlog cannot grow beyond one.
type historyShareQueue struct {
	ctx     context.Context
	run     func(context.Context, historyShareJob)
	mu      sync.Mutex
	jobs    chan historyShareJob
	started bool
	closed  bool
	pending int
	idle    chan struct{}
	wg      sync.WaitGroup
}

func newHistoryShareQueue(ctx context.Context, run func(context.Context, historyShareJob)) *historyShareQueue {
	idle := make(chan struct{})
	close(idle)
	return &historyShareQueue{ctx: ctx, run: run, jobs: make(chan historyShareJob, 1), idle: idle}
}

func (q *historyShareQueue) submit(job historyShareJob) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.ctx.Err() != nil {
		return false
	}
	if q.started {
		select {
		case q.jobs <- job:
		default:
			return false
		}
	}
	if q.pending == 0 {
		q.idle = make(chan struct{})
	}
	q.pending++
	if !q.started {
		q.started = true
		q.wg.Add(1)
		go q.work(job)
	}
	return true
}

func (q *historyShareQueue) work(job historyShareJob) {
	defer q.wg.Done()
	for {
		ctx, cancel := context.WithDeadline(q.ctx, job.deadline)
		q.run(ctx, job) // cancelled jobs still emit exactly one refusal warning
		cancel()
		q.mu.Lock()
		q.pending--
		if q.pending == 0 {
			close(q.idle)
		}
		q.mu.Unlock()
		select {
		case job = <-q.jobs:
		case <-q.ctx.Done():
			// Shutdown refuses the queued job too, with the same one-WARN path.
			select {
			case job = <-q.jobs:
			default:
				return
			}
		}
	}
}

// stop prevents a new WaitGroup Add before joining the worker. Bridge cancels
// the lifecycle context first, so SDK HTTP and import checks can finish.
func (q *historyShareQueue) stop() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wg.Wait()
}

func (q *historyShareQueue) idleSignal() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.idle
}
