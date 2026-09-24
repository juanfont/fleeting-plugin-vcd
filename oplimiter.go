package vcd

import (
	"context"

	"golang.org/x/sync/semaphore"
)

type opKind int

const (
	opCreate opKind = iota
	opDelete
)

// opLimiter bounds the vCD operations of an instance group. Every create and
// delete (including failed-create cleanups and leftover teardown) holds one
// slot for its whole run, so the total approximates the vCD tasks this plugin
// has in flight against the org limit. Deletes never take the last slot, which
// stays available for creates.
type opLimiter struct {
	size    int64
	total   *semaphore.Weighted
	creates *semaphore.Weighted
	deletes *semaphore.Weighted
	gate    *throttleGate
}

func newOpLimiter(total, maxCreates, maxDeletes int, gate *throttleGate) *opLimiter {
	deletes := min(maxDeletes, total-1)
	if deletes < 1 {
		deletes = 1 // with a single slot, reserving it would stop deletes forever
	}
	return &opLimiter{
		size:    int64(total),
		total:   semaphore.NewWeighted(int64(total)),
		creates: semaphore.NewWeighted(int64(min(maxCreates, total))),
		deletes: semaphore.NewWeighted(int64(deletes)),
		gate:    gate,
	}
}

// tryAcquire takes a slot without blocking. It refuses while the org
// operation-limit pause is active.
func (l *opLimiter) tryAcquire(kind opKind) bool {
	if l.gate.paused() {
		return false
	}
	perKind := l.kindSem(kind)
	if !perKind.TryAcquire(1) {
		return false
	}
	if !l.total.TryAcquire(1) {
		perKind.Release(1)
		return false
	}
	return true
}

func (l *opLimiter) release(kind opKind) {
	l.total.Release(1)
	l.kindSem(kind).Release(1)
}

// drain waits for all in-flight operations and keeps every slot, so no new
// operation starts afterwards.
func (l *opLimiter) drain(ctx context.Context) error {
	return l.total.Acquire(ctx, l.size)
}

func (l *opLimiter) kindSem(kind opKind) *semaphore.Weighted {
	if kind == opDelete {
		return l.deletes
	}
	return l.creates
}
