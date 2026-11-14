//go:build integration

package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"gorm.io/gorm"
	"projecthermes/internal/auth"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

var htmxHeader = http.Header{"Hx-Request": {"true"}}

// later keeps directly created test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

// unknownID is a well-formed management ID that names no link.
var unknownID = "999999"

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
	mux.HandleFunc("POST /v1/checkout/sessions/{id}/expire", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             r.PathValue("id"),
			"object":         "checkout.session",
			"status":         "expired",
			"payment_status": "unpaid",
		})
	})
	return billingtest.New(t, db, mux)
}

// newApp wires routes like main.go: auth owns session middleware.
func newApp(t *testing.T) *app {
	t.Helper()
	models := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}, &settings.Settings{}, &auth.User{}, &auth.Session{}}, billing.Models()...)
	db := testdb.Open(t, models...)
	store := links.New(db, nil)
	log := logging.New(io.Discard)
	manager, err := auth.New(db, false, log)
	if err != nil {
		t.Fatal(err)
	}
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

func (a *app) list(t *testing.T) []links.LinkGroup {
	t.Helper()
	groups, err := a.store.ListGroups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func contactDetails(name string) students.Details {
	return students.Details{
		Name: name, Email: "private.student@example.com", Phone: "+12025550123", Municipality: "San Juan", Gender: "Female",
	}
}

// requireRegistry compares link groups and their child enrollments without time precision noise.
func (a *app) requireRegistry(t *testing.T, want ...links.LinkGroup) {
	t.Helper()
	normalize := func(groups []links.LinkGroup) []links.LinkGroup {
		out := make([]links.LinkGroup, len(groups))
		for i, g := range groups {
			out[i].Link = links.LinkEntry{
				ID: g.Link.ID, Name: g.Link.Name, Token: g.Link.Token,
				Status: g.Link.Status, CreatedBy: g.Link.CreatedBy, CanceledBy: g.Link.CanceledBy,
				EnrollmentCount: g.Link.EnrollmentCount,
			}
			out[i].Enrollments = make([]links.EnrollmentEntry, len(g.Enrollments))
			for j, e := range g.Enrollments {
				out[i].Enrollments[j] = links.EnrollmentEntry{
					ID: e.ID, Name: e.Name, Phone: e.Phone,
					Status: e.Status, CanceledBy: e.CanceledBy,
				}
			}
		}
		return out
	}
	got := a.list(t)
	if fmt.Sprintf("%+v", normalize(got)) != fmt.Sprintf("%+v", normalize(want)) {
		t.Fatalf("registry = %+v, want %+v", got, want)
	}
}

func writePaths(id uint64) []string {
	return []string{
		"/management/links", fmt.Sprintf("/management/links/%d/cancel", id), fmt.Sprintf("/management/links/%d/rename", id),
		fmt.Sprintf("/management/enrollments/%d/cancel", id),
		"/management/settings", "/management/program-start-date", "/management/bootcamp-date",
		"/management/students", "/management/students/import",
		"/management/students/1/fields/name", "/management/students/1/billing/reconcile", "/management/students/1/billing/reinstatement", "/management/students/1/billing/portal",
	}
}

// requirePage checks a full page lists exactly want, in order.
func requirePage(t *testing.T, res *http.Response, body string, want []links.LinkGroup) {
	t.Helper()
	start, end := strings.Index(body, `<ul id="generated-links"`), strings.Index(body, "</ul>\n      <div class=\"management-empty\">")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") || start < 0 || end < start {
		t.Fatalf("status = %d, want full management page: %s", res.StatusCode, body)
	}
	requireRows(t, body[start:end], want)
}

