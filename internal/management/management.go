// Package management serves the staff pages: customer links and Trash, students with billing actions, the dashboard and settings. Access is enforced by auth.Require in main.go, not here. See docs/management/overview.md.
package management

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
)

//go:embed templates/*.html
var templates embed.FS

// dbTimeout bounds each request's database work.
const dbTimeout = 5 * time.Second

// linkRow is a link group as staff see it; Declined marks enrollments whose first payment was declined.
type linkRow struct {
	links.LinkGroup
	Declined map[uint64]bool
}

type pageData struct {
	Links              []linkRow
	Notice             string
	Staff              string
	Trash              bool
	ProgramStartDate   string
	ProgramStartError  string
	ProgramStartNotice string
}

type settingsData struct {
	Current       settings.Settings
	Min, Max      int
	Error, Notice string
	// BootcampDate is the date input's value: the saved date, or the rejected submission.
	BootcampDate                  string
	BootcampError, BootcampNotice string
	Staff                         string
}

// site is the staff handler set: one template set over the stores the pages read and write.
type site struct {
	page     *template.Template
	links    *links.Store
	roster   *students.Store
	settings *settings.Store
	billing  *billing.Service
	staff    func(context.Context) string // the signed-in username, for the audit trail
}

// plural picks the noun form that agrees with n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// RegisterRoutes serves customer links, Trash, Students, the dashboard and settings; staff
// returns the verified username of the signed-in account for the audit trail.
// biller supplies each student's billing summary and the staff billing actions.
func RegisterRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, config *settings.Store, staff func(context.Context) string, biller *billing.Service) error {
	page, err := template.New("management").Funcs(template.FuncMap{
		"money":       payments.Money,
		"plural":      plural,
		"cohortLabel": students.CohortLabel,
	}).ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}

	s := &site{page: page, links: store, roster: roster, settings: config, billing: biller, staff: staff}
	mux.Handle("GET /management/{$}", timed(s.index))
	mux.Handle("GET /management/trash", timed(s.trash))
	mux.Handle("POST /management/program-start-date", timed(s.setProgramStartDate))
	mux.Handle("POST /management/links", timed(s.createLink))
	mux.Handle("POST /management/links/{id}/rename", timed(s.renameLink))
	mux.Handle("POST /management/links/{id}/cancel", timed(s.cancelLink))
	mux.Handle("POST /management/enrollments/{id}/cancel", http.HandlerFunc(s.cancelEnrollment))
	mux.Handle("GET /management/settings", timed(s.settingsPage))
	mux.Handle("POST /management/settings", timed(s.setLinkLifetime))
	mux.Handle("POST /management/bootcamp-date", timed(s.setBootcampDate))
	mux.Handle("GET /management/dashboard", timed(s.dashboard))
	mux.Handle("GET /management/students", timed(s.studentList))
	mux.Handle("POST /management/students", timed(s.addStudent))
	mux.Handle("GET /management/students/new", timed(s.addStudentsPage))
	mux.Handle("POST /management/students/import", timed(s.importStudents))
	mux.Handle("GET /management/students/{id}", timed(s.student))
	mux.Handle("GET /management/students/{id}/fields/{field}", timed(s.studentField))
	mux.Handle("POST /management/students/{id}/fields/{field}", timed(s.studentField))
	mux.Handle("GET /management/students/{id}/billing", timed(s.studentBilling))
	mux.Handle("POST /management/students/{id}/billing/reconcile", s.act(s.billing.ReconcileStudent))
	mux.Handle("POST /management/students/{id}/billing/reinstatement", s.act(s.requestReinstatement))
	mux.Handle("POST /management/students/{id}/billing/withdraw", s.act(s.withdraw))
	mux.Handle("GET /management/students/{id}/billing/invoices/{obligation}", http.HandlerFunc(s.invoice))
	mux.Handle("POST /management/students/{id}/billing/portal", http.HandlerFunc(s.portal))
	return nil
}

// timed bounds h's request context by dbTimeout. Only handlers whose work is all database
// get it: provider calls run under billing's own longer providerBudget, which it would cut short.
func timed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		defer cancel()
		h(w, r.WithContext(ctx))
	})
}

// render writes the named template with status. Responses are never cached,
// and vary on HX-Request because htmx gets fragments where browsers get pages.
func (s *site) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Vary", "HX-Request")
	w.WriteHeader(status)
	if err := s.page.ExecuteTemplate(w, name, data); err != nil {
		logging.RecordError(r, "render", err)
	}
}

// htmx reports whether r comes from htmx, which never gets a full page:
// live list refreshes get rows, and actions get their notice.
func htmx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// postedValue reads the single value of name from a form body of at most 1 KiB. ok is false when
// the body is unreadable or name is missing or repeated; value is then the first one sent, if any.
func postedValue(w http.ResponseWriter, r *http.Request, name string) (value string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	err := r.ParseForm()
	values := r.PostForm[name]
	if len(values) == 0 {
		return "", false
	}
	return values[0], err == nil && len(values) == 1
}

