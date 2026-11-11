//go:build integration

package management

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
)

// pinSeq hands each enrolled test student its own PIN: the roster refuses duplicates.
var pinSeq atomic.Int32

func enrollStudent(t *testing.T, a *app, name string) (students.Student, links.Link) {
	t.Helper()
	entry, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	pin := fmt.Sprintf("%06d", 100000+pinSeq.Add(1))
	if saved, err := a.store.Submit(t.Context(), entry.Token, contactDetails(name), pin); err != nil || saved != links.Saved {
		t.Fatalf("enroll student: submitted = %v, error = %v", saved, err)
	}
	acceptAgreement(t, a, entry.Token)
	hash, err := hex.DecodeString(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, hash, now)
	}); err != nil {
		t.Fatalf("enroll student: %v", err)
	}
	var record students.Student
	if err := a.db.WithContext(t.Context()).Where("source_link_hash = ?", hash).Take(&record).Error; err != nil {
		t.Fatal(err)
	}
	var source links.Link
	if err := a.db.WithContext(t.Context()).Where("token_hash = ?", hash).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	return record, source
}

// acceptAgreement accepts token's submitted agreement, as checkout requires before Enroll;
// the start date's month becomes the student's cohort.
func acceptAgreement(t *testing.T, a *app, token string) {
	t.Helper()
	const programStart = "2026-11-10"
	if err := settings.New(a.db).SetProgramStartDate(t.Context(), programStart, "staff"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := a.store.AcceptTerms(t.Context(), token, programStart); err != nil || accepted == nil {
		t.Fatalf("accept agreement: link=%+v, err=%v", accepted, err)
	}
}

func requireStudentState(t *testing.T, a *app, original students.Student, source links.Link, other students.Student, details students.Details) {
	t.Helper()
	want := original
	want.Details = details
	got, err := students.New(a.db, nil).Get(t.Context(), original.ID)
	if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("student = %+v, error = %v; want %+v", got, err, want)
	}
	unrelated, err := students.New(a.db, nil).Get(t.Context(), other.ID)
	if err != nil || unrelated == nil || !reflect.DeepEqual(*unrelated, other) {
		t.Fatalf("unrelated student changed: %+v, error = %v; want %+v", unrelated, err, other)
	}
	var snapshot links.Link
	if err := a.db.WithContext(t.Context()).Where("token_hash = ?", source.TokenHash).Take(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, source) {
		t.Fatalf("source link changed: got %+v, want %+v", snapshot, source)
	}
	var count int64
	if err := a.db.WithContext(t.Context()).Model(&students.Student{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("student count = %d, error = %v; want 2", count, err)
	}
}

func requireStudentRow(t *testing.T, body string, record students.Student) {
	t.Helper()
	_, row, found := strings.Cut(body, fmt.Sprintf(`<tr id="student-row-%d"`, record.ID))
	row, _, closed := strings.Cut(row, "</tr>")
	if !found || !closed || !strings.Contains(row, fmt.Sprintf(`data-student-link href="/management/students/%d"`, record.ID)) {
		t.Fatalf("missing canonical student row %d: %s", record.ID, body)
	}
	previous := -1
	for i, field := range students.StaffFields() {
		cell := "<td>" + escapedHTML(field.Value(record)) + "</td>"
		if i == 0 {
			cell = ">" + escapedHTML(record.Name) + "</a>"
		}
		at := strings.Index(row, cell)
		if at < 0 || at <= previous {
			t.Fatalf("row %d omitted or misplaced %s: %s", record.ID, field.Key, row)
		}
		previous = at
	}
}

func requireStudentDetails(t *testing.T, body string, details students.Details) {
	t.Helper()
	for _, field := range students.Fields() {
		_, section, found := strings.Cut(body, "<h2>"+field.Label+"</h2>")
		section, _, _ = strings.Cut(section, `<div class="student-field"`)
		if !found || !strings.Contains(section, `<p class="student-field-value">`+escapedHTML(valueOf(field, details))+`</p>`) {
			t.Fatalf("details omitted or misplaced canonical %s: %s", field.Key, body)
		}
	}
}

// valueOf reads a contact field from details alone: those fields need no other Student column.
func valueOf(field students.Field, details students.Details) string {
	return field.Value(students.Student{Details: details})
}

func requireStudentEditor(t *testing.T, body string, id uint64, key, value string) {
	t.Helper()
	inputID := fmt.Sprintf("student-input-%d-%s", id, key)
	field, ok := students.FindField(key)
	if !ok || !strings.Contains(body, `<label for="`+inputID+`">`+field.Label+`</label>`) ||
		!strings.Contains(body, `class="student-field-editor"`) {
		t.Fatalf("missing labeled %s editor: %s", key, body)
	}
	if len(field.Options) == 0 {
		if !strings.Contains(body, `<input id="`+inputID+`" name="value" type="`+field.InputType+`" value="`+escapedHTML(value)+`" required`) {
			t.Fatalf("input lost %s value or required state: %s", key, body)
		}
		return
	}
	selected := value
	if !slices.Contains(field.Options, value) {
		selected = ""
	}
	if !strings.Contains(body, `<select id="`+inputID+`" name="value" required`) ||
		strings.Contains(body, `<input id="`+inputID+`"`) ||
		!strings.Contains(body, `<option value=""`) || !strings.Contains(body, ">Select an option</option>") ||
		!strings.Contains(body, `<option value="`+escapedHTML(selected)+`" selected>`) ||
		strings.Count(body, " selected") != 1 || strings.Count(body, "<option ") != len(field.Options)+1 {
		t.Fatalf("native select lost stored/attempted selection: %s", body)
	}
	if selected == "" && value != "" && strings.Contains(body, `<option value="`+escapedHTML(value)+`"`) {
		t.Fatalf("native select forged an option for %q: %s", value, body)
	}
}

type studentEdit struct {
	key, raw string
	want     students.Details
}

func canonicalEdits(initial students.Details) []studentEdit {
	name := initial
	name.Name = "María José Pérez Rivera"
	email := name
	email.Email = "Edited.Student+tag@example.com"
	phone := email
	phone.Phone = "+17872345678"
	municipality := phone
	municipality.Municipality = "Mayagüez"
	return []studentEdit{
		{"name", "  María   José Pérez Rivera  ", name},
		{"email", "  Edited.Student+tag@EXAMPLE.COM  ", email},
		{"phone", " (787) 234-5678 ", phone},
		{"municipality", "  Mayagüez  ", municipality},
	}
}

func TestStudentReadValidateSaveReload(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)

	res, body := a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `<table class="student-table"`) ||
		!strings.Contains(body, `href="/management/students" aria-current="page"`) {
		t.Fatalf("student archive: status = %d: %s", res.StatusCode, body)
	}
	previous := -1
	for _, label := range []string{"Name", "Email", "Phone", "Municipality", "Enrolled"} {
		at := strings.Index(body, `<th scope="col">`+label+`</th>`)
		if at < 0 || at <= previous {
			t.Fatalf("missing or misplaced %s column: %s", label, body)
		}
		previous = at
	}
	newer := strings.Index(body, fmt.Sprintf(`id="student-row-%d"`, other.ID))
	older := strings.Index(body, fmt.Sprintf(`id="student-row-%d"`, original.ID))
	if newer < 0 || older < 0 || newer >= older {
		t.Fatalf("archive must contain both students, newest first: %s", body)
	}
	requireStudentRow(t, body, original)
	requireStudentRow(t, body, other)
	if strings.Contains(body, hex.EncodeToString(original.SourceLinkHash)) || strings.Contains(body, "/customer/") ||
		strings.Contains(body, "/static/management.js") {
		t.Fatalf("archive leaked source access or loaded link-only script: %s", body)
	}

	res, body = a.send(t, staff, http.MethodGet, path, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") ||
		!strings.Contains(body, "Back to students") || strings.Contains(body, `method="dialog"`) {
		t.Fatalf("standalone details: status = %d: %s", res.StatusCode, body)
	}
	requireStudentDetails(t, body, original.Details)
	expected := original.Details
	for _, edit := range canonicalEdits(original.Details) {
		t.Run(edit.key, func(t *testing.T) {
			fieldPath := path + "/fields/" + edit.key
			field, _ := students.FindField(edit.key)
			res, body := a.send(t, staff, http.MethodGet, fieldPath+"?edit=1&value=GET+must+not+save", nil, nil)
			if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") || !strings.Contains(body, ">Cancel</a>") ||
				!strings.Contains(body, `hx-target="closest .student-field" hx-swap="outerHTML"`) {
				t.Fatalf("standalone editor: status = %d: %s", res.StatusCode, body)
			}
			requireStudentEditor(t, body, original.ID, edit.key, valueOf(field, expected))
			requireStudentState(t, a, original, source, other, expected)
			res, body = a.send(t, staff, http.MethodGet, fieldPath, nil, nil)
			if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!doctype html>") ||
				strings.Contains(body, "student-field-editor") || strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("ordinary cancel: status = %d: %s", res.StatusCode, body)
			}
			requireStudentDetails(t, body, expected)
			requireStudentState(t, a, original, source, other, expected)
			res, body = a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {edit.raw}, "modal": {"1"}}, nil)
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != path+"?saved="+edit.key || strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("ordinary save: status = %d, location = %q: %s", res.StatusCode, res.Header.Get("Location"), body)
			}
			expected = edit.want
			requireStudentState(t, a, original, source, other, expected)
			res, body = a.send(t, staff, http.MethodGet, res.Header.Get("Location"), nil, nil)
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `role="status">Saved.`) ||
				strings.Contains(body, `class="student-field-editor"`) || strings.Contains(body, `method="dialog"`) {
				t.Fatalf("persisted detail reload: status = %d: %s", res.StatusCode, body)
			}
			requireStudentDetails(t, body, expected)
			res, body = a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("archive reload: status = %d: %s", res.StatusCode, body)
			}
			canonical := original
			canonical.Details = expected
			requireStudentRow(t, body, canonical)
			requireStudentRow(t, body, other)
		})
	}
}