// requireRefresh checks an htmx list refresh is not a page, leaves the status
// line alone, and has exactly want's rows, in order.
func requireRefresh(t *testing.T, res *http.Response, body string, want []links.LinkGroup) {
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

// requireRows checks parent reusable links, permanent URLs, audit history, cancellation
// availability, and child enrollments for exactly the expected groups.
func requireRows(t *testing.T, html string, want []links.LinkGroup) {
	t.Helper()
	rows := strings.Split(html, `<li class="management-link"`)[1:]
	if len(rows) != len(want) {
		t.Fatalf("rendered %d links, want %d: %s", len(rows), len(want), html)
	}
	for i, group := range want {
		row := rows[i]
		link := group.Link
		ok := strings.Contains(row, string(link.Status)) && strings.Contains(row, link.Name)
		ok = ok && strings.Contains(row, fmt.Sprintf(`href="/customer/%s"`, link.Token))
		ok = ok && strings.Contains(row, fmt.Sprintf(`id="link-url-%d"`, link.ID)) && strings.Contains(row, fmt.Sprintf(`id="link-copy-%d"`, link.ID))
		ok = ok && strings.Contains(row, `data-copy-link`)
		ok = ok && strings.Contains(row, fmt.Sprintf(`action="/management/links/%d/rename"`, link.ID))
		ok = ok && strings.Contains(row, "</time> by "+link.CreatedBy) == (link.CreatedBy != "")
		ok = ok && strings.Contains(row, "Link canceled by "+link.CanceledBy) == (link.Status == links.LinkCanceled && link.CanceledBy != "")
		cancelable := strings.Contains(row, fmt.Sprintf(`action="/management/links/%d/cancel"`, link.ID))
		ok = ok && cancelable == link.Cancelable()
		if !ok {
			t.Fatalf("link row %d does not render %+v: %s", i, link, row)
		}
		hasTree := strings.Contains(row, fmt.Sprintf(`id="link-tree-%d"`, link.ID))
		if hasTree != (len(group.Enrollments) > 0) {
			t.Fatalf("link %d tree present = %v, enrollments = %d: %s", i, hasTree, len(group.Enrollments), row)
		}
		erows := strings.Split(row, `<li class="management-enrollment"`)[1:]
		if len(erows) != len(group.Enrollments) {
			t.Fatalf("link %d rendered %d enrollments, want %d: %s", i, len(erows), len(group.Enrollments), row)
		}
		for j, entry := range group.Enrollments {
			erow := erows[j]
			eok := strings.Contains(erow, string(entry.Status)) && strings.Contains(erow, entry.Name)
			if entry.Phone != "" {
				eok = eok && strings.Contains(erow, `<span class="management-link-phone">`+escapedHTML(entry.Phone)+`</span>`)
			}
			eok = eok && !strings.Contains(erow, "tel:")
			eCancelable := strings.Contains(erow, fmt.Sprintf(`action="/management/enrollments/%d/cancel"`, entry.ID))
			eok = eok && eCancelable == entry.Cancelable()
			if entry.Cancelable() {
				wantLabel := "Cancel enrollment"
				if entry.Status == links.Expired {
					wantLabel = "Clear PIN"
				}
				eok = eok && strings.Contains(erow, wantLabel)
			}
			eok = eok && strings.Contains(erow, "Canceled by "+entry.CanceledBy) == (entry.Status == links.Canceled && entry.CanceledBy != "")
			if entry.SubmittedAt != nil {
				eok = eok && strings.Contains(erow, `datetime="`+entry.SubmittedAt.UTC().Format(time.RFC3339)+`"`)
			}
			wantExpiry := entry.SubmittedAt != nil && (entry.Status == links.AwaitingPayment || entry.Status == links.Expired)
			eok = eok && strings.Contains(erow, `datetime="`+entry.ExpiresAt.UTC().Format(time.RFC3339)+`"`) == wantExpiry
			eok = eok && strings.Contains(erow, "Agreement accepted") == (entry.TermsAcceptedAt != nil)
			if !eok {
				t.Fatalf("enrollment %d.%d does not render %+v: %s", i, j, entry, erow)
			}
		}
	}
}

func TestUnauthorizedRequestsCannotUseManagement(t *testing.T) {
	a := newApp(t)
	existingLink, err := a.store.CreateLink(t.Context(), "seed", "", later)
	if err != nil {
		t.Fatal(err)
	}
	e, cookie, err := a.store.StartEnrollment(t.Context(), existingLink.Token, "")
	if err != nil || e == nil {
		t.Fatalf("start: %+v, %v", e, err)
	}
	if saved, err := a.store.Submit(t.Context(), existingLink.Token, cookie, contactDetails("Private student"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit private student: saved=%v, err=%v", saved, err)
	}
	acceptAgreement(t, a, existingLink.Token, cookie)
	seeded, err := settings.New(a.db).Current(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, e.ID, now)
	}); err != nil {
		t.Fatal(err)
	}
	existing := links.LinkGroup{
		Link: existingLink,
		Enrollments: []links.EnrollmentEntry{{
			ID: e.ID, Name: "Private student",
			Phone: contactDetails("").Phone, PaidAt: &now, Status: links.Finished,
		}},
	}
	existing.Link.EnrollmentCount = 1
	const privateStudentName = "Private student"
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
			if strings.Contains(body, existingLink.Token) {
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
						strings.Contains(body, "<form") ||
						strings.Contains(body, existingLink.Token) || strings.Contains(body, privateStudentName) || strings.Contains(body, "student-details") ||
						strings.Contains(body, "student-table") {
						t.Fatalf("unauthorized management read: status=%d, %s=%q, body=%s",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), body)
					}
				})
			}
		}
		for _, path := range writePaths(existingLink.ID) {
			for _, mode := range modes {
				t.Run(b.name+"/"+mode.name+path, func(t *testing.T) {
					res, body := a.send(t, b.browser, http.MethodPost, path, url.Values{
						"value": {"Unauthorized name"}, "lifetime": {"10"}, "program_start_date": {"2026-12-01"}, "bootcamp_date": {"2026-12-05"},
					}, mode.header)
					if res.StatusCode != mode.status || res.Header.Get(mode.redirectHeader) != "/management/" {
						t.Fatalf("status = %d, %s = %q; want %d to /management/",
							res.StatusCode, mode.redirectHeader, res.Header.Get(mode.redirectHeader), mode.status)
					}
					// htmx would swap a returned sign-in form into the link list.
					if strings.Contains(body, "<form") || strings.Contains(body, existingLink.Token) || strings.Contains(body, privateStudentName) {
						t.Fatalf("unauthorized response body = %s", body)
					}
					a.requireRegistry(t, existing)
					student, err := students.New(a.db, nil).Get(t.Context(), 1)
					if err != nil || student == nil || student.Name != privateStudentName {
						t.Fatalf("unauthorized write changed student: student=%+v, err=%v", student, err)
					}
				})
			}
		}
	}
	current, err := settings.New(a.db).Current(t.Context())
	if err != nil || current.ProgramStartDate != seeded.ProgramStartDate || current.BootcampDate != seeded.BootcampDate ||
		!current.UpdatedAt.Equal(seeded.UpdatedAt) || current.LinkLifetimeDays != settings.DefaultLinkLifetimeDays {
		t.Fatalf("unauthorized request changed settings: current=%+v, seeded=%+v, err=%v", current, seeded, err)
	}
}

