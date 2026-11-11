// Package logging writes structured JSON events to stdout, correlates requests and summarizes slow database queries. Requests must be the outermost wrapper; Routes and RecordError panic without it. See docs/operations/observability.md.
package logging

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"syscall"
)

// New writes one JSON object per event, with stable message and lowercase level fields.
func New(out io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) != 0 {
				return attr
			}
			switch attr.Key {
			case slog.MessageKey:
				attr.Key = "message"
			case slog.LevelKey:
				if level, ok := attr.Value.Any().(slog.Level); ok {
					attr.Value = slog.StringValue(strings.ToLower(level.String()))
				}
			}
			return attr
		},
	}))
}

// ErrorAttrs exposes only an error's class, concrete matched type, and valid SQLSTATE.
// Error messages and other error values must never cross this boundary.
func ErrorAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	kind, matched, state := classifyError(err)
	attrs := make([]slog.Attr, 2, 3)
	attrs[0] = slog.String("error_kind", kind)
	attrs[1] = slog.String("error_type", reflect.TypeOf(matched).String())
	if state != "" {
		attrs = append(attrs, slog.String("sqlstate", state))
	}
	return attrs
}

func classifyError(err error) (kind string, matched any, state string) {
	if matched = matchingError(err, context.Canceled); matched != nil {
		return "canceled", matched, ""
	}
	if matched = matchingError(err, context.DeadlineExceeded); matched != nil {
		return "deadline_exceeded", matched, ""
	}
	network, timedOut := matchingNetworkError(err)
	if timedOut {
		return "deadline_exceeded", network, ""
	}
	var database interface {
		SQLState() string
	}
	if errors.As(err, &database) {
		state = database.SQLState()
		if validSQLState(state) {
			matched = database
			switch {
			case strings.HasPrefix(state, "08"), state == "57P01", state == "57P02", state == "57P03":
				return "connection_failure", matched, state
			case state == "57014":
				return "query_canceled", matched, state
			default:
				return "database", matched, state
			}
		}
	}
	if matched = matchingError(err, sql.ErrTxDone); matched != nil {
		return "transaction_done", matched, ""
	}
	for _, target := range [...]error{
		sql.ErrConnDone, driver.ErrBadConn, net.ErrClosed, io.ErrClosedPipe,
		syscall.EPIPE, syscall.ECONNRESET, syscall.ECONNREFUSED, io.EOF, io.ErrUnexpectedEOF,
	} {
		if matched = matchingError(err, target); matched != nil {
			return "connection_failure", matched, ""
		}
	}
	if network != nil {
		return "connection_failure", network, ""
	}
	return "unknown", err, ""
}

// Prefer a timeout anywhere in the error tree over an earlier non-timeout match.
func matchingNetworkError(err error) (net.Error, bool) {
	var network net.Error
	if !errors.As(err, &network) {
		return nil, false
	}
	if network.Timeout() {
		return network, true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		if matched, timedOut := matchingNetworkError(wrapped.Unwrap()); timedOut {
			return matched, true
		}
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if matched, timedOut := matchingNetworkError(inner); timedOut {
				return matched, true
			}
		}
	}
	return network, false
}

// Descend only matching branches so wrappers do not become the reported error type.
func matchingError(err, target error) error {
	if !errors.Is(err, target) {
		return nil
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		if matched := matchingError(wrapped.Unwrap(), target); matched != nil {
			return matched
		}
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if matched := matchingError(inner, target); matched != nil {
				return matched
			}
		}
	}
	return err
}

func validSQLState(state string) bool {
	if len(state) != 5 {
		return false
	}
	for i := range len(state) {
		if c := state[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func requestLevel(stage string, err error) slog.Level {
	if errors.Is(err, context.Canceled) {
		return slog.LevelWarn
	}
	if stage == "render" && (errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)) {
		kind, _, state := classifyError(err)
		if kind == "connection_failure" && state == "" {
			return slog.LevelWarn
		}
	}
	return slog.LevelError
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
