//go:build integration

package customer

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"projecthermes/internal/billing"
	"projecthermes/internal/billing/billingtest"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

// fixture is one fresh usable link with a started browser session served by the customer routes.
type fixture struct {
	db     *gorm.DB
	store  *links.Store
	entry  links.LinkEntry
	eID    uint64
	cookie string
	path   string
	mux    http.Handler
}

func (f fixture) sessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: f.cookie}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db := customerDatabase(t)
	store := links.New(db, nil)
	entry, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	started, cookie, err := store.StartEnrollment(t.Context(), entry.Token, "")
	if err != nil || started == nil || cookie == "" {
		t.Fatalf("start enrollment: %+v, %q, %v", started, cookie, err)
	}
	return fixture{db, store, entry, started.ID, cookie, "/customer/" + entry.Token, newMux(t, db)}
}

func TestErrorResponsesKeepStudentForm(t *testing.T) {
	f := newFixture(t)
	existing := contactDetails("Existing name")
	saveDetails(t, f.store, f.entry.Token, f.cookie, existing)
	muxes := map[bool]http.Handler{
		false: f.mux,
		true:  newMux(t, failingDB(t, f.db)),
	}
	attempted := contactForm("  Ada Lovelace  ")
	attempted.Set("email", "  Ada+private@EXAMPLE.com  ")
	attempted.Set("email_confirm", " Ada+private@example.COM ")
	attempted.Set("phone", " (202) 555-0123 ")
	attempted.Set("municipality", " Bayamón ")
	cases := []struct {
		name            string
		method          string
		body            string
		status          int
		databaseFailure bool
		values          url.Values
	}{
		{"malformed form", http.MethodPost, "name=%zz", http.StatusBadRequest, false, detailsForm(existing)},
		{"oversized ASCII form", http.MethodPost, "name=" + strings.Repeat("a", 4100), http.StatusRequestEntityTooLarge, false, detailsForm(existing)},
		{"oversized encoded Unicode form", http.MethodPost, url.Values{"name": {strings.Repeat("é", 683)}}.Encode(), http.StatusRequestEntityTooLarge, false, detailsForm(existing)},
		{"database failure on submission", http.MethodPost, attempted.Encode(), http.StatusInternalServerError, true, attempted},
		{"database failure on entry", http.MethodGet, "", http.StatusInternalServerError, true, detailsForm(students.Details{})},
	}
	for _, tc := range cases {
		for _, htmx := range []bool{false, true} {
			t.Run(tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
				target := f.path
				if tc.method == http.MethodGet {
					target += "/forms"
				}
				w := customerRequest(t, muxes[tc.databaseFailure], tc.method, target, tc.body, htmx, f.sessionCookie())
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				body := w.Body.String()
				requireFormError(t, body, f.path, !htmx || tc.method != http.MethodPost)
				requireAttempted(t, body, tc.values)
			})
		}
	}
}

func TestAutosaveKeepsDraftWithoutFinishing(t *testing.T) {
	f := newFixture(t)
	want := students.Details{}
	steps := []struct {
		name, key, raw, value string
		code                  int
	}{
		{"partial name", "name", " Ana ", "Ana", http.StatusOK},
		{"email normalization", "email", " Ada+draft@EXAMPLE.com ", "Ada+draft@example.com", http.StatusOK},
		{"phone normalization", "phone", "(787) 234-5678", "+17872345678", http.StatusOK},
		{"municipality", "municipality", " Bayamón ", "Bayamón", http.StatusOK},
		{"gender", "gender", " Female ", "Female", http.StatusOK},
		{"invalid name", "name", "<script>alert(1)</script>", "Ana", http.StatusUnprocessableEntity},
		{"invalid email", "email", "invalid", "Ada+draft@example.com", http.StatusUnprocessableEntity},
		{"invalid phone", "phone", "123", "+17872345678", http.StatusUnprocessableEntity},
		{"forged municipality", "municipality", "Atlantis", "Bayamón", http.StatusUnprocessableEntity},
		{"forged gender", "gender", "Other", "Female", http.StatusUnprocessableEntity},
		{"clear name", "name", "", "", http.StatusOK},
		{"clear email", "email", "", "", http.StatusOK},
		{"clear phone", "phone", "", "", http.StatusOK},
		{"clear municipality", "municipality", "", "", http.StatusOK},
		{"clear gender", "gender", "", "", http.StatusOK},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			// Unrelated parameters must not be parsed or written by this draft.
			form := url.Values{"name": {"Bad <name>"}, "email": {"invalid"}, "phone": {"invalid"},
				"municipality": {"invalid"}, "gender": {"invalid"}, "pin": {"invalid"}, "email_confirm": {"never persisted"}}
			form.Set(step.key, step.raw)
			w := customerRequest(t, f.mux, http.MethodPost, f.path+"/draft/"+step.key, form.Encode(), true, f.sessionCookie())
			if w.Code != step.code || strings.Contains(w.Body.String(), "<input") {
				t.Fatalf("draft status = %d, want %d, fragment %s", w.Code, step.code, w.Body.String())
			}
			if step.code == http.StatusUnprocessableEntity && !strings.Contains(w.Body.String(), `data-validation="invalid"`) {
				t.Fatalf("draft lacks invalid validation marker: %s", w.Body.String())
			}
			switch step.key {
			case "name":
				want.Name = step.value
			case "email":
				want.Email = step.value
			case "phone":
				want.Phone = step.value
			case "municipality":
				want.Municipality = step.value
			case "gender":
				want.Gender = step.value
			}
			link := stored(t, f.db)[0]
			if link.Details != want || link.SubmittedAt != nil || len(studentRecords(t, f.db)) != 0 {
				t.Fatalf("draft changed unrelated data: link %+v, want details %+v", link, want)
			}
			reload := customerRequest(t, f.mux, http.MethodGet, f.path+"/forms", "", false, f.sessionCookie())
			requireAttempted(t, reload.Body.String(), detailsForm(want))
			groups, err := f.store.ListGroups(t.Context())
			if err != nil || len(groups) != 1 || len(groups[0].Enrollments) != 1 {
				t.Fatalf("list: %+v, %v", groups, err)
			}
			if groups[0].Enrollments[0].Status != links.InProgress {
				t.Fatalf("status = %q, want %q", groups[0].Enrollments[0].Status, links.InProgress)
			}
		})
	}

	final := contactForm("Ana Pérez")
	w := customerRequest(t, f.mux, http.MethodPost, f.path, final.Encode(), true, f.sessionCookie())
	if w.Code != http.StatusOK {
		t.Fatalf("submit: status = %d, body %q", w.Code, w.Body.String())
	}
	requireAdvance(t, w, f.path+"/agreement", true, privateFormValues(final)...)
	if enrolled := studentRecords(t, f.db); len(enrolled) != 0 {
		t.Fatalf("submit students = %+v, want none", enrolled)
	}
	before := stored(t, f.db)
	for _, suffix := range []string{"/draft/name", "/draft/email", "/draft/phone", "/draft/municipality", "/draft/gender"} {
		key := strings.TrimPrefix(suffix, "/draft/")
		w := customerRequest(t, f.mux, http.MethodPost, f.path+suffix, url.Values{key: {""}}.Encode(), true, f.sessionCookie())
		if w.Code != http.StatusConflict || w.Header().Get("HX-Refresh") != "true" {
			t.Fatalf("draft after submit: status = %d, HX-Refresh = %q", w.Code, w.Header().Get("HX-Refresh"))
		}
		requireNoPrivate(t, w.Body.String(), privateFormValues(final)...)
	}
	if after := stored(t, f.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("late drafts changed completed details: before %+v, after %+v", before, after)
	}
}

