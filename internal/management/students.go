package management

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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

// pathID reads a path value as a signed-BIGINT row ID, so no number SQL cannot hold reaches it.
func pathID(r *http.Request, name string) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue(name), 10, 64)
	return id, err == nil && id >= 1 && id <= 1<<63-1
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

func registerStudentRoutes(mux *http.ServeMux, roster *students.Store, biller *billing.Service, staff func(context.Context) string, page *template.Template) {
	notFound := func(w http.ResponseWriter) {
		http.Error(w, "Student or field not found.", http.StatusNotFound)
	}
	load := func(ctx context.Context, w http.ResponseWriter, r *http.Request) *students.Student {
		id, ok := pathID(r, "id")
		if !ok {
			notFound(w)
			return nil
		}
		record, err := roster.Get(ctx, id)
		if err != nil {
			logging.RecordError(r, "load", err)
			http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
			return nil
		}
		if record == nil {
			notFound(w)
		}
		return record
	}
	// withBilling adds the student's billing summary to a page that shows their details.
	withBilling := func(ctx context.Context, w http.ResponseWriter, r *http.Request, data *studentPageData) bool {
		summary, err := biller.ForStudent(ctx, data.Student.ID)
		if err != nil {
			logging.RecordError(r, "billing", err)
			http.Error(w, "Could not load billing. Please try again.", http.StatusInternalServerError)
			return false
		}
		data.Billing = summary
		return true
	}

	mux.Handle("GET /management/students", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		cohorts, err := roster.Cohorts(ctx)
		if err != nil {
			logging.RecordError(r, "list", err)
			http.Error(w, "Could not load students. Please try again.", http.StatusInternalServerError)
			return
		}
		// Unknown filter values show everyone, so a stale link never hides the roster.
		query := r.URL.Query()
		data := studentPageData{
			Staff: staff(r.Context()), Fields: students.StaffFields(), Cohorts: cohorts, Groups: students.GroupOptions(),
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
		records, err := roster.List(ctx, filter)
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
		render(w, r, page, http.StatusOK, name, data)
	}))

	mux.Handle("GET /management/students/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record := load(ctx, w, r)
		if record == nil {
			return
		}
		modal := htmx(r)
		data := studentPageData{Staff: staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
		if field := data.Student.field(r.URL.Query().Get("saved")); field != nil {
			field.Saved = true
		}
		if !withBilling(ctx, w, r, &data) {
			return
		}
		name := "students.html"
		if modal {
			name = "student-detail"
		}
		render(w, r, page, http.StatusOK, name, data)
	}))

	fieldHandler := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		field, ok := students.FindStaffField(r.PathValue("field"))
		if !ok {
			notFound(w)
			return
		}
		// Resolve the record before parsing even a malformed or oversized body.
		record := load(ctx, w, r)
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
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			if err := r.ParseForm(); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "This value is too large.", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "Could not read this field. Please try again.", http.StatusBadRequest)
				}
				return
			}
			modal = r.PostForm.Get("modal") == "1"
			attempted = r.PostForm.Get("value")
			updated, err := roster.UpdateField(ctx, record.ID, field.Key, attempted)
			var invalid *students.ValidationError
			switch {
			case errors.As(err, &invalid):
				status, editing, validation = http.StatusUnprocessableEntity, true, invalid.Message
			case err != nil:
				logging.RecordError(r, "save", err)
				http.Error(w, "Could not save this field. Please try again.", http.StatusInternalServerError)
				return
			case updated == nil:
				notFound(w)
				return
			default:
				biller.TouchContract(ctx, record.EnrollmentID)
				if !partial {
					path := "/management/students/" + strconv.FormatUint(record.ID, 10) + "?saved=" + url.QueryEscape(field.Key)
					http.Redirect(w, r, path, http.StatusSeeOther)
					return
				}
				record, editing, saved = updated, false, true
			}
		}

		modal = partial && modal
		data := studentPageData{Staff: staff(r.Context()), Student: studentDisplay(*record, modal), Modal: modal}
		data.Field = data.Student.field(field.Key)
		data.Field.Editing, data.Field.Error, data.Field.Saved = editing, validation, saved
		if validation != "" {
			data.Field.Value = attempted
		}
		if !partial {
			if !withBilling(ctx, w, r, &data) {
				return
			}
			render(w, r, page, status, "students.html", data)
			return
		}
		if saved {
			data.Student.OOB = modal
			render(w, r, page, status, "student-save", data)
			return
		}
		render(w, r, page, status, "student-field", data.Field)
	}
	mux.Handle("GET /management/students/{id}/fields/{field}", http.HandlerFunc(fieldHandler))
	mux.Handle("POST /management/students/{id}/fields/{field}", http.HandlerFunc(fieldHandler))

	// Billing fragment: read-only, so it never writes to the provider.
	mux.Handle("GET /management/students/{id}/billing", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record := load(ctx, w, r)
		if record == nil {
			return
		}
		data := studentPageData{Student: studentView{Student: *record}}
		if !withBilling(ctx, w, r, &data) {
			return
		}
		render(w, r, page, http.StatusOK, "student-billing", data)
	}))

	// act runs a staff-attributed billing action and answers with the fresh fragment.
	act := func(do func(context.Context, uint64) (*billing.Summary, error)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := pathID(r, "id")
			if !ok {
				notFound(w)
				return
			}
			summary, err := do(r.Context(), id)
			if err != nil {
				billingFailure(w, r, err)
				return
			}
			render(w, r, page, http.StatusOK, "student-billing", studentPageData{
				Student: studentView{Student: students.Student{ID: id}}, Billing: summary,
			})
		})
	}
	mux.Handle("POST /management/students/{id}/billing/reconcile", act(biller.ReconcileStudent))
	mux.Handle("POST /management/students/{id}/billing/reinstatement", act(func(ctx context.Context, id uint64) (*billing.Summary, error) {
		return biller.RequestReinstatement(ctx, id, staff(ctx))
	}))

	// Hosted invoice links expire and are never stored: fetch one now and send staff there.
	mux.Handle("GET /management/students/{id}/billing/invoices/{obligation}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, studentOK := pathID(r, "id")
		obligation, obligationOK := pathID(r, "obligation")
		if !studentOK || !obligationOK {
			notFound(w)
			return
		}
		invoice, err := biller.InvoiceURL(r.Context(), id, obligation)
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, invoice, http.StatusSeeOther)
	}))

	// The portal link is a bearer credential for the payer: shown once, never cached or referred.
	mux.Handle("POST /management/students/{id}/billing/portal", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			notFound(w)
			return
		}
		portal, err := biller.PortalURL(r.Context(), id)
		if err != nil {
			billingFailure(w, r, err)
			return
		}
		render(w, r, page, http.StatusOK, "student-billing-portal", portal)
	}))
}
