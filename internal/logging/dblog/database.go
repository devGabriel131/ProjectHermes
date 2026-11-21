// Package dblog is the pgx pool's query tracer: slow-query and pool-wait summaries in the shared format of
// internal/logging, never SQL. See docs/operations/observability.md.
package dblog

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"apollo/internal/logging"
)

const slowQueryThreshold = 200 * time.Millisecond

type slowQueryBucket struct {
	count uint64
	worst time.Duration
}

// Tracer counts slow successful queries per label without reading their SQL, arguments or rows and, once
// WatchPool is set, the pool waits since the previous Flush. A query's time starts once it holds a
// connection, so it excludes its pool wait.
type Tracer struct {
	log     *slog.Logger
	mu      sync.Mutex
	buckets map[string]slowQueryBucket
	// waits reads the pool's cumulative waits; poolWaits and poolWaited are its values at the previous Flush.
	waits      func() (int64, time.Duration)
	poolWaits  int64
	poolWaited time.Duration
}

var _ pgx.QueryTracer = (*Tracer)(nil)

func New(log *slog.Logger) *Tracer {
	return &Tracer{log: log, buckets: map[string]slowQueryBucket{}}
}

// WatchPool makes Flush report pool's waits: acquires that found every connection busy and waited for one to
// be released, counted as a lower bound. The pool also counts an acquire that opened a new connection as a
// wait, so the connections opened are subtracted; that undercounts by each failed connection attempt, each
// acquire abandoned while its connection was opening, and each waiter that ended up opening a replacement for
// an expired or broken connection. It relies on db.Open opening connections only on acquire (MinConns and
// MinIdleConns 0). The wait time cannot be split, so it also holds the time spent opening connections.
func (t *Tracer) WatchPool(pool *pgxpool.Pool) {
	t.watch(func() (int64, time.Duration) {
		stat := pool.Stat()
		return stat.EmptyAcquireCount() - stat.NewConnsCount(), stat.EmptyAcquireWaitTime()
	})
}

func (t *Tracer) watch(waits func() (int64, time.Duration)) {
	t.mu.Lock()
	t.waits = waits
	t.mu.Unlock()
}

type queryStart struct{}

// TraceQueryStart stamps the start time on the query's context; the SQL and arguments in data are never read.
func (t *Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStart{}, time.Now())
}

// TraceQueryEnd counts a slow successful query under its context's label; failed ones are reported by
// their callers.
func (t *Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	begin, ok := ctx.Value(queryStart{}).(time.Time)
	elapsed := time.Since(begin)
	if !ok || data.Err != nil || elapsed <= slowQueryThreshold {
		return
	}
	label := logging.Label(ctx)
	t.mu.Lock()
	bucket := t.buckets[label]
	bucket.count++
	bucket.worst = max(bucket.worst, elapsed)
	t.buckets[label] = bucket
	t.mu.Unlock()
}

// Flush atomically takes all counters, then emits at most one warning per label and one for pool waits.
func (t *Tracer) Flush() {
	t.mu.Lock()
	buckets := t.buckets
	t.buckets = map[string]slowQueryBucket{}
	var waits int64
	var waited time.Duration
	if t.waits != nil {
		count, duration := t.waits()
		waits, waited = count-t.poolWaits, duration-t.poolWaited
		t.poolWaits, t.poolWaited = count, duration
	}
	t.mu.Unlock()

	for _, label := range slices.Sorted(maps.Keys(buckets)) {
		bucket := buckets[label]
		t.log.LogAttrs(context.Background(), slog.LevelWarn, "database_slow_summary",
			slog.String("operation", label),
			slog.Uint64("count", bucket.count),
			slog.Float64("worst_duration_ms", float64(bucket.worst)/float64(time.Millisecond)),
			slog.Int64("threshold_ms", int64(slowQueryThreshold/time.Millisecond)),
		)
	}
	if waits > 0 {
		t.log.LogAttrs(context.Background(), slog.LevelWarn, "database_pool_wait",
			slog.Int64("count", waits),
			slog.Float64("total_wait_ms", float64(waited)/float64(time.Millisecond)),
		)
	}
}
