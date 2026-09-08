package main

// A budget for automatic media downloads.
//
// Every inbound media event used to start its own goroutine with
// context.Background(): a burst of a hundred photos meant a hundred
// simultaneous CDN transfers, decrypt buffers and file descriptors, none of
// which shutdown could stop. The auto-cache branch of handleMessage now hands
// the message to a small pool instead: autoDownloadWorkers transfers at a
// time, autoDownloadQueue waiting, all of it under the bridge lifecycle
// context, so Shutdown cancels the transfer in flight and waits for the
// workers before main closes the store.
//
// When the backlog is full the arriving message is dropped rather than queued
// without limit, and the messages already waiting keep their place: the row is
// stored either way, so /api/download and download_media still fetch that file
// whenever it is actually wanted. Drops are logged (WARN) and counted in
// whatsapp_bridge_media_autodownload_drops_total.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// autoDownloadWorkers is how many inbound files are cached at once. Four
	// overlaps the CDN round trips of a busy group without letting a burst
	// take over the uplink or the disk.
	autoDownloadWorkers = 4
	// autoDownloadQueue is how many messages may wait for a worker. A backlog
	// deeper than this is already far behind the conversation, so shedding the
	// newest arrivals (and fetching them on demand) beats growing without end.
	autoDownloadQueue = 256
	// autoDownloadTimeout bounds one cached file. whatsmeow's media client has
	// no timeout of its own, so without this a half-open connection would pin
	// a worker until the process restarts.
	autoDownloadTimeout = 10 * time.Minute
)

// mediaJob names one stored message whose media should be cached.
type mediaJob struct {
	messageID string
	chatJID   string
	mediaType string
}

// mediaJobQueue runs media jobs on a fixed pool of workers with a bounded
// backlog. The workers start with the first job and stop when the context
// passed to newMediaJobQueue is done.
type mediaJobQueue struct {
	ctx     context.Context
	run     func(context.Context, mediaJob)
	workers int
	size    int
	jobs    chan mediaJob
	start   sync.Once
	wg      sync.WaitGroup
	active  atomic.Int64
	drops   atomic.Int64
}

// newMediaJobQueue prepares a pool of workers goroutines and a backlog of size
// jobs. Nothing runs until the first submit, so a bridge that never sees media
// costs no goroutines.
func newMediaJobQueue(ctx context.Context, workers, size int, run func(context.Context, mediaJob)) *mediaJobQueue {
	return &mediaJobQueue{ctx: ctx, run: run, workers: workers, size: size, jobs: make(chan mediaJob, size)}
}

func (q *mediaJobQueue) startWorkers() {
	for range q.workers {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-q.ctx.Done():
					return
				case job := <-q.jobs:
					// Shutdown cancelled us between the queue and here: drop
					// the backlog instead of failing every job noisily.
					if q.ctx.Err() != nil {
						return
					}
					q.active.Add(1)
					q.run(q.ctx, job)
					q.active.Add(-1)
				}
			}
		}()
	}
}

// submit queues job for a worker, or reports false when the backlog is full.
// It never blocks the caller: handleMessage runs on whatsmeow's event path.
func (q *mediaJobQueue) submit(job mediaJob) bool {
	q.start.Do(q.startWorkers)
	select {
	case q.jobs <- job:
		return true
	default:
		q.drops.Add(1)
		return false
	}
}

// queued is the current backlog depth, running the number of files being
// fetched right now, and dropped the jobs refused since startup; all three
// feed /metrics.
func (q *mediaJobQueue) queued() int    { return len(q.jobs) }
func (q *mediaJobQueue) running() int64 { return q.active.Load() }
func (q *mediaJobQueue) dropped() int64 { return q.drops.Load() }

// wait blocks until every worker has returned, which happens once the queue's
// context is cancelled and the job in hand is finished.
func (q *mediaJobQueue) wait() { q.wg.Wait() }

// queueAutoDownload asks the pool to cache the media of a stored message. A
// full queue is a WARN and a counter, never a blocked event loop: the row is
// stored, so download_media still fetches the file later.
func (b *Bridge) queueAutoDownload(messageID, chatJID, mediaType string) {
	job := mediaJob{messageID: messageID, chatJID: chatJID, mediaType: mediaType}
	if b.autoDownloads != nil && b.autoDownloads.submit(job) {
		return
	}
	workers, size := autoDownloadWorkers, autoDownloadQueue
	if b.autoDownloads != nil {
		workers, size = b.autoDownloads.workers, b.autoDownloads.size
	}
	b.Log.Warnf("Auto-download queue full (%d waiting, %d at a time): not caching %s media for message %s in %s; download_media still fetches it",
		size, workers, mediaType, messageID, chatJID)
}

// runAutoDownload is the work a pool worker does: cache one inbound file,
// bounded by autoDownloadTimeout so a stalled CDN cannot hold a worker.
func (b *Bridge) runAutoDownload(ctx context.Context, job mediaJob) {
	ctx, cancel := context.WithTimeout(ctx, autoDownloadTimeout)
	defer cancel()
	success, _, _, path, err := b.DownloadMedia(ctx, job.messageID, job.chatJID)
	switch {
	case success && err == nil:
		b.Log.Infof("✅ Auto-downloaded media: %s", path)
	case err != nil:
		b.Log.Warnf("❌ Auto-download failed for message %s in %s: %v", job.messageID, job.chatJID, err)
	default:
		b.Log.Warnf("❌ Auto-download did not cache message %s in %s", job.messageID, job.chatJID)
	}
}
