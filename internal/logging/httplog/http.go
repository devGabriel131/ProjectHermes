// Package httplog instruments HTTP: request IDs, the one request_failed event, route labels, the server's
// error log and Stripe call timing, in the shared format of internal/logging. Requests must be the
// outermost wrapper; Routes and RecordError panic without it. See docs/operations/observability.md.
package httplog

import (
	"bytes"
	"context"
	"crypto/rand"
	"log"
	"log/slog"
	"net/http"
	"time"

	"apollo/internal/logging"
)

type requestContextKey struct{}

// Request handlers share this state through context copies, never across goroutines.
type requestState struct {
	started   time.Time
	requestID string
	route     string
	status    int

	failure      error
	failureStage string
}

// Requests owns one failure event after the complete handler stack returns normally.
func Requests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestState{started: time.Now(), requestID: requestID(r.Header)}
		ctx := logging.WithRoute(r.Context(), &state.route)
		r = r.WithContext(context.WithValue(ctx, requestContextKey{}, state))
		w.Header().Set("X-Request-ID", state.requestID)
		next.ServeHTTP(&statusWriter{ResponseWriter: w, state: state}, r)

		if state.status == 0 {
			state.status = http.StatusOK
		}
		if state.failure == nil && (state.status < 500 || state.status >= 600) {
			return
		}

		stage := "response"
		if state.failure != nil {
			stage = state.failureStage
		}
		attrs := make([]slog.Attr, 0, 9)
		attrs = append(attrs,
			slog.String("request_id", state.requestID),
			slog.String("method", requestMethod(r.Method)),
			slog.String("route", logging.Label(r.Context())),
			slog.String("stage", stage),
			slog.Int("status", state.status),
			slog.Float64("duration_ms", float64(time.Since(state.started))/float64(time.Millisecond)),
		)
		if state.failure == nil {
			attrs = append(attrs, slog.String("error_kind", "unknown"))
		} else {
			attrs = append(attrs, logging.ErrorAttrs(state.failure)...)
		}
		log.LogAttrs(r.Context(), logging.RequestLevel(stage, state.failure), "request_failed", attrs...)
	})
}

// Routes labels the request with mux's matching pattern before serving it, so nested muxes
// behind request-copying middleware still report their leaf pattern.
func Routes(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			stateFromContext(r.Context()).route = pattern
		}
		mux.ServeHTTP(w, r)
	})
}

// RecordError freezes the first failure and its stage; it emits nothing.
func RecordError(r *http.Request, stage string, err error) {
	state := stateFromContext(r.Context())
	if err == nil || state.failure != nil {
		return
	}
	state.failure = err
	state.failureStage = stage
}

func stateFromContext(ctx context.Context) *requestState {
	if state, ok := ctx.Value(requestContextKey{}).(*requestState); ok {
		return state
	}
	panic("httplog: Requests middleware is required for Routes and RecordError")
}

func requestID(header http.Header) string {
	values := header.Values("X-Railway-Request-Id")
	if len(values) == 1 && validRequestID(values[0]) {
		return values[0]
	}
	return rand.Text()
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func requestMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

type statusWriter struct {
	http.ResponseWriter
	state *requestState
}

func (w *statusWriter) WriteHeader(status int) {
	if w.state.status != 0 {
		return
	}
	if status < 100 || status >= 200 || status == http.StatusSwitchingProtocols {
		w.state.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.state.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// ServerErrorLog drops stdlib diagnostic bytes, preserving only a fixed prefix class.
func ServerErrorLog(logger *slog.Logger) *log.Logger {
	return log.New(serverErrorWriter{logger: logger}, "", 0)
}

type serverErrorWriter struct {
	logger *slog.Logger
}

func (w serverErrorWriter) Write(p []byte) (int, error) {
	kind := "other"
	switch {
	case bytes.HasPrefix(p, []byte("http: panic serving ")):
		kind = "panic"
	case bytes.HasPrefix(p, []byte("http: Accept error: ")):
		kind = "accept"
	case bytes.HasPrefix(p, []byte("http: response.WriteHeader on hijacked connection from ")),
		bytes.HasPrefix(p, []byte("http: superfluous response.WriteHeader call from ")),
		bytes.HasPrefix(p, []byte("http: invalid Content-Length of ")),
		bytes.HasPrefix(p, []byte("http: WriteHeader called with both Transfer-Encoding of ")):
		kind = "response_header"
	}
	w.logger.Error("http_server_error", slog.String("kind", kind))
	return len(p), nil
}
