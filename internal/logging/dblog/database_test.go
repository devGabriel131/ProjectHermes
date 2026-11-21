package dblog

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"apollo/internal/logging"
	"apollo/internal/logging/logtest"
)

type databasePrivateData struct{}

func (databasePrivateData) Error() string  { panic("PRIVATE database error was formatted") }
func (databasePrivateData) String() string { panic("PRIVATE query argument was formatted") }

// A zero begin saturates elapsed to the max Duration; a far-future begin saturates it to the min.
// Both keep worst_duration_ms deterministic.
var (
	slowBegin = time.Time{}
	fastBegin = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	maxWorst  = time.Duration(1<<63 - 1)
)

// trace runs one query through tracer as pgx would, with its start moved to begin.
func trace(ctx context.Context, tracer *Tracer, begin time.Time, err error) {
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL:  "SELECT 'PRIVATE SQL'",
		Args: []any{databasePrivateData{}},
	})
	tracer.TraceQueryEnd(context.WithValue(ctx, queryStart{}, begin), nil, pgx.TraceQueryEndData{Err: err})
}

func TestTracerCountsOnlySlowSuccessAndNeverLogsSQL(t *testing.T) {
	var out bytes.Buffer
	tracer := New(logging.New(&out))
	ctx := context.Background()
	// The arguments and the error type panic if formatted.
	trace(ctx, tracer, slowBegin, nil)
	trace(ctx, tracer, slowBegin, databasePrivateData{})
	trace(ctx, tracer, fastBegin, nil)
	tracer.Flush()

	if bytes.Contains(out.Bytes(), []byte("PRIVATE")) {
		t.Fatalf("query data reached the log: %s", out.Bytes())
	}
	events := logtest.Events(t, out.Bytes())
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	assertDatabaseSummary(t, events[0], "other", 1, maxWorst)
}

func TestTracerFlushBoundsAndResets(t *testing.T) {
	var out bytes.Buffer
	tracer := New(logging.New(&out))
	a := logging.WithOperation(context.Background(), "a")
	b := logging.WithOperation(context.Background(), "b")
	for i := range 1000 {
		ctx := a
		if i%2 == 1 {
			ctx = b
		}
		trace(ctx, tracer, slowBegin, nil)
	}
	if out.Len() != 0 {
		t.Fatal("slow traces emitted before Flush")
	}
	tracer.Flush()
	events := logtest.Events(t, out.Bytes())
	if len(events) != 2 {
		t.Fatalf("summary events = %d, want 2", len(events))
	}
	assertDatabaseSummary(t, events[0], "a", 500, maxWorst)
	assertDatabaseSummary(t, events[1], "b", 500, maxWorst)

	before := out.Len()
	tracer.Flush()
	if out.Len() != before {
		t.Fatal("second Flush repeated previous summaries")
	}
}

func TestTracerFlushReportsPoolWaitGrowth(t *testing.T) {
	var out bytes.Buffer
	tracer := New(logging.New(&out))
	count, waited := int64(3), 30*time.Millisecond
	tracer.watch(func() (int64, time.Duration) { return count, waited })
	tracer.Flush()
	count, waited = 5, 50*time.Millisecond
	tracer.Flush()
	tracer.Flush() // no new waits: silent

	events := logtest.Events(t, out.Bytes())
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	for i, want := range []map[string]any{
		{"message": "database_pool_wait", "level": "warn", "count": float64(3), "total_wait_ms": float64(30)},
		{"message": "database_pool_wait", "level": "warn", "count": float64(2), "total_wait_ms": float64(20)},
	} {
		if len(events[i]) != len(want)+1 { // + time
			t.Errorf("event %d = %v, want only %v", i, events[i], want)
		}
		for key, value := range want {
			if events[i][key] != value {
				t.Errorf("event %d %s = %#v, want %#v", i, key, events[i][key], value)
			}
		}
	}
}

func assertDatabaseSummary(t *testing.T, event map[string]any, operation string, count uint64, worst time.Duration) {
	t.Helper()
	for key, want := range map[string]any{
		"message":           "database_slow_summary",
		"level":             "warn",
		"operation":         operation,
		"count":             float64(count),
		"worst_duration_ms": float64(worst) / float64(time.Millisecond),
		"threshold_ms":      float64(200),
	} {
		if event[key] != want {
			t.Errorf("%s = %#v, want %#v", key, event[key], want)
		}
	}
	for key := range event {
		switch key {
		case "time", "message", "level", "operation", "count", "worst_duration_ms", "threshold_ms":
		default:
			t.Errorf("unexpected database summary attribute %q", key)
		}
	}
}