// readForm parses a form body of at most 4 KiB. When it cannot, it has answered 413 or 400.
func readForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	err := r.ParseForm()
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		http.Error(w, "This form is too large.", http.StatusRequestEntityTooLarge)
	case err != nil:
		http.Error(w, "Could not read this form. Please try again.", http.StatusBadRequest)
	}
	return err == nil
}

// list renders a tab's registry as a full page, or as just its rows for an htmx refresh.
func (s *site) list(w http.ResponseWriter, r *http.Request, status int, data pageData) {
	ctx := r.Context()
	groups, err := s.links.ListGroups(ctx)
	if err != nil {
		logging.RecordError(r, "list", err)
		http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
		return
	}
	var declined map[uint64]bool
	if !data.Trash { // Trash holds no Awaiting payment rows, the only ones with the badge
		if declined, err = s.billing.DeclinedEnrollments(ctx); err != nil {
			logging.RecordError(r, "list", err)
			http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
			return
		}
	}
	now := time.Now()
	filtered := make([]linkRow, 0, len(groups))
	for _, g := range groups {
		g.Enrollments = slices.DeleteFunc(g.Enrollments, func(e links.EnrollmentEntry) bool {
			return e.Trashed() != data.Trash || (!data.Trash && e.Archived(now))
		})
		if g.Link.Trashed() == data.Trash || len(g.Enrollments) > 0 {
			filtered = append(filtered, linkRow{LinkGroup: g, Declined: declined})
		}
	}
	data.Links = filtered
	name := "link-rows"
	if !htmx(r) || status != http.StatusOK {
		name = "index.html"
		data.Staff = s.staff(ctx)
		if !data.Trash {
			current, err := s.settings.Current(ctx)
			if err != nil {
				logging.RecordError(r, "load", err)
				http.Error(w, "Could not load settings. Please try again.", http.StatusInternalServerError)
				return
			}
			if data.ProgramStartError == "" {
				data.ProgramStartDate = current.ProgramStartDate
			}
		}
	}
	s.render(w, r, status, name, data)
}

// answer reports a link or enrollment action. A browser gets the links page with the notice. htmx gets
// a refusal as plain text, 204 when the ID named nothing (found is false), or else the notice with
// HX-Trigger: management-changed, so the page refreshes its own list.
func (s *site) answer(w http.ResponseWriter, r *http.Request, status int, notice string, found bool) {
	switch {
	case !htmx(r):
		s.list(w, r, status, pageData{Notice: notice})
	case status != http.StatusOK:
		http.Error(w, notice, status)
	case !found:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("HX-Trigger", "management-changed")
		s.render(w, r, http.StatusOK, "result", pageData{Notice: notice})
	}
}

func (s *site) index(w http.ResponseWriter, r *http.Request) {
	var data pageData
	if r.URL.Query().Has("program-start-date-saved") {
		data.ProgramStartNotice = "Fecha de comienzo guardada."
	}
	s.list(w, r, http.StatusOK, data)
}

func (s *site) trash(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, http.StatusOK, pageData{Trash: true})
}

func (s *site) setProgramStartDate(w http.ResponseWriter, r *http.Request) {
	date, ok := postedValue(w, r, "program_start_date")
	err := settings.ErrDate
	if ok {
		err = s.settings.SetProgramStartDate(r.Context(), date, s.staff(r.Context()))
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/management/?program-start-date-saved", http.StatusSeeOther)
	case errors.Is(err, settings.ErrDate):
		s.list(w, r, http.StatusUnprocessableEntity, pageData{ProgramStartDate: date, ProgramStartError: "Ingresa una fecha válida en formato AAAA-MM-DD."})
	default:
		logging.RecordError(r, "save", err)
		http.Error(w, "Could not save the program start date. Please try again.", http.StatusInternalServerError)
	}
}

func (s *site) createLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	current, err := s.settings.Current(ctx)
	if err == nil {
		_, err = s.links.CreateLink(ctx, s.staff(ctx), "", time.Now().Add(current.LinkLifetime()))
	}
	if err != nil {
		logging.RecordError(r, "create", err)
		http.Error(w, "Could not generate a link. Please try again.", http.StatusInternalServerError)
		return
	}
	s.answer(w, r, http.StatusOK, "Link created.", true)
}

func (s *site) renameLink(w http.ResponseWriter, r *http.Request) {
	name, ok := postedValue(w, r, "name")
	if !ok {
		s.answer(w, r, http.StatusBadRequest, "Could not read the link name.", true)
		return
	}
	var renamed *links.LinkEntry
	var err error
	if id, ok := pathID(r, "id"); ok {
		renamed, err = s.links.RenameLink(r.Context(), id, name, s.staff(r.Context()))
	}
	switch {
	case errors.Is(err, links.ErrInvalidLinkName):
		s.answer(w, r, http.StatusUnprocessableEntity, "Enter a link name (up to 100 characters).", true)
	case err != nil:
		logging.RecordError(r, "rename", err)
		http.Error(w, "Could not rename this link. Please try again.", http.StatusInternalServerError)
	case renamed == nil:
		s.answer(w, r, http.StatusOK, "Link can no longer be renamed.", false)
	default:
		s.answer(w, r, http.StatusOK, "Link renamed.", true)
	}
}

