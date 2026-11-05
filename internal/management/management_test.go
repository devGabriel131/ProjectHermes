//go:build integration

package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"
	"paymentwebsite/internal/auth"
	"paymentwebsite/internal/billing"
	"paymentwebsite/internal/billing/billingtest"
	"paymentwebsite/internal/links"
	"paymentwebsite/internal/logging"
	"paymentwebsite/internal/settings"
	"paymentwebsite/internal/students"
	"paymentwebsite/internal/testdb"
)

var htmxHeader = http.Header{"Hx-Request": {"true"}}

// later keeps directly created test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

// unknownID is a well-formed management ID that names no link.
var unknownID = strings.Repeat("00", sha256.Size)

type app struct {
	url   string
	db    *gorm.DB
	store *links.Store
}

// newBiller builds a billing service with a local mock Stripe server for testing management billing routes.
func newBiller(t *testing.T, db *gorm.DB) *billing.Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/billing_portal/sessions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "bps_test",
			"object": "billing_portal.session",
			"url":    "https://billing.stripe.com/session/test",
		})
	})
	mux.HandleFunc("GET /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "inv_draft_no_url" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     id,
				"object": "invoice",
				"status": "draft",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                 id,
			"object":             "invoice",
			"status":             "open",
			"hosted_invoice_url": "https://invoice.stripe.com/test_" + id,
			"lines": map[string]any{
				"object": "list",
				"data":   []map[string]any{{"amount": 20833, "currency": "usd"}},
			},
		})
	})
	mux.HandleFunc("GET /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
			"invoice_settings": map[string]any{
				"default_payment_method": map[string]any{"id": "pm_card_test"},
			},
		})
	})
	mux.HandleFunc("POST /v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       r.PathValue("id"),
			"object":   "customer",
			"livemode": false,
		})
	})
	return billingtest.New(t, db, mux)
}