func TestUnusableLinksTakePrecedence(t *testing.T) {
	db := customerDatabase(t)
	store := links.New(db, nil)
	expired, err := store.CreateLink(t.Context(), "staff", "", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelLink(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, db)
	before := stored(t, db)
	for _, token := range []string{expired.Token, canceled.Token, strings.Repeat("A", len(expired.Token)), "not-a-token"} {
		path := "/customer/" + token
		for _, tc := range []struct{ name, method, suffix, body string }{
			{"entry", http.MethodGet, "", ""},
			{"start", http.MethodPost, "/start", ""},
			{"forms", http.MethodGet, "/forms", ""},
			{"valid final", http.MethodPost, "", contactForm("Ana Pérez").Encode()},
			{"valid draft", http.MethodPost, "/draft/name", "name=Ana"},
			{"malformed final", http.MethodPost, "", "name=%zz"},
			{"malformed draft", http.MethodPost, "/draft/name", "name=%zz"},
			{"oversized final", http.MethodPost, "", "name=" + strings.Repeat("a", 4100)},
			{"oversized draft", http.MethodPost, "/draft/name", "name=" + strings.Repeat("a", 4100)},
			{"invalid final", http.MethodPost, "", "name=+"},
			{"invalid draft", http.MethodPost, "/draft/name", "name=A%00"},
			{"missing draft parameter", http.MethodPost, "/draft/email", ""},
			{"duplicate draft parameter", http.MethodPost, "/draft/email", "email=&email="},
		} {
			for _, htmx := range []bool{false, true} {
				t.Run(token+"/"+tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
					w := customerRequest(t, mux, tc.method, path+tc.suffix, tc.body, htmx)
					if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") {
						t.Fatalf("status = %d, want branded 404: %s", w.Code, w.Body.String())
					}
					if (w.Header().Get("HX-Refresh") == "true") != htmx {
						t.Fatalf("HX-Refresh = %q, htmx = %v", w.Header().Get("HX-Refresh"), htmx)
					}
				})
			}
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) || len(studentRecords(t, db)) != 0 {
		t.Fatalf("unusable requests changed stored state: before %+v, after %+v", before, after)
	}
}

func TestCompletedAccessAndReplay(t *testing.T) {
	f := newFixture(t)
	const submittedName = "Submission snapshot"
	const canonicalName = "Edited student name"
	// Submit directly so replay must not even fill in the absent first-visit time.
	if saved, err := f.store.Submit(t.Context(), f.entry.Token, f.cookie, contactDetails(submittedName), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	if enrolled := studentRecords(t, f.db); len(enrolled) != 0 {
		t.Fatalf("students = %+v, want none", enrolled)
	}
	before, studentsBefore := stored(t, f.db), studentRecords(t, f.db)
	replay := contactForm("Replacement private")
	replay.Set("email", "Replay+private@example.com")
	replay.Set("email_confirm", " Replay+private@EXAMPLE.COM ")
	mismatched := contactForm("Private mismatch")
	mismatched.Set("email_confirm", "Mismatch+private@example.com")
	private := append(privateFormValues(replay), privateFormValues(contactForm(submittedName))...)
	private = append(private, canonicalName)
	private = append(private, privateFormValues(mismatched)...)
	for _, mode := range []string{"ordinary", "htmx"} {
		for _, tc := range []struct {
			name, suffix, body string
			status             int
		}{
			{"valid final replay", "", replay.Encode(), http.StatusOK},
			{"mismatched final replay", "", mismatched.Encode(), http.StatusOK},
			{"invalid final replay", "", "name=%3Cscript%3E&email=invalid&email_confirm=other&phone=123&municipality=Atlantis", http.StatusOK},
			{"malformed final replay", "", "name=%zz", http.StatusOK},
			{"oversized final replay", "", "name=" + strings.Repeat("x", 5000), http.StatusOK},
			{"valid stale draft", "/draft/name", "name=Replacement", http.StatusConflict},
			{"invalid stale draft", "/draft/phone", "phone=123", http.StatusConflict},
			{"missing stale draft", "/draft/email", "", http.StatusConflict},
			{"duplicate stale draft", "/draft/email", "email=&email=", http.StatusConflict},
			{"malformed stale draft", "/draft/name", "name=%zz", http.StatusConflict},
			{"oversized stale draft", "/draft/name", "name=" + strings.Repeat("x", 5000), http.StatusConflict},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				w := customerRequest(t, f.mux, http.MethodPost, f.path+tc.suffix, tc.body, mode == "htmx", f.sessionCookie())
				wantStatus := tc.status
				if tc.status == http.StatusOK && mode == "ordinary" {
					wantStatus = http.StatusSeeOther
				}
				if w.Code != wantStatus {
					t.Fatalf("status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
				}
				refresh := ""
				if mode == "htmx" && tc.status == http.StatusConflict {
					refresh = "true"
				}
				if got := w.Header().Get("HX-Refresh"); got != refresh {
					t.Fatalf("HX-Refresh = %q, want %q", got, refresh)
				}
				if tc.status == http.StatusOK {
					requireAdvance(t, w, f.path+"/agreement", mode == "htmx", private...)
				} else {
					requireNoPrivate(t, w.Body.String(), private...)
				}
				if after := stored(t, f.db); !reflect.DeepEqual(after, before) {
					t.Fatalf("completed POST changed link from %+v to %+v", before, after)
				}
				if after := studentRecords(t, f.db); !reflect.DeepEqual(after, studentsBefore) {
					t.Fatalf("completed POST changed students from %+v to %+v", studentsBefore, after)
				}
			})
		}
	}
	for _, target := range []string{f.path, f.path + "/forms"} {
		for _, htmx := range []bool{false, true} {
			w := customerRequest(t, f.mux, http.MethodGet, target, "", htmx, f.sessionCookie())
			requireAdvance(t, w, f.path+"/agreement", false, private...)
		}
	}
	server := serve(t, f.db)
	headKeeps(t, f.db, server, f.path, http.StatusOK, f.cookie)

	if err := f.db.WithContext(t.Context()).Model(&links.Enrollment{}).Where("id = ?", before[0].ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	// While the parent link is still active, GET /forms redirects to welcome; POST rejects the expired enrollment.
	w := customerRequest(t, f.mux, http.MethodGet, f.path+"/forms", "", false, f.sessionCookie())
	requireAdvance(t, w, f.path, false)
	// Once the parent link also expires, all routes return 404.
	if err := f.db.WithContext(t.Context()).Model(&links.Link{}).Where("id = ?", f.entry.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodGet, "/forms"},
		{http.MethodPost, ""},
		{http.MethodPost, "/draft/name"},
	} {
		w := customerRequest(t, f.mux, tc.method, f.path+tc.suffix, "name=Replacement", false, f.sessionCookie())
		if w.Code != http.StatusNotFound {
			t.Fatalf("expired completion %s %s: status = %d", tc.method, tc.suffix, w.Code)
		}
	}
	headKeeps(t, f.db, server, f.path, http.StatusNotFound, f.cookie)
	if after := studentRecords(t, f.db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatalf("completion access or expiry changed retained students from %+v to %+v", studentsBefore, after)
	}
}

