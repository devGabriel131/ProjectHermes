//go:build integration

package dblog

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"

	"gorm.io/gorm"

	"apollo/internal/db"
	"apollo/internal/logging"
	"apollo/internal/logging/httplog"
	"apollo/internal/logging/logtest"
	"apollo/internal/testdb"
)

// database_pool_wait counts only acquires that waited for a busy connection, never one that opened a new
// connection. WatchPool relies on the pool opening connections only on acquire, so db.Open must hold
// MinConns and MinIdleConns at 0 even when the DSN asks for more.
func TestPoolWaitCountsOnlyBusyWaits(t *testing.T) {
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || dsn.Host == "" {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	query := dsn.Query()
	query.Set("pool_min_conns", "5")
	query.Set("pool_min_idle_conns", "5")
	dsn.RawQuery = query.Encode()
	var out bytes.Buffer
	tracer := New(logging.New(&out))
	database, err := db.Open(dsn.String(), tracer)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// The delta between flushes absorbs connections warmed at startup, so only this check sees the pins;
	// warming after startup (refilling recycled connections) would hide that window's waits.
	if config := database.Pool.Config(); config.MinConns != 0 || config.MinIdleConns != 0 {
		t.Fatalf("MinConns = %d, MinIdleConns = %d, want 0 and 0", config.MinConns, config.MinIdleConns)
	}
	tracer.WatchPool(database.Pool)
	concurrently := func(n int) {
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				if err := database.GORM.WithContext(t.Context()).Exec("SELECT pg_sleep(0.1)").Error; err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}

	// One connection, then nine at once: each acquire finds one idle or opens one, so none waits.
	if err := database.Pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	concurrently(9)
	tracer.Flush()
	if out.Len() != 0 {
		t.Fatalf("opening connections logged %s", out.Bytes())
	}
	// 25 at once on 10: nine take the idle connections, one opens the tenth, fifteen wait.
	concurrently(25)
	tracer.Flush()
	events := logtest.Events(t, out.Bytes())
	if len(events) != 1 || events[0]["message"] != "database_pool_wait" || events[0]["count"] != float64(15) {
		t.Fatalf("events = %v, want one database_pool_wait with count 15", events)
	}
}

// A request's context, carrying its route label, must reach the pool's tracer through GORM and
// database/sql on every path: an exec, a query, and a statement inside a transaction.
func TestRequestLabelReachesTracerThroughGORM(t *testing.T) {
	var out bytes.Buffer
	log := logging.New(&out)
	tracer := New(log)
	db := testdb.OpenTraced(t, tracer)

	const slowSQL = "SELECT 1 FROM pg_sleep(0.21)"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := db.WithContext(r.Context())
		var one int
		if err := ctx.Exec(slowSQL).Error; err != nil {
			t.Errorf("exec: %v", err)
		}
		if err := ctx.Raw(slowSQL).Scan(&one).Error; err != nil {
			t.Errorf("query: %v", err)
		}
		if err := ctx.Transaction(func(tx *gorm.DB) error { return tx.Exec(slowSQL).Error }); err != nil {
			t.Errorf("transaction: %v", err)
		}
	})
	httplog.Requests(log, httplog.Routes(mux)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe/7", nil))
	tracer.Flush()

	events := logtest.Events(t, out.Bytes())
	if len(events) != 1 {
		t.Fatalf("events = %v, want one summary", events)
	}
	if events[0]["operation"] != "GET /probe/{id}" || events[0]["count"] != float64(3) {
		t.Errorf("summary = %v, want operation GET /probe/{id} with count 3", events[0])
	}
}
