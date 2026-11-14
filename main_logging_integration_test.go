//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/auth"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

const (
	httpRequestID = "logging-http-contract-001"
	httpUsername  = "private-http-staff"
	httpPassword  = "PRIVATE_HTTP_PASSWORD"
	httpQuery     = "PRIVATE_HTTP_QUERY"
	httpHeader    = "PRIVATE_HTTP_HEADER"
	// privateQuery rides on every logged request; no event may carry it.
	privateQuery = "?private=" + httpQuery
)

// loggedHeader fixes the request ID and adds a private header no event may carry.
var loggedHeader = http.Header{"X-Railway-Request-Id": {httpRequestID}, "X-Private-Header": {httpHeader}}

func TestCustomerRolledBackRequestsLogOnce(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry, cookieToken := fixture.createAndStart(t)
	tx := fixture.db.WithContext(t.Context()).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	handler := serverHandler(t, fixture.db, tx, fixture.log)
	sessionCookie := &http.Cookie{Name: "enrollment_session", Value: cookieToken}
	form := httpContactForm("Private rolled back name")
	rec := send(t, handler, http.MethodPost, "/customer/"+entry.Token+"/draft/name"+privateQuery, sessionCookie, form, loggedHeader)
	private := append(httpContactValues(form), entry.Token, cookieToken, httpUsername)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}/draft/{field}",
		stage: "save", kind: "transaction_done",
	}, private...)
	if !strings.Contains(rec.Body.String(), `role="alert"`) || strings.Contains(rec.Body.String(), "data-validation=") {
		t.Fatal("rolled-back write lost its save-error response")
	}
}

func TestStudentSaveAndPreliminaryLoadKeepSafeLeafLogging(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry, cookieToken := fixture.createAndStart(t)
	originalDetails := httpContactDetails("Private original student")
	if saved, err := fixture.store.Submit(t.Context(), entry.Token, cookieToken, originalDetails, ""); err != nil || saved != links.Saved {
		t.Fatalf("enroll student: result = %v, error = %v", saved, err)
	}
	const programStart = "2026-11-10"
	if err := settings.New(fixture.db).SetProgramStartDate(t.Context(), programStart, "staff"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := fixture.store.AcceptTerms(t.Context(), entry.Token, cookieToken, programStart); err != nil || !accepted {
		t.Fatalf("accept agreement: accepted=%v, err=%v", accepted, err)
	}
	source := fixture.enrollment(t, entry.Token, cookieToken)
	now := time.Now().UTC()
	if err := fixture.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, source.ID, now)
	}); err != nil {
		t.Fatalf("enroll student: %v", err)
	}
	var original students.Student
	if err := fixture.db.WithContext(t.Context()).Where("enrollment_id = ?", source.ID).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	handler := serverHandler(t, fixture.db, fixture.db, fixture.log)
	cookie := signInStaff(t, fixture.db, handler)
	fixture.requireQuiet(t)
	var user auth.User
	if err := fixture.db.WithContext(t.Context()).Take(&user, "username = ?", "staff").Error; err != nil {
		t.Fatal(err)
	}
	cookieDigest := sha256.Sum256([]byte(cookie.Value))
	private := []string{user.Username, user.PasswordHash, user.AuthVersion, cookie.Value, hex.EncodeToString(cookieDigest[:]),
		entry.Token, originalDetails.Name, originalDetails.Email, originalDetails.Phone, originalDetails.Municipality,
		"Rejected by database", "reject_test_student_name", "unavailable_students"}
	path := fmt.Sprintf("/management/students/%d/fields/name", original.ID)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students ADD CONSTRAINT reject_test_student_name CHECK (name <> 'Rejected by database')").Error; err != nil {
		t.Fatal(err)
	}
	rec := send(t, handler, http.MethodPost, path+privateQuery, cookie, url.Values{"value": {"Rejected by database"}}, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/students/{id}/fields/{field}",
		stage: "save", kind: "database", sqlstate: "23514",
	}, private...)
	fixture.out.Reset()
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE students RENAME TO unavailable_students").Error; err != nil {
		t.Fatal(err)
	}
	rec = send(t, handler, http.MethodPost, path+privateQuery, cookie, url.Values{"value": {"Rejected by database"}}, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/students/{id}/fields/{field}",
		stage: "load", kind: "database", sqlstate: "42P01",
	}, private...)
}