func TestRegistrySeparatesTrash(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	past := time.Now().Add(-time.Minute)
	mkToken := func(tag string) string {
		return strings.ReplaceAll(fmt.Sprintf("%-43s", strings.ReplaceAll(tag, " ", "_")), " ", "0")[:43]
	}

	activeLink := links.Link{Name: "Active Reusable", Token: mkToken("active_link"), CreatedBy: "staff", ExpiresAt: later, EnrollmentCount: 3}
	canceledLink := links.Link{Name: "Canceled Reusable", Token: mkToken("canceled_link"), CreatedBy: "staff", ExpiresAt: later, CanceledAt: &past, CanceledBy: "staff", EnrollmentCount: 1}
	expiredLink := links.Link{Name: "Expired Reusable", Token: mkToken("expired_link"), CreatedBy: "staff", ExpiresAt: past, EnrollmentCount: 1}
	for _, l := range []*links.Link{&activeLink, &canceledLink, &expiredLink} {
		if err := a.db.WithContext(t.Context()).Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}

	var mainActive, trashActive, trashCanceled, trashExpired []links.EnrollmentEntry
	for _, tc := range []struct {
		name   string
		link   *links.Link
		row    links.Enrollment
		status links.Status
		trash  bool
	}{
		{"in progress", &activeLink, links.Enrollment{ExpiresAt: later, Details: contactDetails("Draft")}, links.InProgress, false},
		{"completed", &activeLink, links.Enrollment{ExpiresAt: past.Add(7 * 24 * time.Hour), Details: contactDetails("Completed student"), SubmittedAt: &past, PaidAt: &past}, links.Finished, false},
		{"expired draft", &activeLink, links.Enrollment{ExpiresAt: past, Details: contactDetails("Expired draft")}, links.Expired, true},
		{"canceled draft", &canceledLink, links.Enrollment{ExpiresAt: later, Details: contactDetails("Canceled draft"), CanceledAt: &past, CanceledBy: "staff"}, links.Canceled, true},
		{"expired link draft", &expiredLink, links.Enrollment{ExpiresAt: past, Details: contactDetails("Expired link draft")}, links.Expired, true},
	} {
		digest := sha256.Sum256([]byte(tc.name))
		tc.row.LinkID, tc.row.SessionHash = tc.link.ID, digest[:]
		if err := a.db.WithContext(t.Context()).Create(&tc.row).Error; err != nil {
			t.Fatal(err)
		}
		if tc.row.PaidAt != nil {
			student := students.Student{EnrollmentID: &tc.row.ID, Details: tc.row.Details, CreatedAt: *tc.row.PaidAt, Cohort: "2026-11"}
			if err := a.db.WithContext(t.Context()).Create(&student).Error; err != nil {
				t.Fatal(err)
			}
		}
		entry := links.EnrollmentEntry{
			ID: tc.row.ID, Name: tc.row.Name, Phone: tc.row.Phone, Status: tc.status,
			ExpiresAt: tc.row.ExpiresAt, SubmittedAt: tc.row.SubmittedAt, PaidAt: tc.row.PaidAt, CanceledBy: tc.row.CanceledBy,
		}
		switch {
		case tc.link == &activeLink && !tc.trash:
			mainActive = append(mainActive, entry)
		case tc.link == &activeLink && tc.trash:
			trashActive = append(trashActive, entry)
		case tc.link == &canceledLink:
			trashCanceled = append(trashCanceled, entry)
		case tc.link == &expiredLink:
			trashExpired = append(trashExpired, entry)
		}
	}

	activeEntry := links.LinkEntry{ID: activeLink.ID, Name: activeLink.Name, Token: activeLink.Token, Status: links.LinkActive, CreatedBy: "staff", EnrollmentCount: 3}
	canceledEntry := links.LinkEntry{ID: canceledLink.ID, Name: canceledLink.Name, Token: canceledLink.Token, Status: links.LinkCanceled, CreatedBy: "staff", CanceledBy: "staff", EnrollmentCount: 1}
	expiredEntry := links.LinkEntry{ID: expiredLink.ID, Name: expiredLink.Name, Token: expiredLink.Token, Status: links.LinkExpired, CreatedBy: "staff", EnrollmentCount: 1}

	mainWant := []links.LinkGroup{{Link: activeEntry, Enrollments: mainActive}}
	trashWant := []links.LinkGroup{
		{Link: activeEntry, Enrollments: trashActive},
		{Link: canceledEntry, Enrollments: trashCanceled},
		{Link: expiredEntry, Enrollments: trashExpired},
	}
	for _, tc := range []struct {
		name, path string
		want       []links.LinkGroup
	}{
		{"active and completed", "/management/", mainWant},
		{"canceled and expired", "/management/trash", trashWant},
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
	a.requireRegistry(t,
		links.LinkGroup{Link: activeEntry, Enrollments: append(mainActive, trashActive...)},
		links.LinkGroup{Link: canceledEntry, Enrollments: trashCanceled},
		links.LinkGroup{Link: expiredEntry, Enrollments: trashExpired},
	)
}

func TestRegistryProjectsNewAndRefreshedRows(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	created, err := a.store.CreateLink(t.Context(), "staff", "Spring Cohort", later)
	if err != nil {
		t.Fatal(err)
	}
	e, cookie, err := a.store.StartEnrollment(t.Context(), created.Token, "")
	if err != nil || e == nil {
		t.Fatal(err)
	}
	details := contactDetails("Registry student")
	details.Email, details.Municipality = "private.registry@example.com", "Loíza"
	for _, field := range students.Fields() {
		if saved, err := a.store.SaveDraft(t.Context(), created.Token, cookie, field.Key, valueOf(field, details)); err != nil || saved != links.Saved {
			t.Fatalf("save registry fixture %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}

	// Ordinary creation renders both links with their permanent URLs.
	res, body := a.send(t, staff, http.MethodPost, "/management/links", nil, nil)
	groups := a.list(t)
	if len(groups) != 2 {
		t.Fatalf("create left %d links, want 2", len(groups))
	}
	requirePage(t, res, body, groups)
	requireRegistryPrivacy(t, body, details)

	// A later draft changes visible phone on refresh.
	if saved, err := a.store.SaveDraft(t.Context(), created.Token, cookie, "phone", "+17872345678"); err != nil || saved != links.Saved {
		t.Fatalf("update registry phone: saved=%v, err=%v", saved, err)
	}
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, htmxHeader)
	groups = a.list(t)
	if groups[0].Enrollments[0].Phone != "+17872345678" {
		t.Fatalf("refreshed phone = %q", groups[0].Enrollments[0].Phone)
	}
	requireRefresh(t, res, body, groups)
	requireRegistryPrivacy(t, body, details)

	// Cancel enrollment moves child enrollment to Trash while keeping parent link active.
	res, body = a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/enrollments/%d/cancel", e.ID), nil, htmxHeader)
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

	// HTMX link creation acknowledges and triggers refresh.
	res, body = a.send(t, staff, http.MethodPost, "/management/links", nil, htmxHeader)
	requireAcknowledged(t, res, body)
	requireRegistryPrivacy(t, body, details)
	if len(a.list(t)) != 3 {
		t.Fatalf("HTMX create left %d links, want 3", len(a.list(t)))
	}
}

func TestRegistryArchivesPaidEnrollments(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, fixture := range []struct {
		name                string
		createdAge, paidAge time.Duration
	}{
		{"Recent completion", 30 * 24 * time.Hour, 23 * time.Hour},
		{"Archived completion", 26 * time.Hour, 25 * time.Hour},
		{"Expired completion", 10 * 24 * time.Hour, 8 * 24 * time.Hour},
	} {
		created, err := a.store.CreateLink(t.Context(), "staff", fixture.name, later)
		if err != nil {
			t.Fatal(err)
		}
		e, cookie, err := a.store.StartEnrollment(t.Context(), created.Token, "")
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := a.store.Submit(t.Context(), created.Token, cookie, contactDetails(fixture.name), ""); err != nil || saved != links.Saved {
			t.Fatalf("submit %s: saved=%v, err=%v", fixture.name, saved, err)
		}
		acceptAgreement(t, a, created.Token, cookie)
		paid := now.Add(-fixture.paidAge)
		if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
			if err := links.Enroll(tx, e.ID, paid); err != nil {
				return err
			}
			return tx.Model(&links.Enrollment{}).Where("id = ?", e.ID).
				Updates(map[string]any{
					"created_at": now.Add(-fixture.createdAge), "submitted_at": paid,
					"paid_at":    paid,
					"expires_at": paid.Add(7 * 24 * time.Hour),
				}).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	draftLink, err := a.store.CreateLink(t.Context(), "staff", "Draft Link", later)
	if err != nil {
		t.Fatal(err)
	}
	draftE, draftCookie, err := a.store.StartEnrollment(t.Context(), draftLink.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.SaveDraft(t.Context(), draftLink.Token, draftCookie, "name", "Old unfinished form"); err != nil || saved != links.Saved {
		t.Fatalf("save old draft: saved=%v, err=%v", saved, err)
	}
	if err := a.db.WithContext(t.Context()).Model(&links.Enrollment{}).
		Where("id = ?", draftE.ID).Update("created_at", now.Add(-28*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	all := a.list(t)
	// On main, all 4 active links show up, with only Recent completion and Old unfinished form as visible enrollments.
	mainWant := make([]links.LinkGroup, len(all))
	byName := make(map[string]links.EnrollmentEntry)
	for i, g := range all {
		mainWant[i].Link = g.Link
		for _, e := range g.Enrollments {
			byName[e.Name] = e
			if e.Name == "Recent completion" || e.Name == "Old unfinished form" {
				mainWant[i].Enrollments = append(mainWant[i].Enrollments, e)
			}
		}
	}
	res, body := a.send(t, staff, http.MethodGet, "/management/", nil, nil)
	requirePage(t, res, body, mainWant)
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

	// Attempting to cancel archived finished enrollments leaves them untouched.
	for _, name := range []string{"Archived completion", "Expired completion"} {
		res, body = a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/enrollments/%d/cancel", byName[name].ID), nil, htmxHeader)
		requireAcknowledged(t, res, body)
	}
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, htmxHeader)
	requireRefresh(t, res, body, mainWant)
	a.requireRegistry(t, all...)
	retained, err := students.New(a.db, nil).List(t.Context(), students.Filter{})
	if err != nil || len(retained) != 3 {
		t.Fatalf("stale cancel lost retained students: students=%+v, err=%v", retained, err)
	}
}

func TestStaffShareLinkStatuses(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	worker := a.signIn(t, "worker")
	cancelLink := func(t *testing.T, browser *http.Client, id string, header http.Header) (*http.Response, string) {
		t.Helper()
		return a.send(t, browser, http.MethodPost, "/management/links/"+id+"/cancel", nil, header)
	}
	cancelEnrollment := func(t *testing.T, browser *http.Client, id string, header http.Header) (*http.Response, string) {
		t.Helper()
		return a.send(t, browser, http.MethodPost, "/management/enrollments/"+id+"/cancel", nil, header)
	}

	if res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmxHeader); res.StatusCode != http.StatusOK {
		t.Fatalf("htmx create: %d: %s", res.StatusCode, body)
	} else {
		requireAcknowledged(t, res, body)
	}
	if res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("ordinary create: %d: %s", res.StatusCode, body)
	} else {
		requirePage(t, res, body, a.list(t))
	}
	groups := a.list(t)
	if len(groups) != 2 {
		t.Fatalf("created %d links, want 2", len(groups))
	}

	// Two visitors start on link 1; visitor A submits, visitor B saves a draft.
	e1A, c1A, err := a.store.StartEnrollment(t.Context(), groups[0].Link.Token, "")
	if err != nil || e1A == nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), groups[0].Link.Token, c1A, contactDetails("Ana Pérez"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit: %v, %v", saved, err)
	}
	e1B, c1B, err := a.store.StartEnrollment(t.Context(), groups[0].Link.Token, "")
	if err != nil || e1B == nil {
		t.Fatal(err)
	}
	if saved, err := a.store.SaveDraft(t.Context(), groups[0].Link.Token, c1B, "name", "Retained draft"); err != nil || saved != links.Saved {
		t.Fatalf("draft: %v, %v", saved, err)
	}

	groups = a.list(t)
	if groups[0].Enrollments[0].Status != links.AwaitingPayment || groups[0].Enrollments[1].Status != links.InProgress {
		t.Fatalf("unexpected statuses: %+v", groups[0].Enrollments)
	}
	for _, browser := range []*http.Client{admin, worker} {
		res, body := a.send(t, browser, http.MethodGet, "/management/", nil, nil)
		requirePage(t, res, body, groups)
	}

	// Worker cancels submitted AwaitingPayment enrollment e1A and records worker as canceler.
	res, body := cancelEnrollment(t, worker, fmt.Sprint(e1A.ID), nil)
	groups[0].Enrollments[0].Status, groups[0].Enrollments[0].CanceledBy = links.Canceled, "worker"
	a.requireRegistry(t, groups...)
	requirePage(t, res, body, []links.LinkGroup{{Link: groups[0].Link, Enrollments: groups[0].Enrollments[1:2]}, groups[1]})
	// Worker cancels draft enrollment e1B; first canceler audit is preserved when admin retries.
	res, body = cancelEnrollment(t, worker, fmt.Sprint(e1B.ID), htmxHeader)
	requireAcknowledged(t, res, body)
	groups[0].Enrollments[1].Status, groups[0].Enrollments[1].CanceledBy = links.Canceled, "worker"
	a.requireRegistry(t, groups...)

	res, body = cancelEnrollment(t, admin, fmt.Sprint(e1B.ID), htmxHeader)
	requireAcknowledged(t, res, body)
	a.requireRegistry(t, groups...)

	// Trash shows link 0 with both canceled enrollments e1A and e1B.
	res, body = a.send(t, admin, http.MethodGet, "/management/trash", nil, nil)
	requirePage(t, res, body, []links.LinkGroup{{Link: groups[0].Link, Enrollments: groups[0].Enrollments}})

	// Canceling link 1 revokes link 1 and records admin as canceler; repeating preserves admin.
	res, body = cancelLink(t, admin, fmt.Sprint(groups[1].Link.ID), htmxHeader)
	requireAcknowledged(t, res, body)
	groups[1].Link.Status, groups[1].Link.CanceledBy = links.LinkCanceled, "admin"
	a.requireRegistry(t, groups...)

	res, body = cancelLink(t, worker, fmt.Sprint(groups[1].Link.ID), htmxHeader)
	requireAcknowledged(t, res, body)
	a.requireRegistry(t, groups...)

	// Unknown/malformed/raw-token/beyond-BIGINT cancel IDs return 204 for HTMX and unchanged main page for ordinary.
	for _, tc := range []struct{ name, id string }{
		{"unknown", unknownID},
		{"malformed", "not-issued"},
		{"raw customer token", groups[0].Link.Token},
		{"beyond BIGINT", "9223372036854775808"},
	} {
		t.Run(tc.name+" cancel ID", func(t *testing.T) {
			if res, body := cancelLink(t, admin, tc.id, htmxHeader); res.StatusCode != http.StatusNoContent || body != "" {
				t.Fatalf("HTMX link cancel: %d, %s", res.StatusCode, body)
			}
			if res, body := cancelEnrollment(t, admin, tc.id, htmxHeader); res.StatusCode != http.StatusNoContent || body != "" {
				t.Fatalf("HTMX enrollment cancel: %d, %s", res.StatusCode, body)
			}
			res, body := cancelLink(t, admin, tc.id, nil)
			requirePage(t, res, body, []links.LinkGroup{{Link: groups[0].Link}})
		})
	}
}

// Database failures answer 500 with no list, row, or notice that htmx could
// swap in as if the request had worked.
func TestStoreFailuresAreServerErrors(t *testing.T) {
	db := testdb.Open(t, &links.Link{}, &links.Enrollment{}, &students.Student{})
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
		{"bootcamp date", http.MethodPost, "/management/bootcamp-date", nil, url.Values{"bootcamp_date": {"2026-12-05"}}},
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
	create := func() links.LinkEntry {
		t.Helper()
		if res, body := a.send(t, admin, http.MethodPost, "/management/links", nil, htmxHeader); res.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d: %s", res.StatusCode, body)
		}
		groups := a.list(t)
		return groups[len(groups)-1].Link
	}
	requireLifetime := func(entry links.LinkEntry, days int) {
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
	if already := a.list(t)[0].Link; already.ID != sent.ID {
		t.Fatalf("registry order changed: %+v", already)
	} else {
		requireLifetime(already, settings.DefaultLinkLifetimeDays) // Moving the slider never changes links already sent.
	}
}

func TestProgramStartDateSavesWithoutChangingExistingEnrollment(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	config := settings.New(a.db)
	pending, err := a.store.CreateLink(t.Context(), "admin", "", later)
	if err != nil {
		t.Fatal(err)
	}
	_, pendingCookie, err := a.store.StartEnrollment(t.Context(), pending.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore, _, err := a.store.LookupSession(t.Context(), pending.Token, pendingCookie)
	if err != nil || pendingBefore == nil {
		t.Fatalf("pending enrollment: %+v, err=%v", pendingBefore, err)
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

	enrolled, err := a.store.CreateLink(t.Context(), "admin", "", later)
	if err != nil {
		t.Fatal(err)
	}
	_, enrolledCookie, err := a.store.StartEnrollment(t.Context(), enrolled.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), enrolled.Token, enrolledCookie, contactDetails("Accepted student"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	if ok, err := a.store.AcceptTerms(t.Context(), enrolled.Token, enrolledCookie, "2026-12-01"); err != nil || !ok {
		t.Fatalf("accept agreement: accepted=%v, err=%v", ok, err)
	}
	accepted, _, err := a.store.LookupSession(t.Context(), enrolled.Token, enrolledCookie)
	if err != nil || accepted == nil || accepted.TermsAcceptedAt == nil {
		t.Fatalf("accepted enrollment: %+v, err=%v", accepted, err)
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
	after, _, err := a.store.LookupSession(t.Context(), enrolled.Token, enrolledCookie)
	if err != nil || after == nil {
		t.Fatalf("lookup after: %+v, err=%v", after, err)
	}
	if after.ProgramStartDate != "2026-12-01" || after.TermsAcceptedAt == nil ||
		!after.TermsAcceptedAt.Equal(*accepted.TermsAcceptedAt) || !after.ExpiresAt.Equal(accepted.ExpiresAt) || after.Details != accepted.Details {
		t.Fatalf("settings changed accepted enrollment: %+v", after)
	}
	pendingAfter, _, err := a.store.LookupSession(t.Context(), pending.Token, pendingCookie)
	if err != nil || pendingAfter == nil {
		t.Fatalf("lookup pending after: %+v, err=%v", pendingAfter, err)
	}
	if !pendingAfter.ExpiresAt.Equal(pendingBefore.ExpiresAt) ||
		pendingAfter.ProgramStartDate != "" || pendingAfter.TermsAcceptedAt != nil {
		t.Fatalf("settings changed existing unfinished enrollment: %+v", pendingAfter)
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

func TestBootcampDateSettings(t *testing.T) {
	a := newApp(t)
	admin := a.signIn(t, "admin")
	config := settings.New(a.db)
	if err := config.SetProgramStartDate(t.Context(), "2027-01-04", "admin"); err != nil {
		t.Fatal(err)
	}

	res, body := a.send(t, admin, http.MethodGet, "/management/settings", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `action="/management/bootcamp-date"`) ||
		!strings.Contains(body, `name="bootcamp_date" type="date" value="" required`) {
		t.Fatalf("settings page must show empty bootcamp date form: status=%d: %s", res.StatusCode, body)
	}

	res, body = a.send(t, admin, http.MethodPost, "/management/bootcamp-date", url.Values{"bootcamp_date": {"2026-12-05"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/management/settings?bootcamp-date-saved" {
		t.Fatalf("save bootcamp date: status=%d, Location=%q: %s", res.StatusCode, res.Header.Get("Location"), body)
	}
	res, body = a.send(t, admin, http.MethodGet, "/management/settings?bootcamp-date-saved", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `name="bootcamp_date" type="date" value="2026-12-05" required`) ||
		!strings.Contains(body, `<p role="status">Saved.</p>`) || !strings.Contains(body, "Last changed by admin") {
		t.Fatalf("saved settings page must confirm and show bootcamp date: status=%d: %s", res.StatusCode, body)
	}

	res, body = a.send(t, admin, http.MethodPost, "/management/settings", url.Values{"lifetime": {"10"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save lifetime: status=%d: %s", res.StatusCode, body)
	}
	res, body = a.send(t, admin, http.MethodPost, "/management/bootcamp-date", url.Values{"bootcamp_date": {"2027-02-06"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("update bootcamp date: status=%d: %s", res.StatusCode, body)
	}
	saved, err := config.Current(t.Context())
	if err != nil || saved.BootcampDate != "2027-02-06" || saved.ProgramStartDate != "2027-01-04" || saved.LinkLifetimeDays != 10 {
		t.Fatalf("independent writes overwrote settings: %+v, err=%v", saved, err)
	}

	for _, tc := range []struct {
		name, query, body, attempted string
	}{
		{name: "missing"},
		{name: "query is not posted", query: "?bootcamp_date=2027-03-06"},
		{name: "empty", body: "bootcamp_date="},
		{name: "invalid calendar date", body: "bootcamp_date=2026-02-30", attempted: "2026-02-30"},
		{name: "duplicate", body: "bootcamp_date=2027-03-06&bootcamp_date=2027-03-07", attempted: "2027-03-06"},
		{name: "malformed encoding", body: "bootcamp_date=2027-03-06&bad=%zz", attempted: "2027-03-06"},
		{name: "body limit", body: "bootcamp_date=2027-03-06&padding=" + strings.Repeat("x", 1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, text := a.sendRaw(t, admin, http.MethodPost, "/management/bootcamp-date"+tc.query, tc.body, nil)
			if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(text, `id="bootcamp-error" role="alert"`) ||
				!strings.Contains(text, `name="bootcamp_date" type="date" value="`+tc.attempted+`" required`) {
				t.Fatalf("invalid form must retain attempted date with error: status=%d: %s", res.StatusCode, text)
			}
			current, err := config.Current(t.Context())
			if err != nil || current.BootcampDate != "2027-02-06" || current.ProgramStartDate != "2027-01-04" || current.LinkLifetimeDays != 10 {
				t.Fatalf("invalid form changed saved settings: %+v, err=%v", current, err)
			}
		})
	}

	res, body = a.send(t, admin, http.MethodGet, "/management/settings", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `name="bootcamp_date" type="date" value="2027-02-06" required`) ||
		strings.Contains(body, `id="bootcamp-error"`) {
		t.Fatalf("reload must show unchanged saved date without error: status=%d: %s", res.StatusCode, body)
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
			EnrollmentID: *student.EnrollmentID, Reference: rand.Text(), PlanID: "three", PlanName: planName,
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
	fee := seedObligation(contractB, billing.Obligation{Kind: "reinstatement", AmountCents: 2500,
		AmountRemainingCents: 2500, SuspensionID: &episode.ID, StripeInvoiceID: ptr("inv_fee"),
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
	requireText("funded registry row", registry, true, `Paid <time datetime="`+now.Format(time.RFC3339)+`">`)

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
	requireText("active row", rowOf(active.ID), true, `<span class="status-badge" data-tone="ok">Active</span>`)
	requireText("active row", rowOf(active.ID), false, `data-tone="danger"`)
	requireText("suspended row", rowOf(suspended.ID), true, `<span class="status-badge" data-tone="danger">Suspended</span>`)
	requireText("suspended row", rowOf(suspended.ID), false, `data-tone="ok"`)
}

func TestRenameLinkAndBillingSafeCancelEndpoints(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	created, err := a.store.CreateLink(t.Context(), "staff", "Original", later)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Inline rename: valid HTMX, invalid 422, malformed 400, unknown or beyond-BIGINT 204.
	res, body := a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/links/%d/rename", created.ID), url.Values{"name": {"  October Cohort  "}}, htmxHeader)
	requireAcknowledged(t, res, body)
	if groups := a.list(t); groups[0].Link.Name != "October Cohort" {
		t.Fatalf("renamed link = %q, want October Cohort", groups[0].Link.Name)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty", "name=   ", http.StatusUnprocessableEntity},
		{"too long", "name=" + strings.Repeat("x", 101), http.StatusUnprocessableEntity},
		{"missing param", "other=1", http.StatusBadRequest},
	} {
		res, body = a.sendRaw(t, staff, http.MethodPost, fmt.Sprintf("/management/links/%d/rename", created.ID), tc.body, htmxHeader)
		if res.StatusCode != tc.status {
			t.Fatalf("rename %s: status=%d, want %d: %s", tc.name, res.StatusCode, tc.status, body)
		}
	}
	for _, id := range []string{unknownID, "9223372036854775808"} {
		if res, _ = a.send(t, staff, http.MethodPost, "/management/links/"+id+"/rename", url.Values{"name": {"Missing"}}, htmxHeader); res.StatusCode != http.StatusNoContent {
			t.Fatalf("rename %s status=%d, want 204", id, res.StatusCode)
		}
	}

	// 2. Billing-safe enrollment cancel:
	// (a) AwaitingPayment with in-flight processing attempt returns 409 Conflict.
	e1, c1, err := a.store.StartEnrollment(t.Context(), created.Token, "")
	if err != nil || e1 == nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), created.Token, c1, contactDetails("Processing Customer"), "123456"); err != nil || saved != links.Saved {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	contract1 := billing.Contract{EnrollmentID: e1.ID, Reference: rand.Text(), PlanID: "full", PlanName: "Pago Único", InstallmentCount: 1, TotalCents: 60000}
	if err := a.db.Create(&contract1).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(&billing.CheckoutAttempt{ContractID: contract1.ID, Reference: rand.Text(), PlanID: "full", FirstAmountCents: 60000, State: "processing", CreationStartedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	res, body = a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/enrollments/%d/cancel", e1.ID), nil, htmxHeader)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "payment is in progress") {
		t.Fatalf("in-flight cancel: status=%d, body=%s", res.StatusCode, body)
	}

	// (b) AwaitingPayment with open session expires Stripe session, cancels enrollment, and frees PIN.
	sessID := "cs_open_mgmt"
	if err := a.db.Model(&billing.CheckoutAttempt{}).Where("contract_id = ?", contract1.ID).
		Updates(map[string]any{"state": "open", "stripe_session_id": sessID}).Error; err != nil {
		t.Fatal(err)
	}
	res, body = a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/enrollments/%d/cancel", e1.ID), nil, htmxHeader)
	requireAcknowledged(t, res, body)
	if !strings.Contains(body, "Enrollment canceled.") {
		t.Fatalf("expected Enrollment canceled notice: %s", body)
	}

	// (c) Expired submission with PIN shows "Clear PIN" in Trash and clears PIN while staying Expired.
	e2, c2, err := a.store.StartEnrollment(t.Context(), created.Token, "")
	if err != nil || e2 == nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), created.Token, c2, contactDetails("Expired Pin Customer"), "123456"); err != nil || saved != links.Saved {
		t.Fatal(err)
	}
	if err := a.db.Model(&links.Enrollment{}).Where("id = ?", e2.ID).Update("expires_at", now.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	_, trashBody := a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	if !strings.Contains(trashBody, "Clear PIN") || !strings.Contains(trashBody, fmt.Sprintf(`action="/management/enrollments/%d/cancel"`, e2.ID)) {
		t.Fatalf("trash missing Clear PIN action for expired submission with PIN: %s", trashBody)
	}
	res, body = a.send(t, staff, http.MethodPost, fmt.Sprintf("/management/enrollments/%d/cancel", e2.ID), nil, htmxHeader)
	requireAcknowledged(t, res, body)
	if !strings.Contains(body, "Expired PIN cleared.") {
		t.Fatalf("expected Expired PIN cleared notice: %s", body)
	}
	_, trashBody = a.send(t, staff, http.MethodGet, "/management/trash", nil, nil)
	if strings.Contains(trashBody, "Clear PIN") {
		t.Fatalf("trash kept Clear PIN button after PIN was cleared: %s", trashBody)
	}
}

func TestDashboardCountsReusableLinksAndEnrollments(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	now := time.Now().UTC()

	// 1 active recent link with 2 enrollments (1 InProgress, 1 Submitted+Paid), plus 1 expired recent link with 0 enrollments, plus 1 old active link.
	activeLink, err := a.store.CreateLink(t.Context(), "staff", "Active", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CreateLink(t.Context(), "staff", "Expired", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	oldLink, err := a.store.CreateLink(t.Context(), "staff", "Old", later)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.Model(&links.Link{}).Where("id = ?", oldLink.ID).Update("created_at", now.Add(-40*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store.StartEnrollment(t.Context(), activeLink.Token, ""); err != nil {
		t.Fatal(err)
	}
	eSub, cSub, err := a.store.StartEnrollment(t.Context(), activeLink.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := a.store.Submit(t.Context(), activeLink.Token, cSub, contactDetails("Dashboard Student"), "556677"); err != nil || saved != links.Saved {
		t.Fatal(err)
	}
	acceptAgreement(t, a, activeLink.Token, cSub)
	if err := a.db.Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, eSub.ID, now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(&billing.Contract{
		EnrollmentID: eSub.ID, Reference: rand.Text(), PlanID: "full", PlanName: "Pago Único",
		InstallmentCount: 1, TotalCents: 60000, FundedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	res, body := a.send(t, staff, http.MethodGet, "/management/dashboard", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard GET status = %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{
		`<dt>Links created</dt><dd>2</dd>`,
		`<dt>Enrollments started</dt><dd>2</dd>`,
		`<dt>Forms completed</dt><dd>1</dd>`,
		`<dt>Paid enrollments</dt><dd>1</dd>`,
		`<dt>Active links</dt><dd>2<small>`,
		`<dt>In progress</dt><dd>1<small>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %s in %s", want, body)
		}
	}
	res, frag := a.send(t, staff, http.MethodGet, "/management/dashboard", nil, htmxHeader)
	if res.StatusCode != http.StatusOK || strings.Contains(frag, "<!doctype html>") || !strings.Contains(frag, `<dt>Links created</dt><dd>2</dd>`) {
		t.Fatalf("dashboard HTMX fragment invalid: %d, %s", res.StatusCode, frag)
	}
}