// TestHeadChecksLinkWithoutChangingIt follows a reusable link through welcome, start, draft, and submit.
// HEAD only reports status without writing; GET welcome never creates an enrollment; POST /start starts the session.
func TestHeadChecksLinkWithoutChangingIt(t *testing.T) {
	db := customerDatabase(t)
	store := links.New(db, nil)
	entry, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	path := "/customer/" + entry.Token
	server := serve(t, db)
	client := &http.Client{
		Timeout:       httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	send := func(t *testing.T, method, target, cookieVal string) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, strings.NewReader(contactForm("Changed student").Encode()))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookieVal != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieVal})
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	headKeeps(t, db, server, path, http.StatusOK, "")
	headKeeps(t, db, server, path+"/forms", http.StatusSeeOther, "")
	if len(stored(t, db)) != 0 {
		t.Fatal("HEAD created an enrollment")
	}

	resp := send(t, http.MethodGet, path, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(stored(t, db)) != 0 {
		t.Fatalf("GET welcome status=%d, enrollments=%d; want 200, 0", resp.StatusCode, len(stored(t, db)))
	}
	headKeeps(t, db, server, path, http.StatusOK, "")

	resp = send(t, http.MethodPost, path+"/start", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != path+"/forms" {
		t.Fatalf("POST /start status=%d, location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookieVal string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			cookieVal = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != path {
				t.Fatalf("unexpected session cookie attributes: %+v", c)
			}
		}
	}
	if cookieVal == "" || len(stored(t, db)) != 1 {
		t.Fatalf("POST /start did not issue cookie or create enrollment: cookie=%q, count=%d", cookieVal, len(stored(t, db)))
	}

	headKeeps(t, db, server, path, http.StatusOK, cookieVal)
	headKeeps(t, db, server, path+"/forms", http.StatusOK, cookieVal)

	saveDetails(t, store, entry.Token, cookieVal, contactDetails("Draft student"))
	headKeeps(t, db, server, path, http.StatusOK, cookieVal)

	// Autosave and the page answer no other methods, so none can edit the draft.
	before := stored(t, db)
	for _, tc := range []struct{ name, method, target string }{
		{"HEAD autosave", http.MethodHead, path + "/draft/name"},
		{"GET autosave", http.MethodGet, path + "/draft/name"},
		{"PUT page", http.MethodPut, path},
		{"DELETE page", http.MethodDelete, path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := send(t, tc.method, tc.target, cookieVal)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", resp.StatusCode)
			}
		})
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("unsupported methods changed stored enrollments from %+v to %+v", before, after)
	}

	// A submitted enrollment still opens read-only: HEAD answers 200, not the 409 that locks edits.
	if saved, err := store.Submit(t.Context(), entry.Token, cookieVal, contactDetails("Ana Pérez"), ""); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	headKeeps(t, db, server, path, http.StatusOK, cookieVal)

	// A failed lookup is a server error, never a guess about the link.
	headKeeps(t, db, serve(t, failingDB(t, db)), path, http.StatusInternalServerError, cookieVal)
}