func TestSessionLoadFailureKeepsPublicHeadQuiet(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	handler := serverHandler(t, fixture.db, fixture.db, fixture.log)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE sessions RENAME TO unavailable_sessions").Error; err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "management_session", Value: "PRIVATE_FORGED_STAFF_SESSION"}
	rec := send(t, handler, http.MethodHead, "/customer/"+unknownToken+privateQuery, cookie, nil, loggedHeader)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Referrer-Policy") != "no-referrer" || strings.Contains(rec.Header().Get("Vary"), "Cookie") || len(rec.Result().Cookies()) != 0 {
		t.Fatal("public HEAD consulted staff sessions or changed its existing quiet 404")
	}
	fixture.requireQuiet(t)
	rec = send(t, handler, http.MethodGet, "/management/"+privateQuery, cookie, nil, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodGet, route: "/management/",
		stage: "session", kind: "database", sqlstate: "42P01",
	}, cookie.Value, unknownToken, "unavailable_sessions")
	if rec.Body.String() != "Internal Server Error\n" {
		t.Fatal("session load failure exposed database detail or changed the SCS 500")
	}
}

func TestSignInCommitFailureLogsActualStatusWithoutCredentials(t *testing.T) {
	// A fresh, empty session store and no incoming cookie make the failure occur
	// in redirect commit, not session load or RenewToken's old-token deletion.
	fixture := newHTTPLoggingFixture(t)
	user := addStaff(t, fixture.db, httpUsername, httpPassword)
	handler := serverHandler(t, fixture.db, fixture.db, fixture.log)
	var count int64
	if err := fixture.db.WithContext(t.Context()).Model(&auth.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("fresh sessions: count = %d, error = %v", count, err)
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE sessions ADD CONSTRAINT reject_session_commit CHECK (false)").Error; err != nil {
		t.Fatal(err)
	}
	rec := send(t, handler, http.MethodPost, "/management/login"+privateQuery, nil, url.Values{"username": {user.Username}, "password": {httpPassword}}, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /management/login",
		stage: "session", kind: "database", sqlstate: "23514",
	}, httpUsername, httpPassword, user.PasswordHash, user.AuthVersion, "reject_session_commit")
	if !strings.Contains(rec.Body.String(), "Internal Server Error") {
		t.Fatal("session commit failure lost the SCS error response")
	}
	if err := fixture.db.WithContext(t.Context()).Model(&auth.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed session commit persisted a session: count = %d, error = %v", count, err)
	}
}

func TestAgreementFailuresLogOnceWithoutContactData(t *testing.T) {
	fixture := newHTTPLoggingFixture(t)
	entry, cookieToken := fixture.createAndStart(t)
	details := httpContactDetails("Private agreement student")
	if result, err := fixture.store.Submit(t.Context(), entry.Token, cookieToken, details, ""); err != nil || result != links.Saved {
		t.Fatalf("submit: %v, %v", result, err)
	}
	const date = "2027-02-28"
	if err := settings.New(fixture.db).SetProgramStartDate(t.Context(), date, httpUsername); err != nil {
		t.Fatal(err)
	}
	handler := serverHandler(t, fixture.db, fixture.db, fixture.log)
	path := "/customer/" + entry.Token
	sessionCookie := &http.Cookie{Name: "enrollment_session", Value: cookieToken}
	private := []string{details.Name, details.Email, details.Phone, details.Municipality, entry.Token, cookieToken,
		httpUsername, "reject_agreement", "unavailable_settings"}
	before := fixture.enrollment(t, entry.Token, cookieToken)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE settings RENAME TO unavailable_settings").Error; err != nil {
		t.Fatal(err)
	}
	rec := send(t, handler, http.MethodGet, path+"/agreement"+privateQuery, sessionCookie, nil, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodGet, route: "GET /customer/{token}/agreement",
		stage: "load", kind: "database", sqlstate: "42P01",
	}, private...)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE unavailable_settings RENAME TO settings").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE enrollments ADD CONSTRAINT reject_agreement CHECK (terms_accepted_at IS NULL)").Error; err != nil {
		t.Fatal(err)
	}
	consent := url.Values{"accept_terms": {"yes"}, "program_start_date": {date}}
	rec = send(t, handler, http.MethodPost, path+"/agreement"+privateQuery, sessionCookie, consent, loggedHeader)
	fixture.requireFailure(t, rec, httpFailure{
		method: http.MethodPost, route: "POST /customer/{token}/agreement",
		stage: "accept", kind: "database", sqlstate: "23514",
	}, private...)
	if after := fixture.enrollment(t, entry.Token, cookieToken); !reflect.DeepEqual(after, before) {
		t.Fatal("failed agreement changed contact or consent")
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE enrollments DROP CONSTRAINT reject_agreement").Error; err != nil {
		t.Fatal(err)
	}
	fixture.out.Reset()
	rec = send(t, handler, http.MethodPost, path+"/agreement"+privateQuery, sessionCookie, consent, loggedHeader)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/payments" || rec.Body.Len() != 0 {
		t.Fatal("agreement retry did not advance quietly")
	}
	fixture.requireQuiet(t)
}

