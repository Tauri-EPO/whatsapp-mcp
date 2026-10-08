package main

// Failed event, history and outbound persistence writes share one bounded
// BUSY/LOCKED retry policy owned by Bridge, including shutdown cancellation.
// Each closure contains repeatable database effects only: an outbound remote
// send is never inside it. Exhaustion records one ERROR with row identity
// (never content) and increments the store-failure counter. Chat rows have
// no message ID. Non-busy errors stop on the first attempt.
//
// A persistent writer lock can consume each SQLite busy timeout. The default
// budget is three attempts and two waits (200 ms + 1 s); retries narrow the
// loss window but cannot guarantee machine latency or fair writer scheduling.

import (
	"errors"
	"time"
)

// SQLite primary result codes for "somebody else holds the lock".
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// isBusyError reports whether err is SQLITE_BUSY or SQLITE_LOCKED, in any of
// their extended forms (the primary code is the low byte).
func isBusyError(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	primary := coded.Code() & 0xff
	return primary == sqliteBusy || primary == sqliteLocked
}

// defaultStoreRetryDelays is how long a live write waits before each new
// attempt on a busy database; its length is the number of retries. A function
// rather than a package variable, like the other Bridge timings (issue #382).
func defaultStoreRetryDelays() []time.Duration {
	return []time.Duration{200 * time.Millisecond, time.Second}
}

// retryBusy runs write, and again after each of b.StoreRetryDelays for as
// long as the database is busy. It returns the error of the last attempt.
func (b *Bridge) retryBusy(write func() error) error {
	return retryBusyWithWait(write, b.StoreRetryDelays, b.waitStoreRetry)
}

// The same bounded loop for event/history writes and outbound persistence.
// write must contain database effects only and be safe to repeat.
func retryBusyWithWait(write func() error, delays []time.Duration, wait func(time.Duration) bool) error {
	err := write()
	for _, delay := range delays {
		if err == nil || !isBusyError(err) || !wait(delay) {
			break
		}
		err = write()
	}
	return err
}

// storeLive runs one write under retryBusy and reports whether it is in the
// store; a write that is given up is counted and logged once. write has to be
// safe to run again (the bridge's inserts are upserts).
func (b *Bridge) storeLive(kind, messageID, chatJID string, write func() error) bool {
	if err := b.retryBusy(write); err != nil {
		b.noteStoreFailure(kind, messageID, chatJID, err)
		return false
	}
	return true
}

// waitStoreRetry sleeps before a retry; false means the bridge is shutting
// down and the write is not tried again.
func (b *Bridge) waitStoreRetry(delay time.Duration) bool {
	if b.storeRetryWait != nil {
		return b.storeRetryWait(delay)
	}
	return b.sleep(delay)
}

// noteStoreFailure records a row that is lost for good: one ERROR naming it,
// one count on /metrics.
func (b *Bridge) noteStoreFailure(kind, messageID, chatJID string, err error) {
	b.metrics.storeFailures.Add(1)
	row := kind
	if messageID != "" {
		row += " " + messageID
	}
	b.Log.Errorf("Failed to store %s in %s: %v", row, chatJID, err)
}