// newApp wires routes like main.go: auth owns session middleware.
func newApp(t *testing.T) *app {
	t.Helper()
	models := append([]any{&links.Link{}, &students.Student{}, &settings.Settings{}, &auth.User{}, &auth.Session{}}, billing.Models()...)
	db := testdb.Open(t, models...)
	store := links.New(db, nil)
	log := logging.New(io.Discard)
	manager, err := auth.New(db, false, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	child := http.NewServeMux()
	if err := RegisterRoutes(child, store, students.New(db, nil), settings.New(db), auth.Staff, newBiller(t, db)); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	manager.RegisterRoutes(root)
	root.Handle("/management/", manager.Require(child))
	server := httptest.NewServer(logging.Requests(log, root))
	t.Cleanup(server.Close)
	return &app{url: server.URL, db: db, store: store}
}

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// signIn stores an account as the CLI does, then signs in through the real form.
func (a *app) signIn(t *testing.T, username string) *http.Client {
	t.Helper()
	password := rand.Text()
	hash, err := argon2id.CreateHash(password, &argon2id.Params{Memory: 16, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	user := auth.User{Username: username, PasswordHash: hash, AuthVersion: rand.Text()}
	if err := a.db.WithContext(t.Context()).Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	browser := newBrowser(t)
	form := url.Values{"username": {username}, "password": {password}}
	if res, body := a.send(t, browser, http.MethodPost, "/management/login", form, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in %s: status = %d: %s", username, res.StatusCode, body)
	}
	return browser
}

func (a *app) send(t *testing.T, browser *http.Client, method, path string, form url.Values, header http.Header) (*http.Response, string) {
	t.Helper()
	return a.sendRaw(t, browser, method, path, form.Encode(), header)
}

// sendRaw posts a body as written, for requests a url.Values cannot spell.
func (a *app) sendRaw(t *testing.T, browser *http.Client, method, path, body string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, a.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	maps.Copy(req.Header, header)
	res, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	content, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(content)
}

func (a *app) list(t *testing.T) []links.Entry {
	t.Helper()
	entries, err := a.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func contactDetails(name string) students.Details {
	return students.Details{
		Name: name, Email: "private.student@example.com", Phone: "+12025550123", Municipality: "San Juan", Gender: "Female",
	}
}

// requireRegistry compares what List can return: never a token, and creation
// times differ in precision from Create's.
func (a *app) requireRegistry(t *testing.T, want ...links.Entry) {
	t.Helper()
	listed := func(entries []links.Entry) []links.Entry {
		out := make([]links.Entry, len(entries))
		for i, e := range entries {
			out[i] = links.Entry{ID: e.ID, Name: e.Name, Phone: e.Phone, Status: e.Status, CreatedBy: e.CreatedBy, CanceledBy: e.CanceledBy}
		}
		return out
	}
	if got := a.list(t); !slices.Equal(listed(got), listed(want)) {
		t.Fatalf("registry = %+v, want %+v", got, want)
	}
}

func writePaths(id string) []string {
	return []string{
		"/management/links", "/management/links/" + id + "/cancel", "/management/links/cancel", "/management/settings", "/management/program-start-date",
		"/management/students", "/management/students/import",
		"/management/students/1/fields/name", "/management/students/1/billing/reconcile", "/management/students/1/billing/reinstatement", "/management/students/1/billing/portal",
	}
}

// createdToken returns the one customer URL token a creating response shows.
func createdToken(t *testing.T, body string) string {
	t.Helper()
	if strings.Count(body, `href="/customer/`) != 1 {
		t.Fatalf("creating response must show exactly one customer URL: %s", body)
	}
	_, rest, _ := strings.Cut(body, `href="/customer/`)
	token, _, _ := strings.Cut(rest, `"`)
	return token
}

// requireCreated checks an htmx create answers with its notice and one URL
// whose elements carry the ids entry's row keeps across list refreshes, and
// returns the URL's token.
func requireCreated(t *testing.T, res *http.Response, body string, entry links.Entry) string {
	t.Helper()
	requireAcknowledged(t, res, body)
	if !strings.Contains(body, `<a id="link-url-`+entry.ID+`"`) || !strings.Contains(body, `<button id="link-copy-`+entry.ID+`"`) {
		t.Fatalf("created URL elements must match row %s: %s", entry.ID, body)
	}
	return createdToken(t, body)
}

// requirePage checks a full page lists exactly want, in order.
func requirePage(t *testing.T, res *http.Response, body string, want []links.Entry) {
	t.Helper()
	start, end := strings.Index(body, `<ul id="generated-links"`), strings.Index(body, "</ul>")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") || start < 0 || end < start {
		t.Fatalf("status = %d, want full management page: %s", res.StatusCode, body)
	}
	requireRows(t, body[start:end], want)
}

// requireRefresh checks an htmx list refresh is not a page, leaves the status
// line alone, and has exactly want's rows, in order.
func requireRefresh(t *testing.T, res *http.Response, body string, want []links.Entry) {
	t.Helper()
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") || strings.Contains(body, `id="link-status"`) ||
		strings.Contains(body, "management-program-settings") || strings.Contains(body, "program_start_date") {
		t.Fatalf("status = %d, want only rows: %s", res.StatusCode, body)
	}
	requireRows(t, body, want)
}

// requireAcknowledged checks an htmx action answers with its notice and has
// the page refresh its list instead of writing rows an older refresh could
// overwrite.
func requireAcknowledged(t *testing.T, res *http.Response, body string) {
	t.Helper()
	if res.StatusCode != http.StatusOK || res.Header.Get("HX-Trigger") != "management-changed" ||
		!strings.Contains(body, `<p id="link-status" hx-swap-oob="innerHTML">`) || strings.Contains(body, "<li") {
		t.Fatalf("status = %d, HX-Trigger = %q; want a notice and a list refresh: %s",
			res.StatusCode, res.Header.Get("HX-Trigger"), body)
	}
}

func escapedHTML(value string) string {
	// html/template also escapes plus signs in text and quoted attributes.
	return strings.ReplaceAll(html.EscapeString(value), "+", "&#43;")
}

func requireRegistryPrivacy(t *testing.T, body string, details students.Details) {
	t.Helper()
	for _, private := range []string{details.Email, details.Municipality} {
		if private != "" && (strings.Contains(body, private) || strings.Contains(body, escapedHTML(private))) {
			t.Fatalf("registry leaked excluded contact data %q: %s", private, body)
		}
	}
}

// requireRows checks status, saved names and phones, audit history, one-time
// URLs and their slots, and cancellation availability for exactly the expected rows.
func requireRows(t *testing.T, html string, want []links.Entry) {
	t.Helper()
	rows := strings.Split(html, `<li class="management-link">`)[1:]
	if len(rows) != len(want) {
		t.Fatalf("rendered %d links, want %d: %s", len(rows), len(want), html)
	}
	for i, entry := range want {
		row := rows[i]
		ok := strings.Contains(row, string(entry.Status)) && strings.Contains(row, entry.Name)
		if entry.Phone != "" {
			ok = ok && strings.Contains(row, `<span class="management-link-phone">`+escapedHTML(entry.Phone)+`</span>`)
		}
		ok = ok && !strings.Contains(row, "tel:")
		cancelable := strings.Contains(row, `action="/management/links/`+entry.ID+`/cancel"`)
		ok = ok && cancelable == entry.Cancelable()
		// A refresh keeps a URL shown earlier only where the row has its slots.
		ok = ok && strings.Contains(row, `id="link-url-`+entry.ID+`"`) && strings.Contains(row, `id="link-copy-`+entry.ID+`"`)
		ok = ok && strings.Contains(row, "</time> by "+entry.CreatedBy) == (entry.CreatedBy != "")
		ok = ok && strings.Contains(row, "canceled by "+entry.CanceledBy) == (entry.CanceledBy != "")
		if entry.SubmittedAt != nil {
			ok = ok && strings.Contains(row, `datetime="`+entry.SubmittedAt.UTC().Format(time.RFC3339)+`"`)
		}
		if entry.Status != links.Finished && entry.Status != links.Canceled {
			ok = ok && strings.Contains(row, `datetime="`+entry.ExpiresAt.UTC().Format(time.RFC3339)+`"`)
		}
		if entry.Token == "" {
			ok = ok && strings.Contains(row, `<span id="link-url-`+entry.ID+`" hidden hx-preserve></span>`) &&
				strings.Contains(row, `<span id="link-copy-`+entry.ID+`" hidden hx-preserve></span>`) &&
				!strings.Contains(row, "<a ") && !strings.Contains(row, "data-copy-link")
		} else {
			ok = ok && strings.Contains(row, `href="/customer/`+entry.Token+`"`) && strings.Contains(row, "data-copy-link")
		}
		if !ok {
			t.Fatalf("row %d does not render %+v: %s", i, entry, row)
		}
	}
}

func TestUnauthorizedRequestsCannotUseManagement(t *testing.T) {
	a := newApp(t)
	existing, err := a.store.Create(t.Context(), "seed", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), existing.Token, contactDetails("Private student"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit private student: saved=%v, err=%v", saved, err)
	}
	acceptAgreement(t, a, existing.Token)
	seeded, err := settings.New(a.db).Current(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hex.DecodeString(existing.ID)
	now := time.Now().UTC()
	if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, hash, now)
	}); err != nil {
		t.Fatal(err)
	}
	existing.PaidAt = &now
	existing.Name, existing.Phone, existing.Status = "Private student", contactDetails("").Phone, links.Finished
	expired := a.signIn(t, "staff")
	// Expiry is enforced from the session row, so moving it into the past needs no sleep.
	if err := a.db.WithContext(t.Context()).Session(&gorm.Session{AllowGlobalUpdate: true}).
		Model(&auth.Session{}).Update("expiry", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	browsers := []struct {
		name    string
		browser *http.Client
	}{
		{"signed out", newBrowser(t)},
		{"expired session", expired},
	}
	modes := []struct {
		name, redirectHeader string
		header               http.Header
		status               int
	}{
		{"ordinary", "Location", nil, http.StatusSeeOther},
		{"htmx", "HX-Redirect", htmxHeader, http.StatusUnauthorized},
	}
	for _, b := range browsers {
		t.Run(b.name+"/page", func(t *testing.T) {
			res, body := a.send(t, b.browser, http.MethodGet, "/management/", nil, nil)
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `action="/management/login"`) {
				t.Fatalf("status = %d, want sign-in form: %s", res.StatusCode, body)
			}
			if strings.Contains(body, existing.ID) || strings.Contains(body, existing.Token) {
				t.Fatalf("sign-in page leaked link data: %s", body)
			}
		})
		for _, path := range []string{
			"/management/trash",
			"/management/settings",
			"/management/students",
			"/management/students/new",
			"/management/students/1",
			"/management/students/1/fields/name?edit=1",
			"/management/students/1/fields/name",
			"/management/students/1/billing",
			"/management/students/1/billing/invoices/1",
		} {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+"/read"+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodGet, path, nil, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" ||
						strings.Contains(body, "<form") || strings.Contains(body, existing.ID) ||
						strings.Contains(body, existing.Token) || strings.Contains(body, existing.Name) || strings.Contains(body, "student-details") ||
						strings.Contains(body, "student-table") {
						t.Fatalf("unauthorized management read: status=%d, %s=%q, body=%s",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), body)
					}
				})
			}
		}
		for _, path := range writePaths(existing.ID) {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodPost, path, url.Values{
						"value": {"Unauthorized name"}, "lifetime": {"10"}, "program_start_date": {"2026-12-01"},
					}, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" {
						t.Fatalf("status = %d, %s = %q; want %d to /management/",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), mode.status)
					}
					// htmx would swap a returned sign-in form into the link list.
					if strings.Contains(body, "<form") || strings.Contains(body, existing.Token) || strings.Contains(body, existing.Name) {
						t.Fatalf("unauthorized response body = %s", body)
					}
					a.requireRegistry(t, existing)
					student, err := students.New(a.db, nil).Get(t.Context(), 1)
					if err != nil || student == nil || student.Name != existing.Name {
						t.Fatalf("unauthorized write changed student: student=%+v, err=%v", student, err)
					}
				})
			}
		}
	}
	current, err := settings.New(a.db).Current(t.Context())
	if err != nil || current.ProgramStartDate != seeded.ProgramStartDate || !current.UpdatedAt.Equal(seeded.UpdatedAt) ||
		current.LinkLifetimeDays != settings.DefaultLinkLifetimeDays {
		t.Fatalf("unauthorized request changed settings: current=%+v, seeded=%+v, err=%v", current, seeded, err)
	}
}

