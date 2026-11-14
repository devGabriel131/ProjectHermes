package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"projecthermes/internal/billing"
	"projecthermes/internal/logging"
	"projecthermes/internal/students"
)

type studentFieldView struct {
	students.Field
	StudentID uint64
	Value     string
	Modal     bool
	Editing   bool
	Error     string
	Saved     bool
}

type studentView struct {
	students.Student
	Fields []studentFieldView
	OOB    bool
}

type studentPageData struct {
	Staff    string
	Fields   []students.Field
	Students []studentView
	Student  studentView
	Field    *studentFieldView
	Modal    bool
	// The roster's filters, "" for all: Status is "active" or "suspended", Cohort one of
	// Cohorts, and Group one of Groups.
	Status, Cohort, Group string
	Cohorts               []string // every cohort with a student, newest first
	Groups                []string
	Billing               *billing.Summary // nil when the student has no billing contract.
}

// studentDisplay evaluates the field registry once for each rendered student.
// The same values feed table rows, detail fields, and saved-field responses.
func studentDisplay(record students.Student, modal bool) studentView {
	fields := students.StaffFields()
	view := studentView{Student: record, Fields: make([]studentFieldView, len(fields))}
	for i, field := range fields {
		view.Fields[i] = studentFieldView{
			Field: field, StudentID: record.ID, Value: field.Value(record), Modal: modal,
		}
	}
	return view
}

func (view *studentView) field(key string) *studentFieldView {
	for i := range view.Fields {
		if view.Fields[i].Key == key {
			return &view.Fields[i]
		}
	}
	return nil
}

// pathID reads a path value as a row ID. This is where the BIGINT range is checked, once: the stores take any uint64.
func pathID(r *http.Request, name string) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue(name), 10, 63)
	return id, err == nil && id >= 1
}

// billingRefusals are the billing errors staff can act on. They keep their
// status, so htmx leaves the page as it is; anything else is a logged 500.
var billingRefusals = []struct {
	err     error
	status  int
	message string
}{
	{billing.ErrUnavailable, http.StatusNotFound, "Student or billing record not found."},
	{billing.ErrNotSuspended, http.StatusConflict, "This student is not suspended for billing."},
	{billing.ErrNeedsReview, http.StatusConflict, "This student's billing needs review before this can continue."},
	{billing.ErrPaymentPending, http.StatusConflict, "A payment is still pending. Try again shortly."},
	{billing.ErrInvoiceNotReady, http.StatusConflict, "This invoice is not ready yet."},
}

func billingFailure(w http.ResponseWriter, r *http.Request, err error) {
	for _, refusal := range billingRefusals {
		if errors.Is(err, refusal.err) {
			http.Error(w, refusal.message, refusal.status)
			return
		}
	}
	logging.RecordError(r, "billing", err)
	http.Error(w, "Could not complete the billing request. Please try again.", http.StatusInternalServerError)
}

func studentNotFound(w http.ResponseWriter) {
	http.Error(w, "Student or field not found.", http.StatusNotFound)
}

// load reads the student the {id} path value names; nil means it has answered.
func (s *site) load(w http.ResponseWriter, r *http.Request) *students.Student {
	id, ok := pathID(r, "id")
	if !ok {
		studentNotFound(w)
		return nil
	}
	record, err := s.roster.Get(r.Context(), id)
	if err != nil {
		logging.RecordError(r, "load", err)
		http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
		return nil
	}
	if record == nil {
		studentNotFound(w)
	}
	return record
}

// withBilling adds the student's billing summary to a page that shows their details.
func (s *site) withBilling(w http.ResponseWriter, r *http.Request, data *studentPageData) bool {
	summary, err := s.billing.ForStudent(r.Context(), data.Student.ID)
	if err != nil {
		logging.RecordError(r, "billing", err)
		http.Error(w, "Could not load billing. Please try again.", http.StatusInternalServerError)
		return false
	}
	data.Billing = summary
	return true
}

func (s *site) studentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cohorts, err := s.roster.Cohorts(ctx)
	if err != nil {
		logging.RecordError(r, "list", err)
		http.Error(w, "Could not load students. Please try again.", http.StatusInternalServerError)
		return
	}
	// Unknown filter values show everyone, so a stale link never hides the roster.
	query := r.URL.Query()
	data := studentPageData{
		Staff: s.staff(ctx), Fields: students.StaffFields(), Cohorts: cohorts, Groups: students.GroupOptions(),
		Status: query.Get("status"), Cohort: query.Get("cohort"), Group: query.Get("group"),
	}
	var filter students.Filter
	if data.Status == "active" || data.Status == "suspended" {
		isActive := data.Status == "active"
		filter.Active = &isActive
	} else {
		data.Status = ""
	}
	if slices.Contains(data.Cohorts, data.Cohort) {
		filter.Cohort = data.Cohort
	} else {
		data.Cohort = ""
	}
	if slices.Contains(data.Groups, data.Group) {
		filter.Group = data.Group
	} else {
		data.Group = ""
	}
	records, err := s.roster.List(ctx, filter)
	if err != nil {
		logging.RecordError(r, "list", err)
		http.Error(w, "Could not load students. Please try again.", http.StatusInternalServerError)
		return
	}
	data.Students = make([]studentView, len(records))
	for i, record := range records {
		data.Students[i] = studentDisplay(record, false)
	}
	name := "students.html"
	if htmx(r) {
		// Filter changes and live refreshes swap just the records. The address bar
		// follows the shown filters, so a reload or bookmark reopens them.
		name = "student-records"
		shown := url.Values{}
		for key, value := range map[string]string{"status": data.Status, "cohort": data.Cohort, "group": data.Group} {
			if value != "" {
				shown.Set(key, value)
			}
		}
		w.Header().Set("HX-Replace-Url", strings.TrimSuffix("/management/students?"+shown.Encode(), "?"))
	}
	s.render(w, r, http.StatusOK, name, data)
}