func TestReusableLinkWelcomeStartAndMultiCustomerIsolation(t *testing.T) {
	db := customerDatabase(t)
	store := links.New(db, nil)
	entry, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(t, db)
	path := "/customer/" + entry.Token

	// Welcome page has Comenzar form targeting /start and creates 0 enrollments.
	w := customerRequest(t, mux, http.MethodGet, path, "", false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `action="`+path+`/start"`) || !strings.Contains(w.Body.String(), "Comenzar") {
		t.Fatalf("welcome page: status=%d, body=%s", w.Code, w.Body.String())
	}
	if len(stored(t, db)) != 0 {
		t.Fatal("welcome GET created enrollment")
	}

	// Visitor 1 starts session.
	w1 := customerRequest(t, mux, http.MethodPost, path+"/start", "", false)
	requireAdvance(t, w1, path+"/forms", false)
	c1 := w1.Result().Cookies()[0]

	// Re-POST /start with existing live cookie reuses enrollment 1 and does not create a second row.
	w1b := customerRequest(t, mux, http.MethodPost, path+"/start", "", false, c1)
	requireAdvance(t, w1b, path+"/forms", false)
	if len(stored(t, db)) != 1 {
		t.Fatalf("reused start created extra row: %d", len(stored(t, db)))
	}

	// Welcome with live InProgress cookie shows Continuar link to /forms.
	w1Welcome := customerRequest(t, mux, http.MethodGet, path, "", false, c1)
	if w1Welcome.Code != http.StatusOK || !strings.Contains(w1Welcome.Body.String(), "Continuar") || !strings.Contains(w1Welcome.Body.String(), path+"/forms") {
		t.Fatalf("welcome with live session: %d, %s", w1Welcome.Code, w1Welcome.Body.String())
	}

	// Visitor 2 (no cookie) starts an independent session on the same link.
	w2 := customerRequest(t, mux, http.MethodPost, path+"/start", "", false)
	requireAdvance(t, w2, path+"/forms", false)
	c2 := w2.Result().Cookies()[0]
	if c2.Value == c1.Value || len(stored(t, db)) != 2 {
		t.Fatalf("visitor 2 did not get isolated enrollment: c1=%q c2=%q count=%d", c1.Value, c2.Value, len(stored(t, db)))
	}
	saveDetails(t, store, entry.Token, c1.Value, contactDetails("Visitor One"))
	saveDetails(t, store, entry.Token, c2.Value, contactDetails("Visitor Two"))
	r1 := customerRequest(t, mux, http.MethodGet, path+"/forms", "", false, c1)
	r2 := customerRequest(t, mux, http.MethodGet, path+"/forms", "", false, c2)
	if !strings.Contains(r1.Body.String(), "Visitor One") || strings.Contains(r1.Body.String(), "Visitor Two") ||
		!strings.Contains(r2.Body.String(), "Visitor Two") || strings.Contains(r2.Body.String(), "Visitor One") {
		t.Fatal("visitor sessions leaked across browsers")
	}

	// Visitor 1 finishes enrollment -> welcome shows completed + Comenzar nueva matrícula, and POST /start starts a fresh one.
	if res, err := store.Submit(t.Context(), entry.Token, c1.Value, contactDetails("Visitor One"), ""); err != nil || res != links.Saved {
		t.Fatalf("submit 1: %v, %v", res, err)
	}
	if err := settings.New(db).SetProgramStartDate(t.Context(), "2026-11-10", "staff"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.AcceptTerms(t.Context(), entry.Token, c1.Value, "2026-11-10"); err != nil || !accepted {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, stored(t, db)[0].ID, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	// The 168-hour deadline is for paying: once paid, it must not lock the customer out of the plan page.
	if err := db.WithContext(t.Context()).Model(&links.Enrollment{}).Where("paid_at IS NOT NULL").
		Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	wDone := customerRequest(t, mux, http.MethodGet, path, "", false, c1)
	if wDone.Code != http.StatusOK || !strings.Contains(wDone.Body.String(), "Tu matrícula ya fue completada.") ||
		!strings.Contains(wDone.Body.String(), `href="`+path+`/payments"`) ||
		!strings.Contains(wDone.Body.String(), "Comenzar nueva matrícula") {
		t.Fatalf("finished welcome missing plan link or start-new option: %d, %s", wDone.Code, wDone.Body.String())
	}
	if wPlan := customerRequest(t, mux, http.MethodGet, path+"/payments", "", false, c1); wPlan.Code != http.StatusOK {
		t.Fatalf("paid plan page past the deadline: %d, %s", wPlan.Code, wPlan.Body.String())
	}
	wNew := customerRequest(t, mux, http.MethodPost, path+"/start", "", false, c1)
	requireAdvance(t, wNew, path+"/forms", false)
	if len(wNew.Result().Cookies()) == 0 || wNew.Result().Cookies()[0].Value == c1.Value || len(stored(t, db)) != 3 {
		t.Fatalf("start new after finish failed: cookies=%+v, count=%d", wNew.Result().Cookies(), len(stored(t, db)))
	}
}

func TestHeadRejectsUnusableLinks(t *testing.T) {
	db := customerDatabase(t)
	store := links.New(db, nil)
	canceled, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelLink(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	expired, err := store.CreateLink(t.Context(), "staff", "", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, db)
	for _, tc := range []struct{ name, token string }{
		{"unknown", strings.Repeat("A", len(canceled.Token))},
		{"malformed", "not-a-token"},
		{"canceled", canceled.Token},
		{"expired", expired.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headKeeps(t, db, server, "/customer/"+tc.token, http.StatusNotFound, "")
			headKeeps(t, db, server, "/customer/"+tc.token+"/forms", http.StatusNotFound, "")
		})
	}
}

func TestDraftCardinalityAllowlistAndFailures(t *testing.T) {
	f := newFixture(t)
	saveDetails(t, f.store, f.entry.Token, f.cookie, contactDetails("Saved student"))
	before := stored(t, f.db)
	failing := newMux(t, failingDB(t, f.db))
	for _, field := range students.Fields() {
		for _, tc := range []struct{ name, suffix, body string }{
			{"missing", "", "unrelated=ignored"},
			{"query cannot supply missing POST value", "?" + field.Key + "=ignored", ""},
			{"duplicate", "", url.Values{field.Key: {"", ""}}.Encode()},
		} {
			for _, htmx := range []bool{false, true} {
				t.Run(field.Key+"/"+tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
					w := customerRequest(t, f.mux, http.MethodPost, f.path+"/draft/"+field.Key+tc.suffix, tc.body, htmx, f.sessionCookie())
					if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "data-validation=") ||
						strings.Contains(w.Body.String(), "<input") {
						t.Fatalf("malformed draft status %d, fragment %s", w.Code, w.Body.String())
					}
				})
			}
		}
	}
	// Unknown keys are rejected before lookup, even when the database is down.
	for _, key := range []string{"email_confirm", "Name", "unknown"} {
		w := customerRequest(t, failing, http.MethodPost, f.path+"/draft/"+key, "name=Ignored", true, f.sessionCookie())
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "customer-not-found") ||
			w.Header().Get("HX-Refresh") != "true" {
			t.Fatalf("unknown key %q: status %d, body %s", key, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		name, body string
		failing    bool
		status     int
	}{
		{"malformed", "name=%zz", false, http.StatusBadRequest},
		{"oversized", "name=" + strings.Repeat("a", 4100), false, http.StatusRequestEntityTooLarge},
		{"save failure", "name=Valid+student", true, http.StatusInternalServerError},
		{"validation lookup failure", "name=%3Cscript%3E", true, http.StatusInternalServerError},
		{"parse lookup failure", "name=%zz", true, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := f.mux
			if tc.failing {
				handler = failing
			}
			w := customerRequest(t, handler, http.MethodPost, f.path+"/draft/name", tc.body, true, f.sessionCookie())
			if w.Code != tc.status || strings.Contains(w.Body.String(), "data-validation=") {
				t.Fatalf("draft error status %d, want %d; body %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if after := stored(t, f.db); !reflect.DeepEqual(after, before) || len(studentRecords(t, f.db)) != 0 {
		t.Fatalf("rejected drafts mutated state: before %+v, after %+v", before, after)
	}
}

func TestFinalValidationAndEmailConfirmation(t *testing.T) {
	f := newFixture(t)
	saveDetails(t, f.store, f.entry.Token, f.cookie, contactDetails("Saved student"))
	before := stored(t, f.db)
	cases := []struct {
		name     string
		replace  url.Values
		errors   []string
		mismatch bool
	}{
		{"missing confirmation", url.Values{"email_confirm": {""}}, []string{"email_confirm"}, false},
		{"malformed confirmation", url.Values{"email_confirm": {"Display <private@example.com>"}}, []string{"email_confirm"}, false},
		{"mismatch", url.Values{"email_confirm": {"other@example.com"}}, []string{"email_confirm"}, true},
		{"local part remains case significant", url.Values{"email_confirm": {"ada+private@example.COM"}}, []string{"email_confirm"}, true},
		{"invalid email is not a mismatch", url.Values{"email": {"bad"}, "email_confirm": {"other@example.com"}}, []string{"email"}, false},
		{"collect every error", url.Values{"name": {"Single"}, "email": {"bad"}, "email_confirm": {"bad"},
			"phone": {"123"}, "municipality": {"Atlantis"}}, []string{"name", "email", "email_confirm", "phone", "municipality"}, false},
		{"rejected name is escaped", url.Values{"name": {`<script>alert("private")</script>`}}, []string{"name"}, false},
		{"NUL name", url.Values{"name": {"A\x00"}}, []string{"name"}, false},
		{"invalid UTF-8 name", url.Values{"name": {"\xff"}}, []string{"name"}, false},
		{"future date of birth", url.Values{"date_of_birth": {"2099-01-01"}}, []string{"date_of_birth"}, false},
		{"split date of birth impossible day", url.Values{"date_of_birth": {""}, "dob_day": {"31"}, "dob_month": {"02"}, "dob_year": {"1995"}}, []string{"date_of_birth"}, false},
		{"split date of birth missing year", url.Values{"date_of_birth": {""}, "dob_day": {"20"}, "dob_month": {"01"}, "dob_year": {""}}, []string{"date_of_birth"}, false},
	}
	for _, tc := range cases {
		for _, htmx := range []bool{false, true} {
			t.Run(tc.name+fmt.Sprint("/htmx=", htmx), func(t *testing.T) {
				form := contactForm("  María   Pérez  ")
				form.Set("email", " Ada+private@EXAMPLE.com ")
				form.Set("email_confirm", "Ada+private@example.com")
				form.Set("phone", " (202) 555-0123 ")
				form.Set("municipality", "Bayamón")
				for key, values := range tc.replace {
					form[key] = values
				}
				w := customerRequest(t, f.mux, http.MethodPost, f.path, form.Encode(), htmx, f.sessionCookie())
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
				}
				body := w.Body.String()
				requireFormError(t, body, f.path, !htmx)
				requireAttempted(t, body, form)
				for _, key := range tc.errors {
					ids := []string{key}
					if key == "date_of_birth" {
						ids = []string{"dob_day", "dob_month", "dob_year"}
					}
					for _, id := range ids {
						control := regexp.MustCompile(`<(?:input|select)\b[^>]*\bid="` + id + `"[^>]*>`).FindString(body)
						if !strings.Contains(control, `aria-invalid="true"`) {
							t.Errorf("missing invalid control %q (%s): %s", id, key, body)
						}
					}
					target := regexp.MustCompile(`(?s)<[^>]*\bid="save-status-` + key + `"[^>]*>(.*?)</(?:p|div)>`).FindString(body)
					if !strings.Contains(target, `role="alert"`) {
						t.Errorf("missing associated error for %q: %s", key, body)
					}
				}
				if strings.Contains(body, `data-confirm-error="mismatch"`) != tc.mismatch {
					t.Errorf("mismatch marker disagrees with parse result: %s", body)
				}
				if strings.Contains(body, `<script>alert`) {
					t.Fatal("rejected attempt rendered executable markup")
				}
				if after := stored(t, f.db); !reflect.DeepEqual(after, before) || len(studentRecords(t, f.db)) != 0 {
					t.Fatalf("invalid final mutated persisted state: before %+v, after %+v", before, after)
				}
			})
		}
	}
	// Reload restores saved contact values, but never infers email confirmation.
	reload := customerRequest(t, f.mux, http.MethodGet, f.path+"/forms", "", false, f.sessionCookie())
	requireAttempted(t, reload.Body.String(), detailsForm(contactDetails("Saved student")))
	valid := contactForm("  María   Pérez  ")
	valid.Set("email", " Ada+private@EXAMPLE.com ")
	valid.Set("email_confirm", " Ada+private@example.COM ")
	valid.Set("phone", " (202) 555-0123 ")
	valid.Set("municipality", " Bayamón ")
	w := customerRequest(t, f.mux, http.MethodPost, f.path, valid.Encode(), false, f.sessionCookie())
	requireAdvance(t, w, f.path+"/agreement", false, privateFormValues(valid)...)
	want := students.Details{Name: "María Pérez", Email: "Ada+private@example.com", Phone: "+12025550123", Municipality: "Bayamón", DateOfBirth: "2005-06-15", Gender: "Female"}
	link, enrolled := stored(t, f.db)[0], studentRecords(t, f.db)
	if link.Details != want || len(enrolled) != 0 {
		t.Fatalf("completion did not copy all normalized fields: link %+v, students %+v", link, enrolled)
	}
	// Split day/month/year controls work for birth years before 2001 (with no JS dependency).
	fPre2001 := newFixture(t)
	pre2001 := contactForm("Carlos Rivera")
	pre2001.Del("date_of_birth")
	pre2001.Set("dob_day", "20")
	pre2001.Set("dob_month", "01")
	pre2001.Set("dob_year", "1985")
	w = customerRequest(t, fPre2001.mux, http.MethodPost, fPre2001.path, pre2001.Encode(), false, fPre2001.sessionCookie())
	requireAdvance(t, w, fPre2001.path+"/agreement", false, privateFormValues(pre2001)...)
	if got := stored(t, fPre2001.db)[0].DateOfBirth; got != "1985-01-20" {
		t.Fatalf("split DOB before 2001 = %q, want 1985-01-20", got)
	}
	// Single-digit day is zero-padded when assembled.
	if raw, _, _, _ := readDateOfBirth(url.Values{"dob_day": {"5"}, "dob_month": {"6"}, "dob_year": {"1992"}}); raw != "1992-06-05" {
		t.Fatalf("readDateOfBirth single-digit = %q, want 1992-06-05", raw)
	}
}

func TestPinChecksAreCappedPerLink(t *testing.T) {
	f := newFixture(t)
	if err := f.db.Exec("UPDATE enrollments SET pin_checks = ?", links.MaxPinChecks-1).Error; err != nil {
		t.Fatal(err)
	}
	pin := url.Values{"pin": {"4821"}}.Encode()
	if w := customerRequest(t, f.mux, http.MethodPost, f.path+"/pin", pin, true, f.sessionCookie()); w.Code != http.StatusOK {
		t.Fatalf("last PIN check status = %d, want 200: %s", w.Code, w.Body.String())
	}
	w := customerRequest(t, f.mux, http.MethodPost, f.path+"/pin", pin, true, f.sessionCookie())
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), pinLimitMessage) {
		t.Fatalf("PIN check over budget: status %d, body %s", w.Code, w.Body.String())
	}
	w = customerRequest(t, f.mux, http.MethodPost, f.path, contactForm("Ada Lovelace").Encode(), true, f.sessionCookie())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("final submit over budget status = %d, want 429: %s", w.Code, w.Body.String())
	}
	var submitted int64
	if err := f.db.Raw("SELECT count(*) FROM enrollments WHERE submitted_at IS NOT NULL").Scan(&submitted).Error; err != nil || submitted != 0 {
		t.Fatalf("submitted links = %d, err %v; want 0", submitted, err)
	}
}

