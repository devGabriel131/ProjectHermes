// Package sentrylog reports error events and handler panics to Sentry, with no more data than the JSON log
// line carries. It sits beside internal/logging because sentry-go imports net/http, which the core must not.
// A nil *Reporter is Sentry switched off: no client, no hub, no goroutine. See docs/operations/observability.md.
package sentrylog

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"

	"apollo/internal/logging"
	"apollo/internal/logging/httplog"
)

// Options configure a Reporter. Environment and Release become the event's environment and release.
type Options struct {
	DSN         string
	Environment string
	Release     string
	// Transport delivers events; nil sends them to the DSN. Tests capture them here.
	Transport sentry.Transport
}

// Reporter owns its own Sentry client and hub; it never touches sentry-go's global hub.
type Reporter struct {
	hub *sentry.Hub
}

// New returns nil, and starts nothing, when o.DSN is empty. Tracing stays off.
func New(o Options) (*Reporter, error) {
	if o.DSN == "" {
		return nil, nil
	}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:            o.DSN,
		Environment:    o.Environment,
		Release:        o.Release,
		Transport:      o.Transport,
		SendDefaultPII: false,
		BeforeSend:     scrub,
	})
	if err != nil {
		return nil, err
	}
	return &Reporter{hub: sentry.NewHub(client, sentry.NewScope())}, nil
}

// Handler sends every record at error level or above to Sentry, its attributes as the event's "attributes"
// context.
func (r *Reporter) Handler() slog.Handler {
	return handler{hub: r.hub}
}

// Middleware reports a handler panic, then panics again so net/http still logs http_server_error. It sits
// just inside httplog.Requests, whose route and request ID the panic event takes. A nil Reporter returns next.
func (r *Reporter) Middleware(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	recoverer := sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(next)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recoverer.ServeHTTP(w, req.WithContext(sentry.SetHubOnContext(req.Context(), r.hub.Clone())))
	})
}

// Flush waits up to timeout for the queue to drain and reports whether it did; a send that failed still
// drains it. A nil Reporter has nothing to send.
func (r *Reporter) Flush(timeout time.Duration) bool {
	if r == nil {
		return true
	}
	return r.hub.Flush(timeout)
}

// LastEventID is the ID of the last event Handler captured.
func (r *Reporter) LastEventID() string {
	return string(r.hub.LastEventID())
}

// attributesContext names the event context that holds a log record's attributes.
const attributesContext = "attributes"

type handler struct {
	hub    *sentry.Hub
	extra  map[string]any // attributes from WithAttrs, keyed with their groups
	prefix string         // the open groups, each followed by "."
}

func (handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

func (h handler) Handle(ctx context.Context, record slog.Record) error {
	extra := make(map[string]any, len(h.extra)+record.NumAttrs())
	maps.Copy(extra, h.extra)
	record.Attrs(func(attr slog.Attr) bool {
		flatten(extra, h.prefix, attr)
		return true
	})
	event := &sentry.Event{Level: sentry.LevelError, Message: record.Message, Timestamp: record.Time,
		Contexts: map[string]sentry.Context{attributesContext: extra}}
	h.hub.CaptureEventWithHint(event, &sentry.EventHint{Context: ctx})
	return nil
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	extra := make(map[string]any, len(h.extra)+len(attrs))
	maps.Copy(extra, h.extra)
	for _, attr := range attrs {
		flatten(extra, h.prefix, attr)
	}
	h.extra = extra
	return h
}

func (h handler) WithGroup(name string) slog.Handler {
	if name != "" {
		h.prefix += name + "."
	}
	return h
}

// flatten keys a group's attributes by the group's name, and drops empty attributes, as slog's handlers do.
func flatten(extra map[string]any, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	switch {
	case attr.Equal(slog.Attr{}):
	case attr.Value.Kind() == slog.KindGroup:
		if attr.Key != "" {
			prefix += attr.Key + "."
		}
		for _, inner := range attr.Value.Group() {
			flatten(extra, prefix, inner)
		}
	default:
		extra[prefix+attr.Key] = attr.Value.Any()
	}
}

// scrub is BeforeSend. It drops the request (URL, query, headers, cookies, body), the user and client IP, the
// host name, breadcrumbs, every context but the log attributes, and every error or panic text, keeping types
// and stack traces. The transaction is the event's label (route pattern, operation or "other"), never a path.
func scrub(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
	ctx := hint.Context
	if ctx == nil {
		ctx = context.Background()
	}
	event.Request = nil
	event.User = sentry.User{}
	event.ServerName = ""
	event.Breadcrumbs = nil
	attributes, logged := event.Contexts[attributesContext]
	event.Contexts = nil
	if logged {
		event.Contexts = map[string]sentry.Context{attributesContext: attributes}
	}
	for i := range event.Exception {
		event.Exception[i].Value = ""
	}
	if hint.RecoveredException != nil {
		event.Message = "panic" // a non-error panic value arrives as the message
	}
	event.Transaction = logging.Label(ctx)
	event.Fingerprint = []string{"{{ default }}", event.Transaction}
	if id := httplog.RequestID(ctx); id != "" {
		if event.Tags == nil {
			event.Tags = map[string]string{}
		}
		event.Tags["request_id"] = id
	}
	return event
}