func TestStudentValidationPreservesAttemptedValues(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	for _, tc := range []struct {
		name, key, value string
	}{
		{"invalid UTF-8", "name", "bad\xffname"},
		{"NUL", "name", "<attempt>\x00"},
		{"blank name", "name", "   "},
		{"one-word name", "name", "María"},
		{"markup name", "name", `<script>alert("student")</script> O'Connor 李`},
		{"malformed email", "email", "not-an-email"},
		{"blank email", "email", ""},
		{"invalid phone", "phone", "123"},
		{"blank phone", "phone", ""},
		{"forged municipality", "municipality", "Forged municipality"},
		{"blank municipality", "municipality", ""},
	} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/htmx=%t", tc.name, partial), func(t *testing.T) {
				var headers http.Header
				if partial {
					headers = htmxHeader
				}
				res, body := a.send(t, staff, http.MethodPost, path+"/fields/"+tc.key, url.Values{"value": {tc.value}, "modal": {"1"}}, headers)
				errorID := fmt.Sprintf("student-error-%d-%s", original.ID, tc.key)
				if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `class="student-field-editor"`) ||
					!strings.Contains(body, `aria-invalid="true"`) || !strings.Contains(body, `aria-describedby="`+errorID+`"`) ||
					!strings.Contains(body, `id="`+errorID+`" role="alert"`) || strings.Contains(body, "Saved.") ||
					strings.Contains(body, "hx-swap-oob") || strings.Contains(body, `<script>alert`) {
					t.Fatalf("invalid field: status = %d: %s", res.StatusCode, body)
				}
				if partial == strings.HasPrefix(body, "<!doctype html>") || strings.Contains(body, `method="dialog"`) ||
					strings.Contains(body, `name="modal" value="1"`) != partial {
					t.Fatalf("validation response has wrong page/field shape: %s", body)
				}
				if tc.name == "NUL" {
					if !strings.Contains(body, "&lt;attempt&gt;") {
						t.Fatalf("editor lost escaped attempted value: %s", body)
					}
				} else if tc.name != "invalid UTF-8" {
					requireStudentEditor(t, body, original.ID, tc.key, tc.value)
				}
				if !partial {
					for _, field := range students.Fields() {
						if field.Key != tc.key && !strings.Contains(body, `<p class="student-field-value">`+escapedHTML(field.Value(original))+`</p>`) {
							t.Fatalf("invalid %s lost canonical %s: %s", tc.key, field.Key, body)
						}
					}
				}
				requireStudentState(t, a, original, source, other, original.Details)
			})
		}
	}
}

