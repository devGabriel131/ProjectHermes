package live

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"projecthermes/internal/logging"
)

// Pages stay current only if a stream catches them up on connect, follows
// changes for longer than the server's WriteTimeout, and ends once shutdown
// starts, even for a page connecting late.
func TestStreamCatchesUpFollowsChangesAndEndsOnClose(t *testing.T) {
	feed := New()
	server := httptest.NewUnstartedServer(feed)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	open := func() *bufio.Reader {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("status = %d, Content-Type = %q; want an event stream", res.StatusCode, res.Header.Get("Content-Type"))
		}
		return bufio.NewReader(res.Body)
	}

	stream := open()
	requireStreamFrame(t, stream, "data: change\n\n")
	time.Sleep(3 * server.Config.WriteTimeout)
	feed.Changed()
	requireStreamFrame(t, stream, "data: change\n\n")
	feed.Close()
	requireStreamEnd(t, stream)

	late := open()
	requireStreamFrame(t, late, "data: change\n\n")
	requireStreamEnd(t, late)
}

func TestStreamStalledReaderExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		feed := New()
		_, client, _, handlerDone := startPipeServer(t, feed)
		synctest.Sleep(writeTimeout - time.Millisecond)
		select {
		case <-handlerDone:
			t.Fatal("stream ended before its write deadline")
		default:
		}
		synctest.Sleep(2 * time.Millisecond)
		select {
		case <-handlerDone:
		default:
			t.Fatal("non-reading client still holds the handler after its write deadline")
		}
		if body, err := io.ReadAll(client); err != nil || len(body) != 0 {
			t.Fatalf("expired connection = %q, %v; want EOF without an event", body, err)
		}
	})
}

func TestStreamShutdownSharesWriteBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                             string
		initialStalled, slowSend, queued bool
	}{
		{name: "initial_flush_stalled", initialStalled: true},
		{name: "final_chunk_stalled"},
		{name: "slow_send_then_final_chunk", slowSend: true},
		{name: "slow_send_then_queued_send", slowSend: true, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				feed := New()
				server, client, request, handlerDone := startPipeServer(t, feed)

				var response *http.Response
				if !tc.initialStalled {
					var err error
					response, err = http.ReadResponse(bufio.NewReader(client), request)
					if err != nil {
						t.Fatal(err)
					}
					requireStreamFrame(t, response.Body, "data: change\n\n")
				}
				if tc.slowSend {
					feed.Changed()
					synctest.Wait()
					if tc.queued {
						feed.Changed()
						synctest.Wait()
					}
				}

				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				started := time.Now()
				shutdownDone := make(chan error, 1)
				go func() { shutdownDone <- server.Shutdown(ctx) }()
				synctest.Wait()
				if tc.slowSend {
					synctest.Sleep(4 * time.Second)
					if tc.queued {
						feed.Close()
					}
					requireStreamFrame(t, response.Body, "data: change\n\n")
				}
				if err := <-shutdownDone; err != nil {
					t.Fatalf("shutdown required force close: %v", err)
				}
				if elapsed := time.Since(started); elapsed >= writeTimeout+time.Second {
					t.Fatalf("shutdown took %s; sends and finalization must share one %s budget", elapsed, writeTimeout)
				}
				select {
				case <-handlerDone:
				default:
					t.Fatal("shutdown returned before the stream handler ended")
				}
			})
		})
	}
}

func TestStreamIdleLifetimeAndHeartbeatsHTTP2(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		feed := New()
		server := httptest.NewTestServer(t, logging.Requests(logging.New(io.Discard), feed))
		server.EnableHTTP2 = true
		server.Config.WriteTimeout = 10 * time.Second
		t.Cleanup(feed.Close)
		open := func() io.Reader {
			t.Helper()
			response, err := server.Client().Get("https://example.com/events")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { response.Body.Close() })
			if response.ProtoMajor != 2 {
				t.Fatalf("protocol = %q; want HTTP/2", response.Proto)
			}
			return response.Body
		}

		started := time.Now()
		stream := open()
		requireStreamFrame(t, stream, "data: change\n\n")
		requireStreamFrame(t, stream, ": heartbeat\n\n")
		if elapsed := time.Since(started); elapsed != heartbeat {
			t.Fatalf("first heartbeat after %s; want %s", elapsed, heartbeat)
		}
		feed.Changed()
		requireStreamFrame(t, stream, "data: change\n\n")
		requireStreamFrame(t, stream, ": heartbeat\n\n")
		if elapsed := time.Since(started); elapsed != 2*heartbeat {
			t.Fatalf("second heartbeat after %s; want %s", elapsed, 2*heartbeat)
		}
		synctest.Sleep(heartbeat - time.Second)
		feed.Close()
		requireStreamEnd(t, stream)

		synctest.Sleep(writeTimeout + time.Second)
		late := open()
		requireStreamFrame(t, late, "data: change\n\n")
		requireStreamEnd(t, late)
	})
}

func requireStreamFrame(t *testing.T, stream io.Reader, want string) {
	t.Helper()
	frame := make([]byte, len(want))
	if _, err := io.ReadFull(stream, frame); err != nil {
		t.Fatalf("reading %q: %v", want, err)
	}
	if string(frame) != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
}

func requireStreamEnd(t *testing.T, stream io.Reader) {
	t.Helper()
	var next [1]byte
	if n, err := stream.Read(next[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("stream end = %q, %v; want clean EOF", next[:n], err)
	}
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
	addr        net.Addr
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.addr }

func startPipeServer(t *testing.T, feed *Feed) (*http.Server, net.Conn, *http.Request, <-chan struct{}) {
	t.Helper()
	conn, client := net.Pipe()
	listener := &pipeListener{
		connections: make(chan net.Conn, 1),
		closed:      make(chan struct{}),
		addr:        conn.LocalAddr(),
	}
	listener.connections <- conn
	handlerDone := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		feed.ServeHTTP(w, r)
	})
	server := &http.Server{
		Handler:      logging.Requests(logging.New(io.Discard), handler),
		WriteTimeout: 10 * time.Second,
	}
	server.RegisterOnShutdown(feed.Close)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		client.Close()
		feed.Close()
		server.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve ended with %v; want ErrServerClosed", err)
		}
	})
	request, err := http.NewRequest(http.MethodGet, "http://example.com/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(client); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	return server, client, request, handlerDone
}
