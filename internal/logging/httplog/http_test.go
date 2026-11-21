package httplog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"

	"apollo/internal/logging"
	"apollo/internal/logging/logtest"
)

type httpTestContextKey struct{}

func TestRequestsNestedMuxFreezesFirstFailure(t *testing.T) {
	var out bytes.Buffer
	leaf := http.NewServeMux()
	leaf.HandleFunc("POST /management/links/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(httpTestContextKey{}) != "leaf" {
			t.Fatal("leaf context copy lost")
		}
	})
	leafRoutes := Routes(leaf)
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stale := r.WithContext(context.WithValue(r.Context(), httpTestContextKey{}, "session"))
		leafRoutes.ServeHTTP(w, stale.WithContext(context.WithValue(stale.Context(), httpTestContextKey{}, "leaf")))
		// Session commit reports through its older request after the leaf returns.
		RecordError(stale, "session", fmt.Errorf("PRIVATE-FIRST-ERROR: %w", sql.ErrTxDone))
		w.WriteHeader(http.StatusInternalServerError)
		RecordError(stale, "render", fmt.Errorf("PRIVATE-RENDER-ERROR: %w", context.Canceled))
		if out.Len() != 0 {
			t.Fatal("failure emitted before handler/session completion")
		}
		w.WriteHeader(http.StatusSeeOther)
	})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://private-host.example/management/links/PRIVATE-TOKEN/cancel?private_query=PRIVATE-QUERY",
		strings.NewReader("PRIVATE-BODY"))
	r.Header.Set("X-Railway-Request-Id", "railway-valid_01.X")
	r.Header.Set("Authorization", "Bearer PRIVATE-AUTHORIZATION")
	r.AddCookie(&http.Cookie{Name: "staff", Value: "PRIVATE-COOKIE"})
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Railway-Request-Id", "upstream-response-id")
	Requests(logging.New(&out), root).ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want committed 500 rather than intended 303", rec.Code)
	}
	if rec.Header().Get("X-Request-ID") != "railway-valid_01.X" || rec.Header().Get("X-Railway-Request-Id") != "upstream-response-id" {
		t.Fatalf("request ID headers changed incorrectly: %v", rec.Header())
	}
	event := onlyRequestEvent(t, out.Bytes())
	for key, want := range map[string]any{
		"level": "error", "request_id": "railway-valid_01.X", "method": "POST",
		"route": "POST /management/links/{id}/cancel",
		"stage": "session", "status": float64(500), "error_kind": "transaction_done",
	} {
		if event[key] != want {
			t.Errorf("%s = %v, want %v", key, event[key], want)
		}
	}
	if duration, ok := event["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("duration_ms = %v, want nonnegative numeric milliseconds", event["duration_ms"])
	}
	for _, forbidden := range []string{
		"PRIVATE-TOKEN", "private-host.example", "private_query", "PRIVATE-QUERY", "PRIVATE-BODY",
		"PRIVATE-AUTHORIZATION", "PRIVATE-COOKIE", "PRIVATE-FIRST-ERROR", "PRIVATE-RENDER-ERROR",
	} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("event exposed %q", forbidden)
		}
	}
}

func TestRequestsRequestIDs(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		reuse  bool
	}{
		{name: "allowed ASCII", values: []string{"AZaz09._-"}, reuse: true},
		{name: "missing"},
		{name: "too long", values: []string{strings.Repeat("a", 129)}},
		{name: "multiple values", values: []string{"first", "second"}},
		{name: "newline", values: []string{"a\nb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.Header[http.CanonicalHeaderKey("X-Railway-Request-Id")] = tc.values
			r.Header.Set("X-Request-ID", "PRIVATE-CLIENT-FALLBACK")
			rec := httptest.NewRecorder()
			rec.Header().Set("X-Railway-Request-Id", "preserve-upstream")
			Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})).ServeHTTP(rec, r)
			id := rec.Header().Get("X-Request-ID")
			if !validRequestID(id) {
				t.Fatalf("response request ID = %q, want valid ASCII ID", id)
			}
			if tc.reuse {
				if id != tc.values[0] {
					t.Errorf("request ID = %q, want reused %q", id, tc.values[0])
				}
			} else {
				if len(id) < 26 || id == "PRIVATE-CLIENT-FALLBACK" || slices.Contains(tc.values, id) {
					t.Errorf("request ID = %q, want generated ID, not untrusted fallback", id)
				}
			}
			if rec.Header().Get("X-Railway-Request-Id") != "preserve-upstream" {
				t.Error("Railway response header overwritten")
			}
			if event := onlyRequestEvent(t, out.Bytes()); event["request_id"] != id {
				t.Errorf("event ID = %v, want response ID %q", event["request_id"], id)
			}
		})
	}
}

