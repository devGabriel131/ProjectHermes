package logging

import (
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm/logger"
)

const slowQueryThreshold = 200 * time.Millisecond

type slowQueryBucket struct {
	count uint64
	worst time.Duration
}

// DBLogger reports bounded slow-success summaries without evaluating SQL callbacks and, once WatchPool
// is set, the connection pool waits since the previous Flush. A statement's time includes its pool wait.
type DBLogger struct {
	log     *slog.Logger
	mu      sync.Mutex
	buckets map[string]slowQueryBucket
	// pool and the wait totals it reported at the previous Flush.
	pool       func() sql.DBStats
	poolWaits  int64
	poolWaited time.Duration
}

var _ logger.Interface = (*DBLogger)(nil)

func NewDBLogger(log *slog.Logger) *DBLogger {
	return &DBLogger{log: log, buckets: map[string]slowQueryBucket{}}
}

// WatchPool makes Flush report pool waits read from stats, normally (*sql.DB).Stats.
func (l *DBLogger) WatchPool(stats func() sql.DBStats) {
	l.mu.Lock()
	l.pool = stats
	l.mu.Unlock()
}

// LogMode ignores levels: SQL is never logged.
func (l *DBLogger) LogMode(logger.LogLevel) logger.Interface { return l }

func (l *DBLogger) Info(context.Context, string, ...any)  {}
func (l *DBLogger) Error(context.Context, string, ...any) {}

func (l *DBLogger) Warn(ctx context.Context, _ string, _ ...any) {
	l.log.WarnContext(ctx, "orm_warning")
}

// Trace counts slow successful queries per label; failed ones are reported by their callers.
func (l *DBLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	elapsed := time.Since(begin)
	if err != nil || elapsed <= slowQueryThreshold {
		return
	}
	label := labelFromContext(ctx)
	l.mu.Lock()
	bucket := l.buckets[label]
	bucket.count++
	bucket.worst = max(bucket.worst, elapsed)
	l.buckets[label] = bucket
	l.mu.Unlock()
}

// Flush atomically takes all counters, then emits at most one warning per label and one for pool waits.
func (l *DBLogger) Flush() {
	l.mu.Lock()
	buckets := l.buckets
	l.buckets = map[string]slowQueryBucket{}
	var waits int64
	var waited time.Duration
	if l.pool != nil {
		stats := l.pool()
		waits, waited = stats.WaitCount-l.poolWaits, stats.WaitDuration-l.poolWaited
		l.poolWaits, l.poolWaited = stats.WaitCount, stats.WaitDuration
	}
	l.mu.Unlock()

	for _, label := range slices.Sorted(maps.Keys(buckets)) {
		bucket := buckets[label]
		l.log.LogAttrs(context.Background(), slog.LevelWarn, "database_slow_summary",
			slog.String("operation", label),
			slog.Uint64("count", bucket.count),
			slog.Float64("worst_duration_ms", float64(bucket.worst)/float64(time.Millisecond)),
			slog.Int64("threshold_ms", int64(slowQueryThreshold/time.Millisecond)),
		)
	}
	if waits > 0 {
		l.log.LogAttrs(context.Background(), slog.LevelWarn, "database_pool_wait",
			slog.Int64("count", waits),
			slog.Float64("total_wait_ms", float64(waited)/float64(time.Millisecond)),
		)
	}
}