func TestRegistrySeparatesTrash(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	past := time.Now().Add(-time.Minute)
	var main, trash, all []links.Entry
	for _, tc := range []struct {
		name   string
		row    links.Link
		status links.Status
		trash  bool
	}{
		{"not opened", links.Link{ExpiresAt: later}, links.NotOpened, false},
		{"opened", links.Link{ExpiresAt: later, OpenedAt: &past}, links.Opened, false},
		{"in progress", links.Link{ExpiresAt: later, Details: contactDetails("Draft")}, links.InProgress, false},
		{"completed", links.Link{ExpiresAt: past.Add(7 * 24 * time.Hour), Details: contactDetails("Completed student"), SubmittedAt: &past, PaidAt: &past}, links.Finished, false},
		{"expired draft", links.Link{ExpiresAt: past, Details: contactDetails("Expired draft")}, links.Expired, true},
		{"canceled draft", links.Link{ExpiresAt: later, Details: contactDetails("Canceled draft"), CanceledAt: &past, CanceledBy: "staff"}, links.Canceled, true},
	} {
		digest := sha256.Sum256([]byte(tc.name))
		tc.row.TokenHash, tc.row.CreatedBy = digest[:], "staff"
		if err := a.db.WithContext(t.Context()).Create(&tc.row).Error; err != nil {
			t.Fatal(err)
		}
		if tc.row.SubmittedAt != nil {
			student := students.Student{SourceLinkHash: digest[:], Details: tc.row.Details, CreatedAt: *tc.row.SubmittedAt, Cohort: "2026-11"}
			if err := a.db.WithContext(t.Context()).Create(&student).Error; err != nil {
				t.Fatal(err)
			}
		}
		entry := links.Entry{
			ID: fmt.Sprintf("%x", digest), Name: tc.row.Name, Phone: tc.row.Phone, Status: tc.status,
			ExpiresAt: tc.row.ExpiresAt, SubmittedAt: tc.row.SubmittedAt, PaidAt: tc.row.PaidAt, CreatedBy: tc.row.CreatedBy, CanceledBy: tc.row.CanceledBy,
		}
		all = append(all, entry)
		if tc.trash {
			trash = append(trash, entry)
		} else {
			main = append(main, entry)
		}
	}
	for _, tc := range []struct {
		name, path string
		want       []links.Entry
	}{
		{"active and completed", "/management/", main},
		{"canceled and expired", "/management/trash", trash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := a.send(t, staff, http.MethodGet, tc.path, nil, nil)
			requirePage(t, res, body, tc.want)
			if strings.Contains(body, "management-program-settings") == (tc.path == "/management/trash") {
				t.Fatalf("program date section must appear only on main: %s", body)
			}
			requireRegistryPrivacy(t, body, contactDetails(""))
			res, body = a.send(t, staff, http.MethodGet, tc.path, nil, htmxHeader)
			requireRefresh(t, res, body, tc.want)
			requireRegistryPrivacy(t, body, contactDetails(""))
		})
	}
	a.requireRegistry(t, all...)
}