func (s *site) student(w http.ResponseWriter, r *http.Request) {
	record := s.load(w, r)
	if record == nil {
		return
	}
	modal := htmx(r)
	data := studentPageData{Staff: s.staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
	if field := data.Student.field(r.URL.Query().Get("saved")); field != nil {
		field.Saved = true
	}
	if !s.withBilling(w, r, &data) {
		return
	}
	name := "students.html"
	if modal {
		name = "student-detail"
	}
	s.render(w, r, http.StatusOK, name, data)
}

func (s *site) studentField(w http.ResponseWriter, r *http.Request) {
	field, ok := students.FindStaffField(r.PathValue("field"))
	if !ok {
		studentNotFound(w)
		return
	}
	// Resolve the record before parsing even a malformed or oversized body.
	record := s.load(w, r)
	if record == nil {
		return
	}

	partial := htmx(r)
	modal := r.URL.Query().Get("modal") == "1"
	editing := r.URL.Query().Get("edit") == "1"
	status := http.StatusOK
	var attempted, validation string
	saved := false
	if r.Method == http.MethodPost {
		if !readForm(w, r) {
			return
		}
		modal = r.PostForm.Get("modal") == "1"
		attempted = r.PostForm.Get("value")
		updated, err := s.roster.UpdateField(r.Context(), record.ID, field.Key, attempted)
		var invalid *students.ValidationError
		switch {
		case errors.As(err, &invalid):
			status, editing, validation = http.StatusUnprocessableEntity, true, invalid.Message
		case err != nil:
			logging.RecordError(r, "save", err)
			http.Error(w, "Could not save this field. Please try again.", http.StatusInternalServerError)
			return
		case updated == nil:
			studentNotFound(w)
			return
		default:
			s.billing.TouchContract(r.Context(), record.EnrollmentID)
			if !partial {
				path := "/management/students/" + strconv.FormatUint(record.ID, 10) + "?saved=" + url.QueryEscape(field.Key)
				http.Redirect(w, r, path, http.StatusSeeOther)
				return
			}
			record, editing, saved = updated, false, true
		}
	}

	modal = partial && modal
	data := studentPageData{Staff: s.staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
	data.Field = data.Student.field(field.Key)
	data.Field.Editing, data.Field.Error, data.Field.Saved = editing, validation, saved
	if validation != "" {
		data.Field.Value = attempted
	}
	if !partial {
		if !s.withBilling(w, r, &data) {
			return
		}
		s.render(w, r, status, "students.html", data)
		return
	}
	if saved {
		data.Student.OOB = modal
		s.render(w, r, status, "student-save", data)
		return
	}
	s.render(w, r, status, "student-field", data.Field)
}

// studentBilling is the billing fragment: read-only, so it never writes to the provider.
func (s *site) studentBilling(w http.ResponseWriter, r *http.Request) {
	record := s.load(w, r)
	if record == nil {
		return
	}
	data := studentPageData{Student: studentView{Student: *record}}
	if !s.withBilling(w, r, &data) {
		return
	}
	s.render(w, r, http.StatusOK, "student-billing", data)
}

// act runs a staff-attributed billing action and answers with the fresh fragment. It is not timed:
// billing bounds the provider work with its own providerBudget, and no page read follows.
func (s *site) act(do func(context.Context, uint64) (*billing.Summary, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			studentNotFound(w)
			return
		}
		summary, err := do(r.Context(), id)
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		s.render(w, r, http.StatusOK, "student-billing", studentPageData{
			Student: studentView{Student: students.Student{ID: id}}, Billing: summary,
		})
	}
}

func (s *site) requestReinstatement(ctx context.Context, studentID uint64) (*billing.Summary, error) {
	return s.billing.RequestReinstatement(ctx, studentID, s.staff(ctx))
}

// invoice sends staff to a hosted invoice. Its links expire and are never stored, so one is fetched now.
func (s *site) invoice(w http.ResponseWriter, r *http.Request) {
	id, studentOK := pathID(r, "id")
	obligation, obligationOK := pathID(r, "obligation")
	if !studentOK || !obligationOK {
		studentNotFound(w)
		return
	}
	invoice, err := s.billing.InvoiceURL(r.Context(), id, obligation)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, invoice, http.StatusSeeOther)
}

// portal shows the payer's portal link, a bearer credential: shown once, never cached or referred.
func (s *site) portal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		studentNotFound(w)
		return
	}
	portal, err := s.billing.PortalURL(r.Context(), id)
	if err != nil {
		billingFailure(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "student-billing-portal", portal)
}