func TestRequestsActualStatus(t *testing.T) {
	cases := []struct {
		name    string
		write   func(http.ResponseWriter)
		status  int
		headers []int
		body    string
	}{
		{name: "no writes", write: func(http.ResponseWriter) {}, status: 200},
		{name: "first final wins", write: func(w http.ResponseWriter) {
			w.WriteHeader(201)
			w.WriteHeader(500)
			w.WriteHeader(200)
		}, status: 201, headers: []int{201}},
		{name: "informational before failure", write: func(w http.ResponseWriter) {
			w.WriteHeader(103)
			w.WriteHeader(102)
			w.WriteHeader(500)
			w.WriteHeader(303)
		}, status: 500, headers: []int{103, 102, 500}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			rec := &headerRecorder{ResponseRecorder: httptest.NewRecorder()}
			Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.write(w)
				RecordError(r, "render", errors.New("PRIVATE-STATUS-ERROR"))
			})).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
			if rec.Code != tc.status || !slices.Equal(rec.headers, tc.headers) || rec.Body.String() != tc.body {
				t.Errorf("response = status %d, headers %v, body %q; want %d, %v, %q", rec.Code, rec.headers, rec.Body.String(), tc.status, tc.headers, tc.body)
			}
			if event := onlyRequestEvent(t, out.Bytes()); event["status"] != float64(tc.status) {
				t.Errorf("logged status = %v, want %d", event["status"], tc.status)
			}
		})
	}
}

// ResponseRecorder commits informational statuses; retain them separately like net/http.
type headerRecorder struct {
	*httptest.ResponseRecorder
	headers []int
}

func (rec *headerRecorder) WriteHeader(status int) {
	rec.headers = append(rec.headers, status)
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		return
	}
	rec.ResponseRecorder.WriteHeader(status)
}

func TestRequestsRenderFailureAfter200(t *testing.T) {
	var out bytes.Buffer
	rec := httptest.NewRecorder()
	Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("already rendered"))
		RecordError(r, "render", fmt.Errorf("PRIVATE-RENDER-WRITE: %w", syscall.EPIPE))
	})).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-RENDER-TOKEN", nil))
	event := onlyRequestEvent(t, out.Bytes())
	if rec.Code != 200 || rec.Body.String() != "already rendered" || event["status"] != float64(200) || event["level"] != "warn" || event["error_kind"] != "connection_failure" {
		t.Fatalf("render failure changed response/classification: status %d, body %q, event %v", rec.Code, rec.Body.String(), event)
	}
	if strings.Contains(out.String(), "PRIVATE-RENDER") {
		t.Fatalf("render failure exposed private data: %s", out.String())
	}
}

func TestRequestsSuccessfulAndExpectedResponsesAreSilent(t *testing.T) {
	for _, status := range []int{0, 404} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var out bytes.Buffer
			rec := httptest.NewRecorder()
			Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				RecordError(r, "lookup", nil)
				if status != 0 {
					w.WriteHeader(status)
				}
			})).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-SUCCESS-TOKEN", nil))
			want := status
			if want == 0 {
				want = 200
			}
			if rec.Code != want || len(logtest.Events(t, out.Bytes())) != 0 {
				t.Fatalf("status %d, events %s; want status %d and silence", rec.Code, out.String(), want)
			}
		})
	}
}

func TestRequestsBare5xxAndRouteFallback(t *testing.T) {
	var out bytes.Buffer
	Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/customer/PRIVATE-BARE-TOKEN?value=PRIVATE-BARE-QUERY", nil))
	event := onlyRequestEvent(t, out.Bytes())
	if event["level"] != "error" || event["stage"] != "response" || event["route"] != "unmatched" || event["status"] != float64(502) || event["error_kind"] != "unknown" {
		t.Fatalf("bare 5xx metadata = %v", event)
	}
	if _, exists := event["error_type"]; exists {
		t.Error("bare response invented an error type")
	}
	if strings.Contains(out.String(), "PRIVATE-BARE") {
		t.Errorf("bare 5xx exposed URL values: %s", out.String())
	}
}