func contactDetails(name string) students.Details {
	return students.Details{Name: name, Email: "Ada+private@example.com", Phone: "+17872345678", Municipality: "San Juan", DateOfBirth: "2005-06-15", Gender: "Female"}
}

func detailsForm(details students.Details) url.Values {
	return url.Values{"name": {details.Name}, "email": {details.Email}, "email_confirm": {""},
		"phone": {details.Phone}, "municipality": {details.Municipality}, "date_of_birth": {details.DateOfBirth}, "gender": {details.Gender}}
}

func contactForm(name string) url.Values {
	form := detailsForm(contactDetails(name))
	form.Set("email_confirm", form.Get("email"))
	form.Set("pin", "123456")
	return form
}

func privateFormValues(form url.Values) []string {
	var private []string
	for key, values := range form {
		for _, value := range values {
			if value == "" {
				continue
			}
			private = append(private, value, strings.TrimSpace(value))
			if field, ok := students.FindField(key); ok {
				if normalized, err := field.Parse(value, true); err == nil {
					private = append(private, normalized)
				}
			}
		}
	}
	return private
}

func saveDetails(t *testing.T, store *links.Store, linkToken, cookieToken string, details students.Details) {
	t.Helper()
	for key, values := range detailsForm(details) {
		if key == "email_confirm" {
			continue
		}
		if result, err := store.SaveDraft(t.Context(), linkToken, cookieToken, key, values[0]); err != nil || result != links.Saved {
			t.Fatalf("save %s draft: result %v, error %v", key, result, err)
		}
	}
}