func TestStudentHTMXFragmentsModalAndEscaping(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)

	res, body := a.send(t, staff, http.MethodGet, path, nil, htmxHeader)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `class="student-details" hx-sync="closest .student-details:queue all"`) ||
		!strings.Contains(body, `id="student-heading"`) || !strings.Contains(body, `method="dialog"`) ||
		!strings.Contains(body, "?edit=1&amp;modal=1") || strings.Contains(body, "Back to students") || strings.Contains(body, `<dialog`) {
		t.Fatalf("modal content: status = %d: %s", res.StatusCode, body)
	}
	requireStudentDetails(t, body, original.Details)
	expected := original.Details
	for _, edit := range canonicalEdits(original.Details) {
		t.Run(edit.key, func(t *testing.T) {
			fieldPath := path + "/fields/" + edit.key
			fieldID := fmt.Sprintf(`id="student-field-%d-%s"`, original.ID, edit.key)
			field, _ := students.FindField(edit.key)
			for _, modal := range []bool{false, true} {
				query := "?edit=1"
				cancel := fieldPath
				if modal {
					query += "&modal=1"
					cancel += "?modal=1"
				}
				res, body := a.send(t, staff, http.MethodGet, fieldPath+query, nil, htmxHeader)
				if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) ||
					strings.Contains(body, "student-heading") || strings.Contains(body, `method="dialog"`) ||
					strings.Contains(body, `name="modal" value="1"`) != modal ||
					!strings.Contains(body, `href="`+cancel+`"`) || !strings.Contains(body, `hx-get="`+cancel+`"`) {
					t.Fatalf("single editor modal=%t: status = %d: %s", modal, res.StatusCode, body)
				}
				requireStudentEditor(t, body, original.ID, edit.key, valueOf(field, expected))
				res, body = a.send(t, staff, http.MethodGet, cancel, nil, htmxHeader)
				if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) ||
					!strings.Contains(body, `<p class="student-field-value">`+escapedHTML(valueOf(field, expected))+`</p>`) ||
					strings.Contains(body, "student-field-editor") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "student-heading") {
					t.Fatalf("cancel field modal=%t: status = %d: %s", modal, res.StatusCode, body)
				}
				requireStudentState(t, a, original, source, other, expected)

				form := url.Values{"value": {edit.raw}, "source_link_hash": {"ignored"}, "id": {fmt.Sprint(other.ID)}}
				query = "?modal=1&value=Query+must+not+save"
				if modal {
					form.Set("modal", "1")
					query = "?modal=0"
				}
				res, body = a.send(t, staff, http.MethodPost, fieldPath+query, form, htmxHeader)
				if res.StatusCode != http.StatusOK || !strings.Contains(body, fieldID) ||
					!strings.Contains(body, `<p class="student-field-value">`+escapedHTML(valueOf(field, edit.want))+`</p>`) ||
					!strings.Contains(body, `role="status">Saved.`) || strings.Contains(body, "<!doctype html>") ||
					strings.Contains(body, "student-heading") || strings.Contains(body, "student-field-editor") {
					t.Fatalf("HTMX save modal=%t: status = %d: %s", modal, res.StatusCode, body)
				}
				if strings.Contains(body, `hx-swap-oob="outerHTML"`) != modal || strings.Contains(body, "<template>") != modal {
					t.Fatalf("OOB row must exist only for body modal flag, modal=%t: %s", modal, body)
				}
				if modal {
					_, oob, _ := strings.Cut(body, "<template>")
					if !strings.HasSuffix(strings.TrimSpace(oob), "</template>") {
						t.Fatalf("modal save must wrap OOB row in template: %s", body)
					}
					canonical := original
					canonical.Details = edit.want
					requireStudentRow(t, oob, canonical)
				}
				expected = edit.want
				requireStudentState(t, a, original, source, other, expected)
			}
			res, body := a.send(t, staff, http.MethodGet, fieldPath+"?edit=1&modal=1", nil, nil)
			if res.StatusCode != http.StatusOK || strings.Contains(body, `method="dialog"`) || strings.Contains(body, `name="modal"`) ||
				!strings.Contains(body, "Back to students") {
				t.Fatalf("presentation flag must not turn fallback into dialog: status = %d: %s", res.StatusCode, body)
			}
			requireStudentEditor(t, body, original.ID, edit.key, valueOf(field, expected))
		})
	}
	res, body = a.send(t, staff, http.MethodGet, path, nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("detail reload: status = %d: %s", res.StatusCode, body)
	}
	requireStudentDetails(t, body, expected)
	// Live refresh returns canonical records, never a nested page or source snapshot.
	res, body = a.send(t, staff, http.MethodGet, "/management/students", nil, htmxHeader)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<dialog") {
		t.Fatalf("live records refresh: status = %d: %s", res.StatusCode, body)
	}
	canonical := original
	canonical.Details = expected
	requireStudentRow(t, body, canonical)
	requireStudentRow(t, body, other)
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, htmxHeader)
	requireRefresh(t, res, body, a.list(t))
	if strings.Contains(body, escapedHTML(expected.Name)) || strings.Contains(body, escapedHTML(expected.Phone)) ||
		!strings.Contains(body, escapedHTML(source.Name)) || !strings.Contains(body, escapedHTML(source.Phone)) {
		t.Fatalf("canonical edits changed registry snapshot: %s", body)
	}
	requireRegistryPrivacy(t, body, source.Details)
	requireRegistryPrivacy(t, body, expected)
}