func httpContactDetails(name string) students.Details {
	return students.Details{Name: name, Email: "Private+retained@example.com", Phone: "+17872345678", Municipality: "Río Grande", DateOfBirth: "2005-06-15"}
}

func httpContactForm(name string) url.Values {
	return url.Values{"name": {name}, "email": {" Private+logging@EXAMPLE.COM "},
		"email_confirm": {" Private+logging@example.com "}, "phone": {"(202) 555-0123"}, "municipality": {"Bayamón"}, "date_of_birth": {"2005-06-15"}}
}

func httpContactValues(form url.Values) []string {
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

// httpLoggingFixture captures every event the server and its queries log, over an isolated database.
type httpLoggingFixture struct {
	db    *gorm.DB
	store *links.Store
	log   *slog.Logger
	out   bytes.Buffer
}

func newHTTPLoggingFixture(t *testing.T) *httpLoggingFixture {
	t.Helper()
	fixture := &httpLoggingFixture{}
	fixture.log = logging.New(&fixture.out)
	fixture.db = testdb.Open(t, models()...).Session(&gorm.Session{Logger: logging.NewDBLogger(fixture.log)})
	fixture.store = links.New(fixture.db, nil)
	return fixture
}

func (fixture *httpLoggingFixture) createAndStart(t *testing.T) (links.LinkEntry, string) {
	t.Helper()
	entry, err := fixture.store.CreateLink(t.Context(), httpUsername, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	started, cookieToken, err := fixture.store.StartEnrollment(t.Context(), entry.Token, "")
	if err != nil || started == nil {
		t.Fatalf("start enrollment: %v, %v", started, err)
	}
	return entry, cookieToken
}

func (fixture *httpLoggingFixture) enrollment(t *testing.T, linkToken, cookieToken string) links.Enrollment {
	t.Helper()
	enrollment, _, err := fixture.store.LookupSession(t.Context(), linkToken, cookieToken)
	if err != nil || enrollment == nil {
		t.Fatalf("read source enrollment: present = %t, error = %v", enrollment != nil, err)
	}
	return *enrollment
}

type httpFailure struct {
	method, route, stage, kind, sqlstate string
}

func (fixture *httpLoggingFixture) requireFailure(t *testing.T, rec *httptest.ResponseRecorder, want httpFailure, private ...string) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Request-ID") != httpRequestID {
		t.Fatalf("failure response: status = %d, request ID = %q", rec.Code, rec.Header().Get("X-Request-ID"))
	}
	data := fixture.out.Bytes()
	if !bytes.HasSuffix(data, []byte{'\n'}) || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("request must emit exactly one newline-terminated event")
	}
	for _, value := range append([]string{httpQuery, httpHeader, "management_session", "SELECT ", "UPDATE ", "INSERT INTO ", "DELETE FROM ", "ALTER TABLE "}, private...) {
		if value != "" && (bytes.Contains(data, []byte(value)) || bytes.Contains(data, []byte(url.QueryEscape(value)))) {
			t.Fatalf("request event contains forbidden private value %q", value)
		}
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("request event is not JSON: %v", err)
	}
	for name, expected := range map[string]any{
		"message": "request_failed", "level": "error", "request_id": httpRequestID,
		"method": want.method, "route": want.route,
		"stage": want.stage, "status": float64(http.StatusInternalServerError), "error_kind": want.kind,
	} {
		if event[name] != expected {
			t.Errorf("event %s = %v, want %v", name, event[name], expected)
		}
	}
	if elapsed, ok := event["duration_ms"].(float64); !ok || elapsed < 0 {
		t.Error("request event lacks a nonnegative numeric elapsed duration")
	}
	wantType := "*pgconn.PgError"
	if want.kind == "transaction_done" {
		wantType = "*errors.errorString"
	}
	if event["error_type"] != wantType {
		t.Errorf("error_type = %v, want concrete type %s", event["error_type"], wantType)
	}
	if want.sqlstate != "" {
		if event["sqlstate"] != want.sqlstate {
			t.Errorf("SQLSTATE = %v, want %s", event["sqlstate"], want.sqlstate)
		}
	} else if _, exists := event["sqlstate"]; exists {
		t.Error("non-PostgreSQL failure acquired a SQLSTATE")
	}
	for name := range event {
		switch name {
		case "time", "message", "level", "request_id", "method", "route", "stage", "status", "duration_ms", "error_kind", "error_type", "sqlstate":
		default:
			t.Errorf("request event has unexpected field %q", name)
		}
	}
}

func (fixture *httpLoggingFixture) requireQuiet(t *testing.T) {
	t.Helper()
	if fixture.out.Len() != 0 {
		t.Fatal("successful or expected request emitted an event")
	}
}