func customerRequest(t *testing.T, handler http.Handler, method, path, body string, htmx bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	for _, c := range cookies {
		if c != nil && c.Value != "" {
			r.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	for name, want := range map[string]string{"Cache-Control": "no-store",
		"Referrer-Policy": "no-referrer", "Vary": "HX-Request", "Content-Type": "text/html; charset=utf-8"} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	return w
}

func requireAttempted(t *testing.T, body string, form url.Values) {
	t.Helper()
	for _, key := range []string{"name", "email", "email_confirm", "phone"} {
		control := regexp.MustCompile(`<input\b[^>]*\bid="` + key + `"[^>]*>`).FindString(body)
		value := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(control)
		want := html.UnescapeString(template.HTMLEscapeString(form.Get(key)))
		if len(value) == 0 || html.UnescapeString(value[1]) != want {
			t.Errorf("%s lost attempted value %q: %s", key, form.Get(key), control)
		}
	}
	selectHTML := regexp.MustCompile(`(?s)<select\b[^>]*\bid="municipality"[^>]*>.*?</select>`).FindString(body)
	want, _ := students.ParseMunicipality(form.Get("municipality"), false)
	selected := regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*\bselected\b[^>]*>`).FindStringSubmatch(selectHTML)
	got := ""
	if len(selected) != 0 {
		got = html.UnescapeString(selected[1])
	}
	if len(selected) == 0 || got != want || selectHTML == "" {
		t.Errorf("municipality selection = %q, want %q: %s", got, want, selectHTML)
	}
	if want == "" && form.Get("municipality") != "" {
		forged := html.UnescapeString(template.HTMLEscapeString(form.Get("municipality")))
		for _, option := range regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*>`).FindAllStringSubmatch(selectHTML, -1) {
			if html.UnescapeString(option[1]) == forged {
				t.Errorf("forged municipality became an option: %s", selectHTML)
			}
		}
	}
	dobDay, dobMonth, dobYear := form.Get("dob_day"), form.Get("dob_month"), form.Get("dob_year")
	if !form.Has("dob_day") && !form.Has("dob_month") && !form.Has("dob_year") {
		if parts := strings.Split(strings.TrimSpace(form.Get("date_of_birth")), "-"); len(parts) == 3 {
			dobYear, dobMonth, dobDay = parts[0], parts[1], parts[2]
		}
	}
	for _, part := range []struct{ id, want string }{{"dob_day", dobDay}, {"dob_year", dobYear}} {
		control := regexp.MustCompile(`<input\b[^>]*\bid="` + part.id + `"[^>]*>`).FindString(body)
		value := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(control)
		wantPart := html.UnescapeString(template.HTMLEscapeString(part.want))
		if len(value) == 0 || html.UnescapeString(value[1]) != wantPart {
			t.Errorf("%s lost attempted value %q: %s", part.id, part.want, control)
		}
	}
	monthSelect := regexp.MustCompile(`(?s)<select\b[^>]*\bid="dob_month"[^>]*>.*?</select>`).FindString(body)
	monthSelected := regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*\bselected\b[^>]*>`).FindStringSubmatch(monthSelect)
	if len(monthSelected) == 0 || monthSelected[1] != dobMonth {
		t.Errorf("dob_month selection = %v, want %q: %s", monthSelected, dobMonth, monthSelect)
	}
}

// httpTimeout bounds each HTTP exchange, so a stuck handler fails its test.
const httpTimeout = 10 * time.Second

func customerDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	models := append([]any{&links.Link{}, &links.Enrollment{}, &students.Student{}, &settings.Settings{}}, billing.Models()...)
	return testdb.Open(t, models...)
}

func testCustomerBiller(t *testing.T, db *gorm.DB) *billing.Service {
	t.Helper()
	return billingtest.New(t, db, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/customers":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_cust_test", "object": "customer"})
		case r.URL.Path == "/v1/checkout/sessions":
			_ = r.ParseForm()
			price := r.FormValue("line_items[0][price]")
			amountTotal := int64(60000)
			switch price {
			case "pr_three_1":
				amountTotal = 20834
			case "pr_six_1":
				amountTotal = 10835
			}
			sessID := "cs_cust_test_" + rand.Text()[:8]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             sessID,
				"object":         "checkout.session",
				"url":            "https://checkout.stripe.com/pay/" + sessID,
				"currency":       "usd",
				"mode":           "payment",
				"customer":       "cus_cust_test",
				"status":         "open",
				"payment_status": "unpaid",
				"amount_total":   amountTotal,
			})
		case strings.HasSuffix(r.URL.Path, "/expire"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             "cs_cust_test",
				"object":         "checkout.session",
				"status":         "expired",
				"payment_status": "unpaid",
			})
		case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             "cs_cust_test",
				"object":         "checkout.session",
				"status":         "expired",
				"payment_status": "unpaid",
			})
		default:
			http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		}
	}))
}

func newMux(t *testing.T, db *gorm.DB) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux, links.New(db, nil), students.New(db, nil), settings.New(db), testCustomerBiller(t, db), false); err != nil {
		t.Fatal(err)
	}
	return logging.Requests(logging.New(io.Discard), mux)
}

// serve runs the customer routes behind a real HTTP server.
func serve(t *testing.T, db *gorm.DB) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newMux(t, db))
	t.Cleanup(server.Close)
	return server
}

// failingDB returns a database handle whose every query fails: its
// transaction is already rolled back.
func failingDB(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	tx := db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	return tx
}

// stored reads every link directly, never through Open, so checks record no visit.
func stored(t *testing.T, db *gorm.DB) []links.Enrollment {
	t.Helper()
	var all []links.Enrollment
	if err := db.WithContext(t.Context()).Order("id").Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	return all
}

func studentRecords(t *testing.T, db *gorm.DB) []students.Student {
	t.Helper()
	all, err := students.New(db, nil).List(t.Context(), students.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func requireFormError(t *testing.T, body, path string, fullPage bool) {
	t.Helper()
	if strings.Count(body, "<form ") != 1 || !strings.Contains(body, `action="`+path+`"`) ||
		!strings.Contains(body, `type="submit"`) {
		t.Fatalf("error response lost the retryable student form: %s", body)
	}
	for _, key := range []string{"name", "email", "email_confirm", "phone", "municipality"} {
		if !strings.Contains(body, `name="`+key+`"`) {
			t.Errorf("error response lost %s control: %s", key, body)
		}
	}
	if strings.Count(body, `<section id="customer-content">`) != 1 ||
		!strings.Contains(body, `hx-target="#customer-content"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Fatalf("error response lost its retryable section target: %s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Errorf("error response lost its inline error: %s", body)
	}
	if got := strings.Contains(body, "<!doctype html>"); got != fullPage {
		t.Errorf("error response full page = %v, want %v", got, fullPage)
	}
}

func requireAdvance(t *testing.T, w *httptest.ResponseRecorder, target string, htmx bool, private ...string) {
	t.Helper()
	want, header := http.StatusSeeOther, "Location"
	if htmx {
		want, header = http.StatusOK, "HX-Redirect"
	}
	if w.Code != want || w.Header().Get(header) != target {
		t.Fatalf("advance status = %d, %s = %q, want %d to %s", w.Code, header, w.Header().Get(header), want, target)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("advance exposed a response body: %s", w.Body.String())
	}
	requireNoPrivate(t, w.Header().Get(header), private...)
}

func requireNoPrivate(t *testing.T, body string, private ...string) {
	t.Helper()
	for _, hidden := range private {
		if hidden != "" && strings.Contains(body, hidden) {
			t.Fatalf("response exposed %q: %s", hidden, body)
		}
	}
}

