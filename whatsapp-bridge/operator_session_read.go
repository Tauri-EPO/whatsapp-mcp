package main

import "context"

type archiveSessionRead struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// At most one export and one snapshot are admitted. Register before opening a
// session reader, so logout cannot miss an in-flight open or admit a new one.
func (b *Bridge) beginArchiveSessionRead(cancel context.CancelFunc) (func(), bool) {
	b.archiveSessionMu.Lock()
	defer b.archiveSessionMu.Unlock()
	if b.operatorLogout.Load() {
		return nil, false
	}
	for i, reader := range b.archiveSessionReaders {
		if reader == nil {
			reader = &archiveSessionRead{cancel: cancel, done: make(chan struct{})}
			b.archiveSessionReaders[i] = reader
			return func() {
				b.archiveSessionMu.Lock()
				defer b.archiveSessionMu.Unlock()
				b.archiveSessionReaders[i] = nil
				close(reader.done)
			}, true
		}
	}
	return nil, false
}

func (b *Bridge) cancelArchiveSessionReads(ctx context.Context) bool {
	b.archiveSessionMu.Lock()
	var done [2]<-chan struct{}
	for i, reader := range b.archiveSessionReaders {
		if reader != nil {
			reader.cancel()
			done[i] = reader.done
		}
	}
	b.archiveSessionMu.Unlock()
	for _, finished := range done {
		if finished == nil {
			continue
		}
		select {
		case <-finished:
		case <-ctx.Done():
			return false
		}
	}
	return true
}