func TestRegistryProjectsNewAndRefreshedRows(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	created, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	details := contactDetails("Registry student")
	details.Email, details.Municipality = "private.registry@example.com", "Loíza"
	for _, field := range students.Fields() {
		if saved, err := a.store.SaveDraft(t.Context(), created.Token, field.Key, valueOf(field, details)); err != nil || saved != links.Saved {
			t.Fatalf("save registry fixture %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}

	// Ordinary creation renders the shared row alongside its one-time URL.
	res, body := a.send(t, staff, http.MethodPost, "/management/links", nil, nil)
	entries := a.list(t)
	if len(entries) != 2 {
		t.Fatalf("create left %d links, want 2", len(entries))
	}
	entries[1].Token = createdToken(t, body)
	requirePage(t, res, body, entries)
	requireRegistryPrivacy(t, body, details)

	// A later draft changes visible phone, while refresh keeps URL/copy slots.
	if saved, err := a.store.SaveDraft(t.Context(), created.Token, "phone", "+17872345678"); err != nil || saved != links.Saved {
		t.Fatalf("update registry phone: saved=%v, err=%v", saved, err)
	}
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, htmxHeader)
	entries = a.list(t)
	if entries[0].Phone != "+17872345678" {
		t.Fatalf("refreshed phone = %q", entries[0].Phone)
	}
	requireRefresh(t, res, body, entries)
	requireRegistryPrivacy(t, body, details)

	res, body = a.send(t, staff, http.MethodPost, "/management/links/"+created.ID+"/cancel", nil, htmxHeader)
	requireAcknowledged(t, res, body)
	requireRegistryPrivacy(t, body, details)
	trash := a.list(t)[:1]
	for _, headers := range []http.Header{nil, htmxHeader} {
		res, body = a.send(t, staff, http.MethodGet, "/management/trash", nil, headers)
		if headers == nil {
			requirePage(t, res, body, trash)
		} else {
			requireRefresh(t, res, body, trash)
		}
		requireRegistryPrivacy(t, body, details)
	}
	res, body = a.send(t, staff, http.MethodPost, "/management/links", nil, htmxHeader)
	all := a.list(t)
	requireCreated(t, res, body, all[len(all)-1])
	requireRegistryPrivacy(t, body, details)
}

func TestRegistryArchivesCompletedSubmissions(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, fixture := range []struct {
		name                     string
		createdAge, submittedAge time.Duration
	}{
		{"Recent completion", 30 * 24 * time.Hour, 23 * time.Hour},
		{"Archived completion", 26 * time.Hour, 25 * time.Hour},
		{"Expired completion", 10 * 24 * time.Hour, 8 * 24 * time.Hour},
	} {
		created, err := a.store.Create(t.Context(), "staff", later)
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := a.store.Submit(t.Context(), created.Token, contactDetails(fixture.name), ""); err != nil || saved != links.Saved {
			t.Fatalf("submit %s: saved=%v, err=%v", fixture.name, saved, err)
		}
		acceptAgreement(t, a, created.Token)
		submitted := now.Add(-fixture.submittedAge)
		digest, _ := hex.DecodeString(created.ID)
		if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
			if err := links.Enroll(tx, digest, submitted); err != nil {
				return err
			}
			return tx.Model(&links.Link{}).Where("token_hash = ?", digest).
				Updates(map[string]any{
					"created_at": now.Add(-fixture.createdAge), "submitted_at": submitted,
					"paid_at": submitted,
					"expires_at": submitted.Add(7 * 24 * time.Hour),
				}).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	draft, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.SaveDraft(t.Context(), draft.Token, "name", "Old unfinished form"); err != nil || saved != links.Saved {
		t.Fatalf("save old draft: saved=%v, err=%v", saved, err)
	}
	if err := a.db.WithContext(t.Context()).Model(&links.Link{}).
		Where("token_hash = decode(?, 'hex')", draft.ID).Update("created_at", now.Add(-28*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	all := a.list(t)
	byName := make(map[string]links.Entry, len(all))
	for _, entry := range all {
		byName[entry.Name] = entry
	}
	res, body := a.send(t, staff, http.MethodGet, "/management/", nil, nil)
	// Old creation dates do not archive a recent completion or unfinished form.
	requirePage(t, res, body, []links.Entry{byName["Recent completion"], byName["Old unfinished form"]})
	res, body = a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	requirePage(t, res, body, nil)
	res, body = a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "student-table") ||
		strings.Contains(body, "Old unfinished form") {
		t.Fatalf("Students must retain completions, not drafts: status=%d, body=%s", res.StatusCode, body)
	}
	for _, name := range []string{"Recent completion", "Archived completion", "Expired completion"} {
		if !strings.Contains(body, name) {
			t.Errorf("Students omitted %s after archival or access expiry: %s", name, body)
		}
	}

	// A cancel from an old page leaves archived finished rows out of the list
	// it has the page refresh.
	for _, name := range []string{"Archived completion", "Expired completion"} {
		res, body = a.send(t, staff, http.MethodPost, "/management/links/"+byName[name].ID+"/cancel", nil, htmxHeader)
		requireAcknowledged(t, res, body)
	}
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, htmxHeader)
	requireRefresh(t, res, body, []links.Entry{byName["Recent completion"], byName["Old unfinished form"]})
	a.requireRegistry(t, all...)
	res, body = a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	requirePage(t, res, body, nil)
	retained, err := students.New(a.db, nil).List(t.Context(), students.Filter{})
	if err != nil || len(retained) != 3 {
		t.Fatalf("stale cancel lost retained students: students=%+v, err=%v", retained, err)
	}
}

func TestStaffShareLinkStatuses(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	worker := a.signIn(t, "worker")
	cancel := func(t *testing.T, browser *http.Client, id string, header http.Header) (*http.Response, string) {
		t.Helper()
		return a.send(t, browser, http.MethodPost, "/management/links/"+id+"/cancel", nil, header)
	}
	var firstToken, secondToken, thirdToken string
	var entries []links.Entry

	if !t.Run("creation full page and one-time tokens", func(t *testing.T) {
		res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmxHeader)
		created := a.list(t)
		if len(created) != 1 {
			t.Fatalf("HTMX create left %d links, want 1", len(created))
		}
		firstToken = requireCreated(t, res, body, created[0])

		res, body = a.send(t, admin, http.MethodPost, "/management/links", nil, nil)
		secondToken = createdToken(t, body)
		created = a.list(t)
		if len(created) != 2 {
			t.Fatalf("ordinary create left %d links, want 2", len(created))
		}
		created[1].Token = secondToken
		requirePage(t, res, body, created)
	}) {
		return
	}

	if !t.Run("shared customer statuses", func(t *testing.T) {
		if saved, err := a.store.Submit(t.Context(), firstToken, contactDetails("Ana Pérez"), ""); err != nil || saved != links.Saved {
			t.Fatalf("submit name: saved=%v, err=%v", saved, err)
		}
		if link, err := a.store.Open(t.Context(), secondToken); err != nil || link == nil {
			t.Fatalf("open link: link=%v, err=%v", link, err)
		}
		entries = a.list(t)
		if entries[0].Status != links.AwaitingPayment || entries[0].Name != "Ana Pérez" || entries[1].Status != links.Opened {
			t.Fatalf("submitted and opened customer statuses: %+v", entries)
		}
		for name, browser := range map[string]*http.Client{"admin": admin, "worker": worker} {
			t.Run(name+" fresh page", func(t *testing.T) {
				res, body := a.send(t, browser, http.MethodGet, "/management/", nil, nil)
				requirePage(t, res, body, entries)
			})
		}
		res, body := cancel(t, worker, entries[0].ID, nil)
		a.requireRegistry(t, entries...)
		requirePage(t, res, body, entries)
	}) {
		return
	}

	if !t.Run("canceled draft retained", func(t *testing.T) {
		res, body := a.send(t, worker, http.MethodPost, "/management/links", nil, htmxHeader)
		created := a.list(t)
		if len(created) != 3 {
			t.Fatalf("worker create left %d links, want 3", len(created))
		}
		thirdToken = requireCreated(t, res, body, created[2])
		if link, err := a.store.Open(t.Context(), thirdToken); err != nil || link == nil {
			t.Fatalf("open worker's link: link=%v, err=%v", link, err)
		}
		if saved, err := a.store.SaveDraft(t.Context(), secondToken, "name", "Retained draft"); err != nil || saved != links.Saved {
			t.Fatalf("save draft: saved=%v, err=%v", saved, err)
		}
		entries = a.list(t)
		if entries[1].Status != links.InProgress || entries[1].Name != "Retained draft" || entries[2].Status != links.Opened {
			t.Fatalf("draft and unrelated customer statuses: %+v", entries)
		}
		entries[1].Status, entries[1].CanceledBy = links.Canceled, "worker"
		res, body = cancel(t, worker, entries[1].ID, htmxHeader)
		requireAcknowledged(t, res, body)
		a.requireRegistry(t, entries...)
		res, body = a.send(t, admin, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:2])
	}) {
		return
	}

	if !t.Run("first canceler audit preserved", func(t *testing.T) {
		res, body := cancel(t, admin, entries[1].ID, htmxHeader)
		requireAcknowledged(t, res, body)
		a.requireRegistry(t, entries...)
	}) {
		return
	}

	if !t.Run("expired HTMX cancel leaves link uncanceled", func(t *testing.T) {
		expired, err := a.store.Create(t.Context(), "admin", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		expired.Token, expired.Status = "", links.Expired
		entries = append(entries, expired)
		res, body := cancel(t, worker, expired.ID, htmxHeader)
		requireAcknowledged(t, res, body)
		a.requireRegistry(t, entries...)
	}) {
		return
	}

	for _, tc := range []struct{ name, id string }{
		{"unknown", unknownID},
		{"malformed", "not-issued"},
		{"raw customer token", thirdToken},
	} {
		t.Run(tc.name+" cancel ID", func(t *testing.T) {
			if res, body := cancel(t, admin, tc.id, htmxHeader); res.StatusCode != http.StatusNoContent || body != "" {
				t.Fatalf("HTMX: status = %d, want 204 so nothing is swapped: %s", res.StatusCode, body)
			}
			res, body := cancel(t, admin, tc.id, nil)
			requirePage(t, res, body, []links.Entry{entries[0], entries[2]})
			a.requireRegistry(t, entries...)
		})
	}

	if !t.Run("ordinary cancel all retains completed and earlier audit", func(t *testing.T) {
		entries[2].Status, entries[2].CanceledBy = links.Canceled, "admin"
		res, body := a.send(t, admin, http.MethodPost, "/management/links/cancel", nil, nil)
		requirePage(t, res, body, entries[:1])
		a.requireRegistry(t, entries...)
		res, body = a.send(t, worker, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:])
	}) {
		return
	}

	t.Run("HTMX cancel all retains completed and earlier audit", func(t *testing.T) {
		_, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmxHeader)
		createdToken(t, body)
		created := a.list(t)
		if len(created) != len(entries)+1 {
			t.Fatalf("create before cancel all left %d links, want %d", len(created), len(entries)+1)
		}
		newest := created[len(created)-1]
		newest.Status, newest.CanceledBy = links.Canceled, "worker"
		entries = append(entries, newest)
		res, body := a.send(t, worker, http.MethodPost, "/management/links/cancel", nil, htmxHeader)
		requireAcknowledged(t, res, body)
		res, body = a.send(t, worker, http.MethodGet, "/management/", nil, htmxHeader)
		requireRefresh(t, res, body, entries[:1])
		a.requireRegistry(t, entries...)
		res, body = a.send(t, worker, http.MethodGet, "/management/trash", nil, nil)
		requirePage(t, res, body, entries[1:])
		for i, want := range []struct{ createdBy, canceledBy string }{
			{"admin", ""},
			{"admin", "worker"},
			{"worker", "admin"},
			{"admin", ""},
			{"admin", "worker"},
		} {
			if entries[i].CreatedBy != want.createdBy || entries[i].CanceledBy != want.canceledBy {
				t.Errorf("link %d: created by %q, canceled by %q; want %q, %q",
					i, entries[i].CreatedBy, entries[i].CanceledBy, want.createdBy, want.canceledBy)
			}
		}
	})
}