// headKeeps sends HEAD for path, plain and from HTMX, expecting want each
// time, and fails if any stored link changed.
func headKeeps(t *testing.T, db *gorm.DB, server *httptest.Server, path string, want int, cookieVal string) {
	t.Helper()
	before := stored(t, db)
	studentsBefore := studentRecords(t, db)
	for _, htmx := range []bool{false, true} {
		if got := head(t, server, path, htmx, cookieVal); got != want {
			t.Fatalf("HEAD %s (htmx=%v): status = %d, want %d", path, htmx, got, want)
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("HEAD %s changed stored links from %+v to %+v", path, before, after)
	}
	if after := studentRecords(t, db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatalf("HEAD %s changed students from %+v to %+v", path, studentsBefore, after)
	}
}

// head sends one HEAD request for path, checks the customer response headers,
// and returns the status. http.Client never reads a HEAD response body, so
// head reads the raw connection: the request asks the server to close it after
// replying, and any byte after the headers would be a body.
func head(t *testing.T, server *httptest.Server, path string, htmx bool, cookieVal string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), httpTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(httpTimeout)); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close = true
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	if cookieVal != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieVal})
	}
	if err := r.Write(conn); err != nil {
		t.Fatal(err)
	}
	wire := bufio.NewReader(conn)
	resp, err := http.ReadResponse(wire, r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(wire); err != nil || len(body) != 0 {
		t.Fatalf("HEAD %s (htmx=%v) sent body %q, read error %v", path, htmx, body, err)
	}
	refresh := ""
	if htmx && resp.StatusCode == http.StatusNotFound {
		refresh = "true"
	}
	for name, want := range map[string]string{
		"Cache-Control":   "no-store",
		"Referrer-Policy": "no-referrer",
		"Vary":            "HX-Request",
		"Content-Type":    "text/html; charset=utf-8",
		"HX-Refresh":      refresh,
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("HEAD %s (htmx=%v): %s = %q, want %q", path, htmx, name, got, want)
		}
	}
	return resp.StatusCode
}

func TestContactEditFlowAndBackNavigation(t *testing.T) {
	f := newFixture(t)

	// 1. Unsubmitted link: GET /edit redirects to /forms
	w := customerRequest(t, f.mux, http.MethodGet, f.path+"/edit", "", false, f.sessionCookie())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/forms" {
		t.Fatalf("unsubmitted /edit: status %d, location %q; want redirect to /forms", w.Code, w.Header().Get("Location"))
	}

	// Submit link
	initial := contactDetails("Original Student")
	initial.Email = "original@example.com"
	initial.Phone = "+17875551111"
	initial.Municipality = "Ponce"
	res, err := f.store.Submit(t.Context(), f.entry.Token, f.cookie, initial, "")
	if err != nil || res != links.Saved {
		t.Fatalf("submit: %v, %v", res, err)
	}

	// 2. Submitted link:
	// GET /edit renders prefilled form with back link to agreement and action=/edit
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/edit", "", false, f.sessionCookie())
	if w.Code != http.StatusOK {
		t.Fatalf("GET /edit: status %d, body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="Original Student"`) || !strings.Contains(body, `value="original@example.com"`) ||
		!strings.Contains(body, "17875551111") || !strings.Contains(body, `value="Ponce" selected`) ||
		!strings.Contains(body, `id="dob_day"`) || !strings.Contains(body, `value="15"`) ||
		!strings.Contains(body, `value="06" selected`) || !strings.Contains(body, `value="2005"`) {
		t.Fatalf("GET /edit missing prefilled values: %s", body)
	}
	if !strings.Contains(body, `id="email_confirm"`) || !strings.Contains(body, `name="email_confirm"`) || !strings.Contains(body, `value=""`) {
		t.Fatalf("GET /edit should have blank email_confirm value: %s", body)
	}
	if !strings.Contains(body, `href="`+f.path+`/agreement"`) || !strings.Contains(body, "Volver al acuerdo") {
		t.Fatalf("GET /edit missing back link to agreement: %s", body)
	}
	if !strings.Contains(body, `action="`+f.path+`/edit"`) {
		t.Fatalf("GET /edit missing action=/edit: %s", body)
	}
	if !strings.Contains(body, "Guardar información") {
		t.Fatalf("GET /edit missing 'Guardar información' button: %s", body)
	}
	// Form inputs should not have hx-post autosave
	if strings.Contains(body, `hx-post="`+f.path+`/draft/`) {
		t.Fatalf("GET /edit should not have autosave hx-post: %s", body)
	}

	// Agreement page has "Editar información del estudiante"
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/agreement", "", false, f.sessionCookie())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), f.path+"/edit") || !strings.Contains(w.Body.String(), "Editar información del estudiante") {
		t.Fatalf("unfunded agreement page missing edit back link: %s", w.Body.String())
	}

	// POST /edit with validation error returns 422
	invalidForm := url.Values{
		"name":          {"New Name"},
		"email":         {"invalid-email"},
		"email_confirm": {"invalid-email"},
		"phone":         {"+17875551111"},
		"municipality":  {"Ponce"},
		"date_of_birth": {"2005-06-15"},
		"gender":        {"Female"},
	}
	w = customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", invalidForm.Encode(), false, f.sessionCookie())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /edit with invalid email: status %d, want 422", w.Code)
	}

	// POST /edit with valid update
	validForm := url.Values{
		"name":          {"Updated Student"},
		"email":         {"updated@example.com"},
		"email_confirm": {"updated@example.com"},
		"phone":         {"+17875552222"},
		"municipality":  {"San Juan"},
		"date_of_birth": {"2004-05-14"},
		"gender":        {"Female"},
	}
	w = customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", validForm.Encode(), false, f.sessionCookie())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/agreement" {
		t.Fatalf("POST /edit: status %d, location %q; want redirect to /agreement", w.Code, w.Header().Get("Location"))
	}

	// Check DB: enrollment row is updated with new contact details; no students row exists
	link, _, err := f.store.LookupSession(t.Context(), f.entry.Token, f.cookie)
	if err != nil || link == nil {
		t.Fatalf("lookup session: %v, %v", link, err)
	}
	if link.Name != "Updated Student" || link.Email != "updated@example.com" || link.Phone != "+17875552222" || link.Municipality != "San Juan" {
		t.Fatalf("link snapshot should be updated, got: %+v", link)
	}
	var studentCount int64
	if err := f.db.Model(&students.Student{}).Count(&studentCount).Error; err != nil {
		t.Fatalf("count students: %v", err)
	}
	if studentCount != 0 {
		t.Fatalf("expected 0 students, got %d", studentCount)
	}

	// 3. Funded link. A contract exists only after the terms are accepted, which also keeps /payments from redirecting to /agreement.
	if err := f.db.WithContext(t.Context()).Model(&links.Enrollment{}).
		Where("id = ?", link.ID).
		Updates(map[string]any{"terms_accepted_at": time.Now().UTC(), "program_start_date": "2026-11-01"}).Error; err != nil {
		t.Fatal(err)
	}
	fundedAt := time.Now().UTC()
	contract := billing.Contract{
		EnrollmentID:     link.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		TotalCents:       60000,
		FundedAt:         &fundedAt,
	}
	if err := f.db.Create(&contract).Error; err != nil {
		t.Fatalf("create funded contract: %v", err)
	}

	// GET /edit redirects to /payments
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/edit", "", false, f.sessionCookie())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/payments" {
		t.Fatalf("funded GET /edit: status %d, location %q; want redirect to /payments", w.Code, w.Header().Get("Location"))
	}

	// POST /edit redirects to /payments
	w = customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", validForm.Encode(), false, f.sessionCookie())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/payments" {
		t.Fatalf("funded POST /edit: status %d, location %q; want redirect to /payments", w.Code, w.Header().Get("Location"))
	}

	// Agreement page does NOT have edit back link when funded
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/agreement", "", false, f.sessionCookie())
	if strings.Contains(w.Body.String(), "Editar información del estudiante") {
		t.Fatalf("funded agreement page should not show edit link: %s", w.Body.String())
	}

	// Payments page has top back link to agreement and no duplicate bottom link
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/payments", "", false, f.sessionCookie())
	body = w.Body.String()
	if !strings.Contains(body, `href="`+f.path+`/agreement"`) || !strings.Contains(body, "Volver al acuerdo") {
		t.Fatalf("payments page missing top back link: %s", body)
	}
	if strings.Contains(body, "Revisar el Acuerdo de Pago") {
		t.Fatalf("payments page still has duplicate review link: %s", body)
	}
}

