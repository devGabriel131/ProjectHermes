package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"projecthermes/internal/logging"
)

func TestServeLifecycle(t *testing.T) {
	for _, test := range []struct {
		name  string
		force bool
		drain time.Duration
	}{
		{name: "in_flight_request_drains", drain: 10 * time.Second},
		{name: "blocked_request_is_force_closed", force: true, drain: 100 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			const waitTimeout = 20 * time.Second
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			capture := &serveLogCapture{shutdownRequested: make(chan struct{})}
			log := logging.New(capture)
			signalContext, cancelSignal := context.WithCancel(context.Background())
			clientContext, cancelClient := context.WithCancel(context.Background())
			started := make(chan struct{})
			release := make(chan struct{})
			handlerDone := make(chan struct{})
			var releaseOnce sync.Once
			var requestContext context.Context
			var requestCanceledAt time.Time
			server := &http.Server{Handler: logging.Requests(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				requestContext = r.Context()
				close(started)
				select {
				case <-release:
					_, _ = io.WriteString(w, "drained")
				case <-r.Context().Done():
					requestCanceledAt = time.Now()
				}
			}))}
			transport := &http.Transport{DisableKeepAlives: true}
			client := &http.Client{Transport: transport, Timeout: waitTimeout}
			serveDone := make(chan struct{})
			clientDone := make(chan struct{})
			var serveErr, clientErr error
			var status int
			var body []byte
			t.Cleanup(func() {
				cancelSignal()
				releaseOnce.Do(func() { close(release) })
				cancelClient()
				_ = server.Close()
				_ = listener.Close()
				transport.CloseIdleConnections()
				for _, pending := range []struct {
					name string
					done <-chan struct{}
				}{
					{name: "serve", done: serveDone},
					{name: "client", done: clientDone},
				} {
					select {
					case <-pending.done:
					case <-time.After(waitTimeout):
						t.Errorf("%s did not stop during cleanup", pending.name)
					}
				}
				select {
				case <-started:
					select {
					case <-handlerDone:
					case <-time.After(waitTimeout):
						t.Error("handler did not stop during cleanup")
					}
				default:
				}
			})
			go func() {
				defer close(serveDone)
				serveErr = serve(signalContext, server, listener, log, logging.NewDBLogger(log), test.drain)
			}()
			go func() {
				defer close(clientDone)
				request, err := http.NewRequestWithContext(clientContext, http.MethodGet, "http://"+listener.Addr().String()+"/", nil)
				if err != nil {
					clientErr = err
					return
				}
				response, err := client.Do(request)
				if err != nil {
					clientErr = err
					return
				}
				defer response.Body.Close()
				status = response.StatusCode
				body, clientErr = io.ReadAll(response.Body)
			}()
			await := func(done <-chan struct{}, name string) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(waitTimeout):
					t.Fatalf("timed out waiting for %s", name)
				}
			}
			await(started, "in-flight handler")
			signaledAt := time.Now()
			cancelSignal()
			await(capture.shutdownRequested, "shutdown_requested")
			if err := requestContext.Err(); err != nil {
				t.Fatalf("signal canceled the in-flight request before drain: %v", err)
			}
			if !test.force {
				releaseOnce.Do(func() { close(release) })
			}
			await(serveDone, "serve result")
			await(handlerDone, "handler exit")
			await(clientDone, "request result")
			if test.force {
				if !errors.Is(serveErr, context.DeadlineExceeded) {
					t.Errorf("serve error = %v, want drain deadline failure", serveErr)
				}
				if clientErr == nil {
					t.Errorf("force-closed request succeeded: status=%d body=%q", status, body)
				}
				if requestCanceledAt.IsZero() {
					t.Error("force close did not cancel the blocked request context")
				} else if elapsed := requestCanceledAt.Sub(signaledAt); elapsed < test.drain {
					t.Errorf("request context canceled after %s, before the %s drain", elapsed, test.drain)
				}
			} else {
				if serveErr != nil {
					t.Errorf("serve returned an error after a successful drain: %v", serveErr)
				}
				if clientErr != nil || status != http.StatusOK || string(body) != "drained" {
					t.Errorf("drained request: status=%d body=%q error=%v", status, body, clientErr)
				}
				if !requestCanceledAt.IsZero() {
					t.Error("request context canceled before handler release")
				}
			}
			events := capture.events(t)
			if len(events) != 2 {
				t.Fatalf("lifecycle events = %+v, want exactly shutdown_requested and one terminal outcome", events)
			}
			if event := events[0]; event.Message != "shutdown_requested" || event.Level != "info" || event.DrainTimeoutMS != test.drain.Milliseconds() {
				t.Errorf("shutdown request event = %+v", event)
			}
			wantMessage, wantLevel := "shutdown_completed", "info"
			if test.force {
				wantMessage, wantLevel = "shutdown_failed", "error"
			}
			outcome := events[1]
			if outcome.Message != wantMessage || outcome.Level != wantLevel {
				t.Errorf("terminal event = %+v, want %s at %s", outcome, wantMessage, wantLevel)
			}
			if outcome.DurationMS == nil || *outcome.DurationMS < 0 {
				t.Errorf("terminal event lacks a nonnegative numeric duration_ms: %+v", outcome)
			}
			if test.force && (outcome.ErrorKind != "deadline_exceeded" || outcome.DurationMS == nil || *outcome.DurationMS < float64(test.drain.Milliseconds())) {
				t.Errorf("forced outcome must classify the real %s timeout: %+v", test.drain, outcome)
			}
		})
	}
}

type serveLogEvent struct {
	Message        string   `json:"message"`
	Level          string   `json:"level"`
	DrainTimeoutMS int64    `json:"drain_timeout_ms"`
	DurationMS     *float64 `json:"duration_ms"`
	ErrorKind      string   `json:"error_kind"`
}

type serveLogCapture struct {
	mu                sync.Mutex
	buffer            bytes.Buffer
	shutdownRequested chan struct{}
	requested         bool
}

func (c *serveLogCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.buffer.Write(data)
	var event serveLogEvent
	if json.Unmarshal(data, &event) == nil && event.Message == "shutdown_requested" && !c.requested {
		c.requested = true
		close(c.shutdownRequested)
	}
	return n, err
}

func (c *serveLogCapture) events(t *testing.T) []serveLogEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var events []serveLogEvent
	for _, line := range bytes.Split(bytes.TrimSpace(c.buffer.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event serveLogEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("invalid lifecycle JSON: %v", err)
		}
		events = append(events, event)
	}
	return events
}