// Database failures answer 500 with no list, row, or notice that htmx could
// swap in as if the request had worked.
func TestStoreFailuresAreServerErrors(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &students.Student{})
	failed := db.WithContext(t.Context()).Begin()
	if err := failed.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// A signed-in username lets cancellation reach the database instead of
	// stopping at the audit check.
	staff := func(context.Context) string { return "staff" }
	if err := RegisterRoutes(mux, links.New(failed, nil), students.New(failed, nil), settings.New(failed), staff, newBiller(t, db)); err != nil {
		t.Fatal(err)
	}
	handler := logging.Requests(logging.New(io.Discard), mux)

	cancel := "/management/links/" + unknownID + "/cancel" // Well-formed, so it reaches the database.
	for _, tc := range []struct {
		name, method, path string
		header             http.Header
		form               url.Values
	}{
		{"list", http.MethodGet, "/management/", nil, nil},
		{"trash", http.MethodGet, "/management/trash", nil, nil},
		{"students", http.MethodGet, "/management/students", nil, nil},
		{"student", http.MethodGet, "/management/students/1", nil, nil},
		{"add student", http.MethodPost, "/management/students", nil, url.Values{"name": {"Ana Torres"}, "email": {"ana@example.com"}, "pin": {"9999"}, "group": {"Básico"}}},
		{"cancel", http.MethodPost, cancel, nil, nil},
		{"htmx cancel", http.MethodPost, cancel, htmxHeader, nil},
		{"program date", http.MethodPost, "/management/program-start-date", nil, url.Values{"program_start_date": {"2026-12-01"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			maps.Copy(req.Header, tc.header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			body := w.Body.String()
			if w.Code != http.StatusInternalServerError || strings.Contains(body, "generated-links") ||
				strings.Contains(body, "management-link") || strings.Contains(body, "student-table") ||
				strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("status = %d, want 500 without a list, row, or notice: %s", w.Code, body)
			}
		})
	}
}

