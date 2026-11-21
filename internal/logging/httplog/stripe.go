package httplog

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"apollo/internal/logging"
)

// StripeTimer times every Stripe HTTP attempt (SDK retries included, up to response headers) per request route
// or background operation and reports stripe_call_summary on Flush, splitting request time into ours and Stripe's.
type StripeTimer struct {
	next    http.RoundTripper
	log     *slog.Logger
	mu      sync.Mutex
	buckets map[string]stripeBucket
}

type stripeBucket struct {
	count        uint64
	total, worst time.Duration
}

func NewStripeTimer(log *slog.Logger, next http.RoundTripper) *StripeTimer {
	return &StripeTimer{next: next, log: log, buckets: map[string]stripeBucket{}}
}

// RoundTrip counts failed attempts too: they hold the caller as long as successful ones.
func (t *StripeTimer) RoundTrip(r *http.Request) (*http.Response, error) {
	begin := time.Now()
	response, err := t.next.RoundTrip(r)
	elapsed := time.Since(begin)
	label := logging.Label(r.Context())
	t.mu.Lock()
	bucket := t.buckets[label]
	bucket.count++
	bucket.total += elapsed
	bucket.worst = max(bucket.worst, elapsed)
	t.buckets[label] = bucket
	t.mu.Unlock()
	return response, err
}

// Flush atomically takes all counters, then emits at most one summary per label.
func (t *StripeTimer) Flush() {
	t.mu.Lock()
	buckets := t.buckets
	t.buckets = map[string]stripeBucket{}
	t.mu.Unlock()

	for _, label := range slices.Sorted(maps.Keys(buckets)) {
		bucket := buckets[label]
		t.log.LogAttrs(context.Background(), slog.LevelInfo, "stripe_call_summary",
			slog.String("operation", label),
			slog.Uint64("count", bucket.count),
			slog.Float64("avg_duration_ms", float64(bucket.total)/float64(bucket.count)/float64(time.Millisecond)),
			slog.Float64("worst_duration_ms", float64(bucket.worst)/float64(time.Millisecond)),
		)
	}
}