func TestRequestsMethodNormalizationAndUnmatchedRoute(t *testing.T) {
	for _, method := range []string{http.MethodGet, "PRIVATE-METHOD"} {
		t.Run(method, func(t *testing.T) {
			var out bytes.Buffer
			Requests(logging.New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, "/PRIVATE-UNMATCHED?value=PRIVATE-QUERY", nil))
			want := method
			if method == "PRIVATE-METHOD" {
				want = "OTHER"
			}
			event := onlyRequestEvent(t, out.Bytes())
			if event["method"] != want || event["route"] != "unmatched" {
				t.Fatalf("method/route = %v/%v, want %s/unmatched", event["method"], event["route"], want)
			}
			if strings.Contains(out.String(), "PRIVATE-") {
				t.Errorf("unmatched request exposed method/URL: %s", out.String())
			}
		})
	}
}

func onlyRequestEvent(t *testing.T, data []byte) map[string]any {
	t.Helper()
	events := logtest.Events(t, data)
	if len(events) != 1 {
		t.Fatalf("got %d events, want one: %s", len(events), data)
	}
	if events[0]["message"] != "request_failed" {
		t.Fatalf("event message = %v, want request_failed", events[0]["message"])
	}
	return events[0]
}

const serverLogPrivateMarker = "PRIVATE_sql_cookie_password_token@example.invalid"

func TestServerErrorLogPrivacy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		record string
		kind   string
	}{
		{name: "panic", record: "http: panic serving 192.0.2.1:4321: " + serverLogPrivateMarker + "\ngoroutine 45 [running]:\nprivateHandler()\n\tprivate.go:42\n", kind: "panic"},
		{name: "accept", record: "http: Accept error: " + serverLogPrivateMarker + " 192.0.2.1:4321; retrying in 5ms\nprivate.go:42", kind: "accept"},
		{name: "superfluous header", record: "http: superfluous response.WriteHeader call from " + serverLogPrivateMarker + " (private.go:42)\nprivate stack", kind: "response_header"},
		{name: "hijacked header", record: "http: response.WriteHeader on hijacked connection from " + serverLogPrivateMarker + " (private.go:42)\nprivate stack", kind: "response_header"},
		{name: "content length", record: "http: invalid Content-Length of " + serverLogPrivateMarker + "\nprivate stack", kind: "response_header"},
		{name: "transfer encoding", record: "http: WriteHeader called with both Transfer-Encoding of " + serverLogPrivateMarker + " and a Content-Length of 99\nprivate stack", kind: "response_header"},
		{name: "TLS", record: "http: TLS handshake error from 192.0.2.1:4321: " + serverLogPrivateMarker + "\nprivate.go:42", kind: "other"},
		{name: "embedded prefix", record: serverLogPrivateMarker + "\nhttp: panic serving 192.0.2.1:4321: panic\nprivate.go:42", kind: "other"},
		{name: "unknown", record: serverLogPrivateMarker + "\nprivate.go:42", kind: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			serverLog := ServerErrorLog(logging.New(&out))
			if serverLog.Prefix() != "" || serverLog.Flags() != 0 {
				t.Fatal("stdlib logger must add no prefix or flags")
			}
			serverLog.Print(tc.record)
			events := logtest.Events(t, out.Bytes())
			if len(events) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
				t.Fatal("one stdlib record must emit one JSON line")
			}
			event := events[0]
			if len(event) != 4 || event["level"] != "error" || event["message"] != "http_server_error" || event["kind"] != tc.kind {
				t.Fatalf("unexpected server diagnostic: %v", event)
			}
			for _, forbidden := range []string{serverLogPrivateMarker, "192.0.2.1", "private.go", "goroutine", "privateHandler", "private stack"} {
				if bytes.Contains(out.Bytes(), []byte(forbidden)) {
					t.Errorf("serialized raw diagnostic content %q", forbidden)
				}
			}
		})
	}
}