func TestSettingsLifetimeAppliesOnlyToNewLinks(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	create := func() links.Entry {
		t.Helper()
		if res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmxHeader); res.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d: %s", res.StatusCode, body)
		}
		entries := a.list(t)
		return entries[len(entries)-1]
	}
	requireLifetime := func(entry links.Entry, days int) {
		t.Helper()
		want := time.Duration(days) * 24 * time.Hour
		if got := entry.ExpiresAt.Sub(entry.CreatedAt); (got - want).Abs() > time.Second {
			t.Fatalf("link lifetime = %v, want %v", got, want)
		}
	}
	requireSlider := func(body string, days int) {
		t.Helper()
		if !strings.Contains(body, `type="range" min="2" max="14" step="1"`) || !strings.Contains(body, fmt.Sprintf(`value="%d"`, days)) {
			t.Fatalf("settings page must show a 2-14 day slider at %d: %s", days, body)
		}
	}

	sent := create()
	requireLifetime(sent, settings.DefaultLinkLifetimeDays)
	res, body := a.send(t, admin, http.MethodGet, "/management/settings", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings page: status = %d: %s", res.StatusCode, body)
	}
	requireSlider(body, settings.DefaultLinkLifetimeDays)

	res, body = a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {"10"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/management/settings?saved" {
		t.Fatalf("save: status = %d, Location %q: %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	_, body = a.send(t, admin, http.MethodGet, "/management/settings?saved", nil, nil)
	requireSlider(body, 10)
	if !strings.Contains(body, "Saved.") || !strings.Contains(body, "Last changed by admin") {
		t.Fatalf("saved page must confirm and name who changed it: %s", body)
	}

	for _, bad := range []string{"1", "15", "ten", ""} {
		t.Run("rejects "+bad, func(t *testing.T) {
			res, body := a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {bad}}, nil)
			if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status = %d, want 422 with an error: %s", res.StatusCode, body)
			}
			requireSlider(body, 10)
		})
	}

	requireLifetime(create(), 10)
	if already := a.list(t)[0]; already.ID != sent.ID {
		t.Fatalf("registry order changed: %+v", already)
	} else {
		requireLifetime(already, settings.DefaultLinkLifetimeDays) // Moving the slider never changes links already sent.
	}
}

func TestProgramStartDateSavesWithoutChangingExistingEnrollment(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	config := settings.New(a.db)
	pending, err := a.store.Create(t.Context(), "admin", later)
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := a.store.Lookup(t.Context(), pending.Token)
	if err != nil || pendingBefore == nil {
		t.Fatalf("pending link: link=%+v, err=%v", pendingBefore, err)
	}
	saveDate := func(date string) {
		t.Helper()
		res, body := a.send(t, admin, http.MethodPost, "/management/program-start-date", url.Values{"program_start_date": {date}}, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/management/?program-start-date-saved" {
			t.Fatalf("save program date: status=%d, Location=%q: %s", res.StatusCode, res.Header.Get("Location"), body)
		}
	}
	requireDate := func(path, date string) {
		t.Helper()
		res, body := a.send(t, admin, http.MethodGet, path, nil, nil)
		requirePage(t, res, body, a.list(t))
		dateSection, registry := strings.Index(body, `class="management-panel management-program-settings"`), strings.Index(body, `aria-labelledby="link-registry-heading"`)
		if dateSection < 0 || dateSection >= registry || !strings.Contains(body, `<label for="program-start-date">Fecha de comienzo</label>`) ||
			!strings.Contains(body, `name="program_start_date" type="date" value="`+date+`" required`) ||
			!strings.Contains(body, `<form class="management-settings-form" method="post" action="/management/program-start-date">`) {
			t.Fatalf("main page must show saved date in native form above registry: %s", body)
		}
		if strings.Contains(path, "program-start-date-saved") &&
			(!strings.Contains(body, `<p id="program-start-notice" role="status">Fecha de comienzo guardada.</p>`) ||
				!strings.Contains(body, `<p id="link-status" role="status"></p>`)) {
			t.Fatalf("date save notice must not replace link status: %s", body)
		}
	}
	requireDate("/management/", "")
	saveDate("2026-12-01")
	requireDate("/management/?program-start-date-saved", "2026-12-01")
	current, err := config.Current(t.Context())
	if err != nil || current.ProgramStartDate != "2026-12-01" ||
		current.LinkLifetimeDays != settings.DefaultLinkLifetimeDays || current.UpdatedBy != "admin" {
		t.Fatalf("date save must retain lifetime and record staff: current=%+v, err=%v", current, err)
	}

	enrolled, err := a.store.Create(t.Context(), "admin", later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), enrolled.Token, contactDetails("Accepted student"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	accepted, err := a.store.AcceptTerms(t.Context(), enrolled.Token, "2026-12-01")
	if err != nil || accepted == nil || accepted.TermsAcceptedAt == nil {
		t.Fatalf("accept agreement: link=%+v, err=%v", accepted, err)
	}
	res, body := a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {"10"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save lifetime: status=%d: %s", res.StatusCode, body)
	}
	current, err = config.Current(t.Context())
	if err != nil || current.ProgramStartDate != "2026-12-01" || current.LinkLifetimeDays != 10 {
		t.Fatalf("lifetime save changed program date: current=%+v, err=%v", current, err)
	}
	saveDate("2027-01-15")
	requireDate("/management/", "2027-01-15")
	current, err = config.Current(t.Context())
	if err != nil || current.ProgramStartDate != "2027-01-15" || current.LinkLifetimeDays != 10 {
		t.Fatalf("program date save changed lifetime: current=%+v, err=%v", current, err)
	}
	after, err := a.store.Lookup(t.Context(), enrolled.Token)
	if err != nil || after == nil || after.ProgramStartDate != "2026-12-01" || after.TermsAcceptedAt == nil ||
		!after.TermsAcceptedAt.Equal(*accepted.TermsAcceptedAt) || !after.ExpiresAt.Equal(accepted.ExpiresAt) || after.Details != accepted.Details {
		t.Fatalf("settings changed accepted enrollment: link=%+v, err=%v", after, err)
	}
	pendingAfter, err := a.store.Lookup(t.Context(), pending.Token)
	if err != nil || pendingAfter == nil || !pendingAfter.ExpiresAt.Equal(pendingBefore.ExpiresAt) ||
		pendingAfter.ProgramStartDate != "" || pendingAfter.TermsAcceptedAt != nil {
		t.Fatalf("settings changed existing unfinished link: link=%+v, err=%v", pendingAfter, err)
	}
	res, body = a.send(t, admin, http.MethodGet, "/management/settings", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `value="10"`) {
		t.Fatalf("lifetime page must show saved lifetime: status=%d: %s", res.StatusCode, body)
	}
}

func TestProgramStartDateRejectsInvalidFormsWithoutSaving(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	config := settings.New(a.db)
	if err := config.SetProgramStartDate(t.Context(), "2026-12-01", "admin"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, body, attempted string
		header                       http.Header
	}{
		{name: "missing"},
		{name: "query is not posted", query: "?program_start_date=2027-01-15"},
		{name: "empty", body: "program_start_date="},
		{name: "invalid calendar date", body: "program_start_date=2026-02-30", attempted: "2026-02-30"},
		{name: "duplicate", body: "program_start_date=2027-01-15&program_start_date=2027-01-16", attempted: "2027-01-15", header: htmxHeader},
		{name: "malformed encoding", body: "program_start_date=2027-01-15&bad=%zz", attempted: "2027-01-15"},
		{name: "body limit", body: "program_start_date=2027-01-15&padding=" + strings.Repeat("x", 1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, text := a.sendRaw(t, admin, http.MethodPost, "/management/program-start-date"+tc.query, tc.body, tc.header)
			if res.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(text, "<!doctype html>") ||
				!strings.Contains(text, `id="program-start-error" role="alert"`) ||
				!strings.Contains(text, `value="`+tc.attempted+`" required`) ||
				!strings.Contains(text, `<p id="link-status" role="status"></p>`) {
				t.Fatalf("invalid form must retain attempted date in full page with distinct error: status=%d: %s", res.StatusCode, text)
			}
			current, err := config.Current(t.Context())
			if err != nil || current.ProgramStartDate != "2026-12-01" || current.LinkLifetimeDays != settings.DefaultLinkLifetimeDays {
				t.Fatalf("invalid form changed saved settings: current=%+v, err=%v", current, err)
			}
		})
	}
	res, body := a.send(t, admin, http.MethodGet, "/management/", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `type="date" value="2026-12-01" required`) || strings.Contains(body, `id="program-start-error"`) {
		t.Fatalf("reload must show unchanged saved date without prior error: status=%d: %s", res.StatusCode, body)
	}
}

func TestRegistryLoadsProgramSettingsOnlyForFullMainPage(t *testing.T) {
	a := newApp(t)
	failed := a.db.WithContext(t.Context()).Begin()
	if err := failed.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, a.store, students.New(a.db, nil), settings.New(failed), func(context.Context) string { return "staff" }, newBiller(t, a.db)); err != nil {
		t.Fatal(err)
	}
	handler := logging.Requests(logging.New(io.Discard), mux)
	for _, tc := range []struct {
		name, path string
		header     http.Header
		status     int
	}{
		{"main needs settings", "/management/", nil, http.StatusInternalServerError},
		{"main rows", "/management/", htmxHeader, http.StatusOK},
		{"trash page", "/management/trash", nil, http.StatusOK},
		{"trash rows", "/management/trash", htmxHeader, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			maps.Copy(req.Header, tc.header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			body := w.Body.String()
			if w.Code != tc.status || strings.Contains(body, "management-program-settings") ||
				tc.status == http.StatusInternalServerError && !strings.Contains(body, "Could not load settings.") {
				t.Fatalf("status=%d, want %d without date control: %s", w.Code, tc.status, body)
			}
		})
	}
}

