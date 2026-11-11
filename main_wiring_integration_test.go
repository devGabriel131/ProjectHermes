//go:build integration

package main

import (
	"crypto/rand"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"

	"projecthermes/internal/auth"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/live"
	"projecthermes/internal/logging"
	"projecthermes/internal/testdb"
)

// unknownToken has a valid customer-token shape but matches no link.
var unknownToken = strings.Repeat("A", 43)

func TestRoutesAreMounted(t *testing.T) {
	t.Parallel()
	handler, _ := newTestServer(t)
	for _, test := range []struct {
		name, method, path string
		status             int
		location           string
	}{
		{"root redirects to management", http.MethodGet, "/", http.StatusFound, "/management/"},
		{"stylesheet is public", http.MethodGet, "/static/styles.css", http.StatusOK, ""},
		{"favicon is public", http.MethodGet, "/favicon.ico", http.StatusOK, ""},
		{"management prefix requires sign-in", http.MethodGet, "/management/events", http.StatusSeeOther, "/management/"},
		{"webhook bypasses sign-in and rejects unsigned events", http.MethodPost, "/webhooks/stripe", http.StatusBadRequest, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := send(t, handler, test.method, test.path, nil, nil, nil)
			if rec.Code != test.status || rec.Header().Get("Location") != test.location {
				t.Fatalf("%s %s: status %d, Location %q; want %d, %q",
					test.method, test.path, rec.Code, rec.Header().Get("Location"), test.status, test.location)
			}
		})
	}
}

func TestPublicCustomerIgnoresStaffSessions(t *testing.T) {
	t.Parallel()
	handler, db := newTestServer(t)
	staff := signInStaff(t, db, handler)
	if err := db.WithContext(t.Context()).Exec("ALTER TABLE sessions RENAME TO unavailable_sessions").Error; err != nil {
		t.Fatal(err)
	}

	rec := send(t, handler, http.MethodHead, "/customer/"+unknownToken, staff, nil, nil)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 ||
		rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" ||
		strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Cookie") || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("public customer with unavailable staff sessions: status %d, headers %v, body %q",
			rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := send(t, handler, http.MethodGet, "/management/", staff, nil, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("protected management with unavailable staff sessions: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestCrossOriginWritesAreRejected(t *testing.T) {
	t.Parallel()
	handler, db := newTestServer(t)
	staff := signInStaff(t, db, handler)
	// Every write carries the signed-in cookie, so only the origin headers can explain a 403.
	writes := []struct {
		name, path string
		sameOrigin int
	}{
		{"management write", "/management/links", http.StatusOK},
		{"customer post", "/customer/" + unknownToken, http.StatusNotFound},
		{"login", "/management/login", http.StatusUnauthorized},
	}
	form := url.Values{"username": {"staff"}, "password": {"wrong password"}}
	origins := map[string]http.Header{
		"fetch metadata": {"Sec-Fetch-Site": {"cross-site"}},
		"origin":         {"Origin": {"https://attacker.example"}},
		"neither":        nil,
	}
	for _, write := range writes {
		for name, header := range origins {
			want := http.StatusForbidden
			if header == nil {
				want = write.sameOrigin
			}
			t.Run(name+"/"+write.name, func(t *testing.T) {
				if rec := send(t, handler, http.MethodPost, write.path, staff, form, header); rec.Code != want {
					t.Fatalf("POST %s: status %d, want %d: %s", write.path, rec.Code, want, rec.Body.String())
				}
			})
		}
	}
}

// newTestServer builds the real handler over an isolated database.
func newTestServer(t *testing.T) (http.Handler, *gorm.DB) {
	t.Helper()
	db := testdb.Open(t, models()...)
	log := logging.New(io.Discard)
	feed := live.New()
	t.Cleanup(feed.Close)
	authentication, err := auth.New(db, false, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authentication.Close)
	server, err := newServer("127.0.0.1:0", db, feed, authentication, billingtest.New(t, db, nil), log)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler, db
}

// signInStaff stores an account as the CLI does, then signs in through the real form.
func signInStaff(t *testing.T, db *gorm.DB, handler http.Handler) *http.Cookie {
	t.Helper()
	password := rand.Text()
	hash, err := argon2id.CreateHash(password, &argon2id.Params{Memory: 16, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(t.Context()).Create(&auth.User{Username: "staff", PasswordHash: hash, AuthVersion: rand.Text()}).Error; err != nil {
		t.Fatal(err)
	}
	rec := send(t, handler, http.MethodPost, "/management/login", nil, url.Values{"username": {"staff"}, "password": {password}}, nil)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(cookies) != 1 {
		t.Fatalf("sign in: status %d, cookies %v", rec.Code, cookies)
	}
	return cookies[0]
}

// send serves one request in memory, with cookie and form when given.
func send(t *testing.T, handler http.Handler, method, path string, cookie *http.Cookie, form url.Values, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	maps.Copy(r.Header, header)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}
