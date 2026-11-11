// Package live tells open management pages that data they show has changed.
// Events carry no data: pages re-fetch their own rows through ordinary,
// authorized requests, so a stream never reveals what changed.
package live

import (
	"io"
	"net/http"
	"sync"
	"time"

	"projecthermes/internal/logging"
)

// heartbeat keeps proxies from closing quiet streams; Railway's cutoff is five
// idle minutes.
const heartbeat = 30 * time.Second

const writeTimeout = 5 * time.Second

// Feed fans committed writes out to every open event stream.
// ponytail: in-process fan-out reaches only this replica's pages; move to
// PostgreSQL LISTEN/NOTIFY before running more than one replica.
type Feed struct {
	mu sync.Mutex
	// streams is nil once Close has ended them.
	streams map[chan struct{}]struct{}
	// closeDeadline keeps sends and HTTP finalization on one shutdown budget.
	closeDeadline time.Time
}

func New() *Feed {
	return &Feed{streams: make(map[chan struct{}]struct{})}
}

// Changed wakes every open stream. A stream with an event already pending
// absorbs further wake-ups, so a burst of writes costs each page one refresh.
func (f *Feed) Changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for stream := range f.streams {
		select {
		case stream <- struct{}{}:
		default:
		}
	}
}

// Close ends idle streams and bounds remaining sends and HTTP finalization to
// writeTimeout. A later stream ends after its first event, within its own budget.
func (f *Feed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streams == nil {
		return
	}
	f.closeDeadline = time.Now().Add(writeTimeout)
	for stream := range f.streams {
		close(stream)
	}
	f.streams = nil
}

// ServeHTTP streams server-sent events: one on connect, so a page catches up
// on anything it missed while disconnected, then one per burst of changes.
func (f *Feed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	response := http.NewResponseController(w)
	// Bound each send, not the stream's lifetime, replacing the server deadline.
	if err := response.SetWriteDeadline(time.Time{}); err != nil {
		logging.RecordError(r, "stream", err)
		http.Error(w, "Live updates are unavailable.", http.StatusInternalServerError)
		return
	}
	stream := make(chan struct{}, 1)
	stream <- struct{}{}
	var lateDeadline time.Time
	f.mu.Lock()
	if f.streams == nil {
		lateDeadline = time.Now().Add(writeTimeout)
		close(stream)
	} else {
		f.streams[stream] = struct{}{}
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, stream)
		f.mu.Unlock()
	}()
	nextWriteDeadline := func() time.Time {
		deadline := time.Now().Add(writeTimeout)
		limit := lateDeadline
		if limit.IsZero() {
			f.mu.Lock()
			limit = f.closeDeadline
			f.mu.Unlock()
		}
		if !limit.IsZero() && limit.Before(deadline) {
			return limit
		}
		return deadline
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
events:
	for {
		event := ": heartbeat\n\n"
		select {
		case <-r.Context().Done():
			break events
		case _, open := <-stream:
			if !open {
				break events
			}
			event = "data: change\n\n"
		case <-ticker.C:
		}
		// Write failures mean the page went away, which needs no report.
		if err := response.SetWriteDeadline(nextWriteDeadline()); err != nil {
			return
		}
		if _, err := io.WriteString(w, event); err != nil {
			return
		}
		if err := response.Flush(); err != nil {
			return
		}
		if err := response.SetWriteDeadline(time.Time{}); err != nil {
			return
		}
	}
	// net/http writes the final chunk after return; keep it on the same budget.
	_ = response.SetWriteDeadline(nextWriteDeadline())
}
