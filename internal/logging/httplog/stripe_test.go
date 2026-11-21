package httplog

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"apollo/internal/logging"
	"apollo/internal/logging/logtest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStripeTimerFlushSummarizesEveryAttemptPerLabel(t *testing.T) {
	var out bytes.Buffer
	failure := errors.New("stripe unreachable")
	timer := NewStripeTimer(logging.New(&out), roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, failure }))
	background := context.Background()
	for _, ctx := range []context.Context{logging.WithOperation(background, "b"), logging.WithOperation(background, "a"), logging.WithOperation(background, "b"), background} {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/prices", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := timer.RoundTrip(r); !errors.Is(err, failure) {
			t.Fatalf("RoundTrip error = %v, want the transport's", err)
		}
	}
	if out.Len() != 0 {
		t.Fatal("Stripe calls emitted before Flush")
	}
	timer.Flush()

	events := logtest.Events(t, out.Bytes())
	want := []struct {
		operation string
		count     float64
	}{{"a", 1}, {"b", 2}, {"other", 1}}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i, w := range want {
		event := events[i]
		if event["message"] != "stripe_call_summary" || event["level"] != "info" || event["operation"] != w.operation || event["count"] != w.count {
			t.Errorf("event %d = %v, want %s count %v", i, event, w.operation, w.count)
		}
		if avg, worst := event["avg_duration_ms"].(float64), event["worst_duration_ms"].(float64); avg < 0 || avg > worst {
			t.Errorf("event %d avg_duration_ms %v, worst_duration_ms %v", i, avg, worst)
		}
		for key := range event {
			switch key {
			case "time", "message", "level", "operation", "count", "avg_duration_ms", "worst_duration_ms":
			default:
				t.Errorf("unexpected Stripe summary attribute %q", key)
			}
		}
	}

	before := out.Len()
	timer.Flush()
	if out.Len() != before {
		t.Fatal("second Flush repeated previous summaries")
	}
}
