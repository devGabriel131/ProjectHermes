//go:build integration

package dblog

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"gorm.io/gorm"

	"apollo/internal/logging"
	"apollo/internal/logging/httplog"
	"apollo/internal/logging/logtest"
	"apollo/internal/testdb"
)

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