func TestContactEditGuard(t *testing.T) {
	f := newFixture(t)

	// 1. Submit link
	initial := contactDetails("Staff Fixed")
	initial.Phone = "+17875551111"
	initial.Email = "staffixed@example.com"
	initial.Municipality = "San Juan"
	initial.DateOfBirth = "2005-06-15"
	res, err := f.store.Submit(t.Context(), f.entry.Token, f.cookie, initial, "")
	if err != nil || res != links.Saved {
		t.Fatalf("submit: %v, %v", res, err)
	}
	// Customer submits POST /edit changing only date_of_birth
	editForm := url.Values{
		"name":          {"Staff Fixed"},
		"email":         {"staffixed@example.com"},
		"email_confirm": {"staffixed@example.com"},
		"phone":         {"+17875551111"},
		"municipality":  {"San Juan"},
		"dob_day":       {"1"},
		"dob_month":     {"01"},
		"dob_year":      {"1998"},
		"gender":        {"Female"},
	}
	w := customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", editForm.Encode(), false, f.sessionCookie())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/agreement" {
		t.Fatalf("POST /edit: %d, loc %s", w.Code, w.Header().Get("Location"))
	}

	link, _, err := f.store.LookupSession(t.Context(), f.entry.Token, f.cookie)
	if err != nil || link == nil {
		t.Fatal(err)
	}
	if link.DateOfBirth != "1998-01-01" {
		t.Fatalf("link dob not updated: %s, want 1998-01-01", link.DateOfBirth)
	}

	// 2. In-flight and active attempt gate checks. A contract exists only after the terms are accepted.
	if err := f.db.WithContext(t.Context()).Model(&links.Enrollment{}).
		Where("id = ?", link.ID).
		Updates(map[string]any{"terms_accepted_at": time.Now().UTC(), "program_start_date": "2026-11-01"}).Error; err != nil {
		t.Fatal(err)
	}
	contract := billing.Contract{
		EnrollmentID:     link.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		PlanName:         "Pago Único",
		InstallmentCount: 1,
		TotalCents:       60000,
	}
	if err := f.db.Create(&contract).Error; err != nil {
		t.Fatal(err)
	}
	attempt := billing.CheckoutAttempt{
		ContractID:       contract.ID,
		Reference:        rand.Text(),
		PlanID:           "full",
		FirstAmountCents: 60000,
		State:            "open",
	}
	if err := f.db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}

	// State = "open": edits allowed, agreement has edit link
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/agreement", "", false, f.sessionCookie())
	if !strings.Contains(w.Body.String(), "Editar información del estudiante") {
		t.Fatalf("open state should allow edit link: %s", w.Body.String())
	}

	// State in inFlightAttemptStates ("processing", "uncertain", "expiring"): edits blocked
	for _, blockedState := range []string{"processing", "uncertain", "expiring"} {
		if err := f.db.Model(&billing.CheckoutAttempt{}).Where("id = ?", attempt.ID).Update("state", blockedState).Error; err != nil {
			t.Fatal(err)
		}
		// Agreement page has NO edit link
		w = customerRequest(t, f.mux, http.MethodGet, f.path+"/agreement", "", false, f.sessionCookie())
		if strings.Contains(w.Body.String(), "Editar información del estudiante") {
			t.Fatalf("state %s should hide edit link: %s", blockedState, w.Body.String())
		}
		// GET /edit redirects to /payments
		w = customerRequest(t, f.mux, http.MethodGet, f.path+"/edit", "", false, f.sessionCookie())
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/payments" {
			t.Fatalf("GET /edit in %s: %d, loc %s; want redirect to /payments", blockedState, w.Code, w.Header().Get("Location"))
		}
		// POST /edit redirects to /payments and does NOT modify the link's contact snapshot
		attemptForm := url.Values{
			"name":          {"Hacked Name"},
			"email":         {"hacked@example.com"},
			"email_confirm": {"hacked@example.com"},
			"phone":         {"+17875559999"},
			"municipality":  {"San Juan"},
			"date_of_birth": {"2000-01-01"},
			"gender":        {"Female"},
		}
		w = customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", attemptForm.Encode(), false, f.sessionCookie())
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != f.path+"/payments" {
			t.Fatalf("POST /edit in %s: %d, loc %s; want redirect to /payments", blockedState, w.Code, w.Header().Get("Location"))
		}
		var checkLink links.Enrollment
		f.db.Where("id = ?", link.ID).Take(&checkLink)
		if checkLink.Name != "Staff Fixed" || checkLink.DateOfBirth != "1998-01-01" {
			t.Fatalf("POST /edit during %s mutated link: %+v", blockedState, checkLink)
		}
	}

	// State = "creating": edits allowed again
	if err := f.db.Model(&billing.CheckoutAttempt{}).Where("id = ?", attempt.ID).Update("state", "creating").Error; err != nil {
		t.Fatal(err)
	}
	w = customerRequest(t, f.mux, http.MethodGet, f.path+"/agreement", "", false, f.sessionCookie())
	if !strings.Contains(w.Body.String(), "Editar información del estudiante") {
		t.Fatalf("creating state should allow edit link: %s", w.Body.String())
	}

	// 3. Concurrency proof: edit during locked Link transaction waits and succeeds
	lockHeld := make(chan struct{})
	lockRelease := make(chan struct{})
	txDone := make(chan struct{})
	go func() {
		defer close(txDone)
		_ = f.db.Transaction(func(tx *gorm.DB) error {
			var lockedLink links.Enrollment
			if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
				Where("id = ?", link.ID).Take(&lockedLink).Error; err != nil {
				return err
			}
			close(lockHeld)
			<-lockRelease
			return nil
		})
	}()
	<-lockHeld
	// Concurrently attempt POST /edit while link row lock is held
	editDone := make(chan int)
	go func() {
		concurrentForm := url.Values{
			"name":          {"Staff Fixed"},
			"email":         {"staffixed@example.com"},
			"email_confirm": {"staffixed@example.com"},
			"phone":         {"+17875559999"},
			"municipality":  {"San Juan"},
			"date_of_birth": {"2002-02-02"},
			"gender":        {"Female"},
		}
		cw := customerRequest(t, f.mux, http.MethodPost, f.path+"/edit", concurrentForm.Encode(), false, f.sessionCookie())
		editDone <- cw.Code
	}()

	// Verify edit has not finished yet (it is blocked on the link lock)
	select {
	case code := <-editDone:
		t.Fatalf("POST /edit completed prematurely while lock was held, code %d", code)
	case <-time.After(50 * time.Millisecond):
		// Expected: still waiting
	}

	close(lockRelease)
	<-txDone

	code := <-editDone
	if code != http.StatusSeeOther {
		t.Fatalf("concurrent POST /edit after unlock: %d; want 303", code)
	}
	f.db.Where("id = ?", link.ID).Take(&link)
	if link.DateOfBirth != "2002-02-02" {
		t.Fatalf("concurrent edit did not persist: %s", link.DateOfBirth)
	}
}