func TestStudentEscapesPersistedHostileName(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	hostile := `<script>alert("student")</script> O'Connor 李`
	if err := a.db.WithContext(t.Context()).Model(&students.Student{}).Where("id = ?", original.ID).Update("name", hostile).Error; err != nil {
		t.Fatal(err)
	}
	expected := original.Details
	expected.Name = hostile
	path := fmt.Sprintf("/management/students/%d", original.ID)
	for _, reload := range []string{path, "/management/students", path + "/fields/name?edit=1"} {
		res, body := a.send(t, staff, http.MethodGet, reload, nil, nil)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, escapedHTML(hostile)) || strings.Contains(body, `<script>alert`) {
			t.Fatalf("escaped persisted value on %s: status = %d: %s", reload, res.StatusCode, body)
		}
	}
	requireStudentState(t, a, original, source, other, expected)
}

func TestStudentRejectedRequestsCannotMutate(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"

	for _, key := range []string{"id", "source_link_hash", "created_at", "missing", "name = 'changed'; DROP TABLE students;--"} {
		t.Run("unknown field "+key, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				res, body := a.send(t, staff, method, path+"/fields/"+url.PathEscape(key), url.Values{"value": {"Changed student"}}, htmxHeader)
				if res.StatusCode != http.StatusNotFound || strings.Contains(body, "student-field-editor") || strings.Contains(body, "hx-swap-oob") {
					t.Fatalf("unknown field: status = %d: %s", res.StatusCode, body)
				}
			}
			requireStudentState(t, a, original, source, other, original.Details)
		})
	}
	for _, id := range []string{"0", "-1", "+1", "not-a-number", "18446744073709551616", "18446744073709551615", "1 OR 1=1"} {
		t.Run("invalid student "+id, func(t *testing.T) {
			missing := "/management/students/" + url.PathEscape(id)
			for _, suffix := range []string{"", "/fields/name?edit=1"} {
				res, body := a.send(t, staff, http.MethodGet, missing+suffix, nil, htmxHeader)
				if res.StatusCode != http.StatusNotFound || strings.Contains(body, "student-details") || strings.Contains(body, "student-field-editor") {
					t.Fatalf("invalid student GET: status = %d: %s", res.StatusCode, body)
				}
			}
			for _, raw := range []string{"value=", "value=%zz", "value=" + strings.Repeat("x", 4096)} {
				res, body := a.sendRaw(t, staff, http.MethodPost, missing+"/fields/name", raw, htmxHeader)
				if res.StatusCode != http.StatusNotFound || strings.Contains(body, "student-field-editor") || strings.Contains(body, "hx-swap-oob") {
					t.Fatalf("missing student must precede body validation: status = %d: %s", res.StatusCode, body)
				}
			}
			requireStudentState(t, a, original, source, other, original.Details)
		})
	}
	for _, tc := range []struct {
		name, raw string
		status    int
	}{
		{"malformed escape", "value=%zz", http.StatusBadRequest},
		{"malformed delimiter", "value=changed;other=value", http.StatusBadRequest},
		{"oversized", "value=" + strings.Repeat("x", 4096), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := a.sendRaw(t, staff, http.MethodPost, fieldPath, tc.raw, htmxHeader)
			if res.StatusCode != tc.status || strings.Contains(body, "student-field-editor") || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") {
				t.Fatalf("rejected body: status = %d, want %d: %s", res.StatusCode, tc.status, body)
			}
			requireStudentState(t, a, original, source, other, original.Details)
		})
	}

	// A concurrent disappearance (or a database-suppressed update) is not a save.
	if err := a.db.WithContext(t.Context()).Exec(`CREATE FUNCTION suppress_student_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.WithContext(t.Context()).Exec(`CREATE TRIGGER suppress_student_update BEFORE UPDATE ON students FOR EACH ROW EXECUTE FUNCTION suppress_student_update()`).Error; err != nil {
		t.Fatal(err)
	}
	res, body := a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {"Changed student"}, "modal": {"1"}}, htmxHeader)
	if res.StatusCode != http.StatusNotFound || strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") {
		t.Fatalf("zero-row save: status = %d: %s", res.StatusCode, body)
	}
	requireStudentState(t, a, original, source, other, original.Details)
}

func TestStudentEmptyAndDatabaseFailures(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	res, body := a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `class="student-empty"`) || strings.Contains(body, `<table`) {
		t.Fatalf("empty archive: status = %d: %s", res.StatusCode, body)
	}
	original, source := enrollStudent(t, a, "Original submission")
	other, _ := enrollStudent(t, a, "Unrelated student")
	path := fmt.Sprintf("/management/students/%d", original.ID)
	fieldPath := path + "/fields/name"
	if err := a.db.WithContext(t.Context()).Exec(`ALTER TABLE students ADD CONSTRAINT reject_test_student_name CHECK (name <> 'Rejected by database')`).Error; err != nil {
		t.Fatal(err)
	}
	for _, headers := range []http.Header{nil, htmxHeader} {
		res, body = a.send(t, staff, http.MethodPost, fieldPath, url.Values{"value": {"Rejected by database"}, "modal": {"1"}}, headers)
		if res.StatusCode != http.StatusInternalServerError || strings.Contains(body, "student-field-editor") ||
			strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "Rejected by database") {
			t.Fatalf("failed database write: status = %d: %s", res.StatusCode, body)
		}
		requireStudentState(t, a, original, source, other, original.Details)
	}
	if err := a.db.WithContext(t.Context()).Exec(`ALTER TABLE students RENAME TO unavailable_students`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/management/students"},
		{http.MethodGet, path + "?saved=name"},
		{http.MethodGet, fieldPath + "?edit=1"},
		{http.MethodPost, fieldPath},
	} {
		for _, headers := range []http.Header{nil, htmxHeader} {
			res, body = a.send(t, staff, tc.method, tc.path, url.Values{"value": {"Changed student"}, "modal": {"1"}}, headers)
			if res.StatusCode != http.StatusInternalServerError || strings.Contains(body, "student-empty") ||
				strings.Contains(body, "Saved.") || strings.Contains(body, "hx-swap-oob") || strings.Contains(body, "Original submission") {
				t.Fatalf("failed database read %s %s: status = %d: %s", tc.method, tc.path, res.StatusCode, body)
			}
		}
	}
}

// Staff see a student's PIN and may change it, but never onto another student's.
func TestStudentPinEditKeepsPinsUnique(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")
	first, _ := enrollStudent(t, a, "First student")
	second, _ := enrollStudent(t, a, "Second student")
	path := fmt.Sprintf("/management/students/%d", second.ID)
	stored := func() string {
		got, err := students.New(a.db, nil).Get(t.Context(), second.ID)
		if err != nil || got == nil {
			t.Fatalf("reload: %+v, %v", got, err)
		}
		return got.Pin
	}

	res, body := a.send(t, staff, http.MethodGet, path, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `<p class="student-field-value">`+second.Pin+`</p>`) {
		t.Fatalf("staff cannot see the PIN: status = %d: %s", res.StatusCode, body)
	}
	res, body = a.send(t, staff, http.MethodPost, path+"/fields/pin", url.Values{"value": {first.Pin}}, htmxHeader)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "That PIN is already in use.") || stored() != second.Pin {
		t.Fatalf("duplicate PIN: status = %d, stored %q: %s", res.StatusCode, stored(), body)
	}
	res, body = a.send(t, staff, http.MethodPost, path+"/fields/pin", url.Values{"value": {"000999"}}, htmxHeader)
	if res.StatusCode != http.StatusOK || stored() != "000999" {
		t.Fatalf("free PIN: status = %d, stored %q: %s", res.StatusCode, stored(), body)
	}
}

func TestOnlyPaidCustomersAppearInStudentsTab(t *testing.T) {
	a := newApp(t)
	staff := a.signIn(t, "staff")

	// Customer 1: submits form, unpaid (Awaiting payment)
	entry1, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	details1 := contactDetails("Unpaid Customer")
	details1.Email = "unpaid.secret@example.com"
	details1.Phone = "+17875551111"
	if saved, err := a.store.Submit(t.Context(), entry1.Token, details1, "123456"); err != nil || saved != links.Saved {
		t.Fatalf("submit customer 1: %v, %v", saved, err)
	}

	// Customer 2: submits form and pays (enrolled via links.Enroll)
	entry2, err := a.store.Create(t.Context(), "staff", later)
	if err != nil {
		t.Fatal(err)
	}
	details2 := contactDetails("Paid Customer")
	details2.Email = "paid.student@example.com"
	details2.Phone = "+17875552222"
	if saved, err := a.store.Submit(t.Context(), entry2.Token, details2, "654321"); err != nil || saved != links.Saved {
		t.Fatalf("submit customer 2: %v, %v", saved, err)
	}
	acceptAgreement(t, a, entry2.Token)
	hash2, _ := hex.DecodeString(entry2.ID)
	now := time.Now().UTC()
	if err := a.db.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, hash2, now)
	}); err != nil {
		t.Fatalf("enroll customer 2: %v", err)
	}

	// GET /management/students: lists ONLY the paid one
	res, body := a.send(t, staff, http.MethodGet, "/management/students", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /management/students: %d", res.StatusCode)
	}
	if !strings.Contains(body, "Paid Customer") {
		t.Fatalf("students tab missing paid student: %s", body)
	}
	if strings.Contains(body, "Unpaid Customer") {
		t.Fatalf("students tab leaked unpaid customer: %s", body)
	}

	// GET /management/: lists the unpaid one with "Awaiting payment", name, phone, NO email
	res, body = a.send(t, staff, http.MethodGet, "/management/", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /management/: %d", res.StatusCode)
	}
	if !strings.Contains(body, "Unpaid Customer") || !strings.Contains(body, "Awaiting payment") ||
		!strings.Contains(body, "17875551111") {
		t.Fatalf("links page missing unpaid customer name/phone/status: %s", body)
	}
	if strings.Contains(body, "unpaid.secret@example.com") {
		t.Fatalf("links page leaked customer email: %s", body)
	}
}