func (s *site) cancelLink(w http.ResponseWriter, r *http.Request) {
	var canceled *links.LinkEntry
	var err error
	if id, ok := pathID(r, "id"); ok {
		canceled, err = s.links.CancelLink(r.Context(), id, s.staff(r.Context()))
	}
	if err != nil {
		logging.RecordError(r, "cancel", err)
		http.Error(w, "Could not cancel this link. Please try again.", http.StatusInternalServerError)
		return
	}
	notice := "Link canceled."
	if canceled == nil || canceled.Status != links.LinkCanceled {
		notice = "Link can no longer be canceled."
	}
	s.answer(w, r, http.StatusOK, notice, canceled != nil)
}

// cancelEnrollment is not timed: billing may expire a checkout session at the provider under its own
// providerBudget. Only the page reads after it get dbTimeout.
func (s *site) cancelEnrollment(w http.ResponseWriter, r *http.Request) {
	var (
		canceled *links.EnrollmentEntry
		changed  bool
		err      error
	)
	if id, ok := pathID(r, "id"); ok {
		canceled, changed, err = s.billing.CancelEnrollment(r.Context(), id, s.staff(r.Context()))
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	switch {
	case errors.Is(err, billing.ErrCancelBlocked):
		s.answer(w, r, http.StatusConflict, "Cannot cancel while a payment is in progress.", true)
	case err != nil:
		logging.RecordError(r, "cancel", err)
		http.Error(w, "Could not cancel this enrollment. Please try again.", http.StatusInternalServerError)
	default:
		s.answer(w, r, http.StatusOK, cancelEnrollmentNotice(canceled, changed), canceled != nil)
	}
}

func cancelEnrollmentNotice(canceled *links.EnrollmentEntry, changed bool) string {
	switch {
	case canceled == nil || !changed:
		return "Enrollment can no longer be canceled."
	case canceled.Status == links.Expired:
		return "Expired PIN cleared."
	default:
		return "Enrollment canceled."
	}
}

// showSettings renders the settings page with the saved values.
func (s *site) showSettings(w http.ResponseWriter, r *http.Request, status int, data settingsData) {
	var err error
	if data.Current, err = s.settings.Current(r.Context()); err != nil {
		logging.RecordError(r, "load", err)
		http.Error(w, "Could not load settings. Please try again.", http.StatusInternalServerError)
		return
	}
	data.Min, data.Max = settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays
	if data.BootcampError == "" {
		data.BootcampDate = data.Current.BootcampDate
	}
	data.Staff = s.staff(r.Context())
	s.render(w, r, status, "settings.html", data)
}

func (s *site) settingsPage(w http.ResponseWriter, r *http.Request) {
	var data settingsData
	if r.URL.Query().Has("saved") {
		data.Notice = "Saved."
	}
	if r.URL.Query().Has("bootcamp-date-saved") {
		data.BootcampNotice = "Saved."
	}
	s.showSettings(w, r, http.StatusOK, data)
}

func (s *site) setBootcampDate(w http.ResponseWriter, r *http.Request) {
	date, ok := postedValue(w, r, "bootcamp_date")
	err := settings.ErrDate
	if ok {
		err = s.settings.SetBootcampDate(r.Context(), date, s.staff(r.Context()))
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/management/settings?bootcamp-date-saved", http.StatusSeeOther)
	case errors.Is(err, settings.ErrDate):
		s.showSettings(w, r, http.StatusUnprocessableEntity, settingsData{BootcampDate: date, BootcampError: "Enter a valid date (YYYY-MM-DD)."})
	default:
		logging.RecordError(r, "save", err)
		http.Error(w, "Could not save the bootcamp date. Please try again.", http.StatusInternalServerError)
	}
}

func (s *site) setLinkLifetime(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	// Anything that isn't a whole number is as out of range as 1 or 15.
	err := settings.ErrLinkLifetimeRange
	if days, parseErr := strconv.Atoi(r.PostFormValue("lifetime")); parseErr == nil {
		err = s.settings.SetLinkLifetimeDays(r.Context(), days, s.staff(r.Context()))
	}
	switch {
	case err == nil:
		// Redirect so a reload doesn't resubmit the form.
		http.Redirect(w, r, "/management/settings?saved", http.StatusSeeOther)
	case errors.Is(err, settings.ErrLinkLifetimeRange):
		s.showSettings(w, r, http.StatusUnprocessableEntity, settingsData{
			Error: fmt.Sprintf("Choose between %d and %d days.", settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays),
		})
	default:
		logging.RecordError(r, "save", err)
		http.Error(w, "Could not save settings. Please try again.", http.StatusInternalServerError)
	}
}