// TestManagementBillingRoutes drives every staff billing route through the real session stack.
// The mock Stripe server answers only portal sessions, invoice reads and customer reads, so the
// seeded rows keep reconcile and reinstatement away from any other provider call: Student A's
// later installments have no invoice item (invoice sync leaves them alone), and Student B is
// unfunded with its fee invoice already armed.
func TestManagementBillingRoutes(t *testing.T) {
	const planName = "Plan de 3 Pagos Mensuales"
	a := newApp(t)
	admin := a.signIn(t, "admin_user")
	db := a.db.WithContext(t.Context())
	now := time.Now().UTC().Truncate(time.Microsecond)
	ptr := func(s string) *string { return &s }
	create := func(row any) {
		t.Helper()
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	enroll := func(name string) students.Student {
		student, _ := enrollStudent(t, a, name)
		return student
	}
	seedContract := func(student students.Student, funded *time.Time) billing.Contract {
		t.Helper()
		contract := billing.Contract{
			SourceLinkHash: student.SourceLinkHash, Reference: rand.Text(), PlanID: "three", PlanName: planName,
			InstallmentCount: 3, TotalCents: 62500, FundedAt: funded,
			StripeCustomerID:    ptr(fmt.Sprintf("cus_%d", student.ID)),
			SyncedCustomerName:  student.Name,
			SyncedCustomerEmail: student.Email,
		}
		create(&contract)
		return contract
	}
	seedObligation := func(contract billing.Contract, o billing.Obligation) billing.Obligation {
		t.Helper()
		o.ContractID, o.Reference, o.StripePriceID, o.CollectionDisposition = contract.ID, rand.Text(), "price_test", "collect"
		create(&o)
		return o
	}

	active, suspended, plain := enroll("Active Student"), enroll("Suspended Student"), enroll("Plain Student")

	// Student A: active and funded; installment 1 paid, 2 has a hosted invoice, 3 is a draft without one.
	contractA := seedContract(active, &now)
	seedObligation(contractA, billing.Obligation{Kind: "tuition", Number: 1, AmountCents: 20834, AmountPaidCents: 20834,
		FirstSettledAt: &now, LatestStripeStatus: ptr("paid")})
	ready := seedObligation(contractA, billing.Obligation{Kind: "tuition", Number: 2, AmountCents: 20833, AmountRemainingCents: 20833,
		StripeInvoiceID: ptr("inv_ready"), LatestStripeStatus: ptr("open")})
	draft := seedObligation(contractA, billing.Obligation{Kind: "tuition", Number: 3, AmountCents: 20833, AmountRemainingCents: 20833,
		StripeInvoiceID: ptr("inv_draft_no_url"), LatestStripeStatus: ptr("draft")})

	// Student B: inactive with an open suspension and a ready fee invoice. Student C has no contract.
	if err := db.Model(&students.Student{}).Where("id = ?", suspended.ID).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	contractB := seedContract(suspended, nil)
	episode := billing.Suspension{ContractID: contractB.ID, TriggeringTuitionInstallmentNumber: 1,
		StartedAt: now.AddDate(0, 0, -16)}
	create(&episode)
	fee := seedObligation(contractB, billing.Obligation{Kind: "reinstatement", AmountCents: billing.ReinstatementFeeCents,
		AmountRemainingCents: billing.ReinstatementFeeCents, SuspensionID: &episode.ID, StripeInvoiceID: ptr("inv_fee"),
		StripeInvoiceItemID: ptr("ii_fee"), ScheduledFinalizationAt: &now, LatestStripeStatus: ptr("open")})

	billingPath := func(student any, rest string) string {
		return fmt.Sprintf("/management/students/%v/billing%s", student, rest)
	}
	invoicePath := func(student, obligation any) string {
		return billingPath(student, fmt.Sprintf("/invoices/%v", obligation))
	}
	fragment := func(id uint64) string { return fmt.Sprintf(`id="student-billing-%d"`, id) }
	tooBig, unknown := fmt.Sprint(uint64(1)<<63), fmt.Sprint(uint64(1)<<63-1)
	missing := []string{"0", tooBig, unknown}

	request := func(method, path string, want int) (*http.Response, string) {
		t.Helper()
		res, body := a.send(t, admin, method, path, nil, nil)
		if res.StatusCode != want {
			t.Fatalf("%s %s: status = %d, want %d: %s", method, path, res.StatusCode, want, body)
		}
		return res, body
	}
	requireText := func(what, body string, want bool, parts ...string) {
		t.Helper()
		for _, part := range parts {
			if strings.Contains(body, part) != want {
				t.Fatalf("%s: contains %q = %t, want %t: %s", what, part, !want, want, body)
			}
		}
	}
	requireHeaders := func(res *http.Response, want map[string]string) {
		t.Helper()
		for name, value := range want {
			if got := res.Header.Get(name); got != value {
				t.Fatalf("%s = %q, want %q", name, got, value)
			}
		}
	}

	// The funded contract shows its paid time in the registry row.
	_, registry := request(http.MethodGet, "/management/", http.StatusOK)
	requireText("funded registry row", registry, true, `· paid <time datetime="`+now.Format(time.RFC3339)+`">`)

	// GET fragment: out-of-range and unknown students are 404; a student without a contract still gets the shell.
	for _, id := range missing {
		request(http.MethodGet, billingPath(id, ""), http.StatusNotFound)
	}
	_, body := request(http.MethodGet, billingPath(plain.ID, ""), http.StatusOK)
	requireText("student without contract", body, true, fragment(plain.ID), "No billing record for this student.")
	_, body = request(http.MethodGet, billingPath(active.ID, ""), http.StatusOK)
	requireText("active student", body, true, fragment(active.ID), planName, "<dt>Tuition total</dt><dd>$625.00</dd>",
		"<dt>Status</dt><dd>Active</dd>", "<dt>Last synced</dt><dd>Never</dd>", `href="`+invoicePath(active.ID, ready.ID)+`"`)
	requireText("active student", body, false, "Billing-suspended", billingPath(active.ID, "/reinstatement"),
		`href="`+invoicePath(active.ID, draft.ID)+`"`)

	// POST reconcile refreshes the contract and answers with the fresh fragment.
	for _, id := range missing {
		request(http.MethodPost, billingPath(id, "/reconcile"), http.StatusNotFound)
	}
	_, body = request(http.MethodPost, billingPath(active.ID, "/reconcile"), http.StatusOK)
	requireText("reconciled student", body, true, fragment(active.ID), planName, "<dt>Status</dt><dd>Active</dd>")
	requireText("reconciled student", body, false, "<dt>Last synced</dt><dd>Never</dd>")

	// POST reinstatement: only a suspended student with a contract can request it.
	for _, id := range missing {
		request(http.MethodPost, billingPath(id, "/reinstatement"), http.StatusNotFound)
	}
	request(http.MethodPost, billingPath(plain.ID, "/reinstatement"), http.StatusNotFound)
	_, body = request(http.MethodPost, billingPath(active.ID, "/reinstatement"), http.StatusConflict)
	requireText("reinstating an active student", body, true, "This student is not suspended for billing.")
	_, body = request(http.MethodPost, billingPath(suspended.ID, "/reinstatement"), http.StatusOK)
	requireText("reinstatement request", body, true, fragment(suspended.ID), "<dt>Status</dt><dd>Billing-suspended</dd>",
		"Reinstatement fee: $25.00 remaining", `href="`+invoicePath(suspended.ID, fee.ID)+`"`)
	var requested billing.Suspension
	if err := db.Take(&requested, episode.ID).Error; err != nil {
		t.Fatal(err)
	}
	if requested.ReinstatementRequestedAt == nil || requested.RequestedBy == nil || *requested.RequestedBy != "admin_user" || requested.ClosedAt != nil {
		t.Fatalf("reinstatement request must be recorded for staff and leave the episode open: %+v", requested)
	}

	// GET invoice: bounds, another student's obligation and unknown obligations are 404; only a hosted invoice redirects.
	for _, path := range []string{
		invoicePath(active.ID, "0"), invoicePath(active.ID, tooBig), invoicePath(active.ID, unknown),
		invoicePath("0", ready.ID), invoicePath(tooBig, ready.ID), invoicePath(unknown, ready.ID),
		invoicePath(suspended.ID, ready.ID), invoicePath(active.ID, fee.ID), invoicePath(plain.ID, ready.ID),
	} {
		request(http.MethodGet, path, http.StatusNotFound)
	}
	_, body = request(http.MethodGet, invoicePath(active.ID, draft.ID), http.StatusConflict)
	requireText("draft invoice", body, true, "This invoice is not ready yet.")
	res, _ := request(http.MethodGet, invoicePath(active.ID, ready.ID), http.StatusSeeOther)
	if location := res.Header.Get("Location"); !strings.HasPrefix(location, "https://invoice.stripe.com/") {
		t.Fatalf("Location = %q, want a hosted invoice URL", location)
	}
	requireHeaders(res, map[string]string{"Cache-Control": "no-store"})

	// POST portal: only a student with a Stripe customer gets a link, and it is never cached or referred.
	for _, id := range missing {
		request(http.MethodPost, billingPath(id, "/portal"), http.StatusNotFound)
	}
	request(http.MethodPost, billingPath(plain.ID, "/portal"), http.StatusNotFound)
	res, body = request(http.MethodPost, billingPath(active.ID, "/portal"), http.StatusOK)
	requireText("portal", body, true, `href="https://billing.stripe.com/session/test"`)
	requireHeaders(res, map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})

	// Student table: the Status column shows each student's own badge.
	_, body = request(http.MethodGet, "/management/students", http.StatusOK)
	requireText("student table", body, true, `<th scope="col">Status</th>`)
	rowOf := func(id uint64) string {
		_, rest, _ := strings.Cut(body, fmt.Sprintf(`<tr id="student-row-%d"`, id))
		row, _, _ := strings.Cut(rest, "</tr>")
		return row
	}
	requireText("active row", rowOf(active.ID), true, `<span class="status-badge status-active">Active</span>`)
	requireText("active row", rowOf(active.ID), false, "status-suspended")
	requireText("suspended row", rowOf(suspended.ID), true, `<span class="status-badge status-suspended">Suspended</span>`)
	requireText("suspended row", rowOf(suspended.ID), false, "status-active")
}
