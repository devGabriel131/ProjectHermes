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

type pageData struct {
	Links              []links.Entry
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
	Staff         string
}

// render writes the named template with status. Responses are never cached,
// and vary on HX-Request because htmx gets fragments where browsers get pages.
func render(w http.ResponseWriter, r *http.Request, page *template.Template, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Vary", "HX-Request")
	w.WriteHeader(status)
	if err := page.ExecuteTemplate(w, name, data); err != nil {
		logging.RecordError(r, "render", err)
	}
}

// htmx reports whether r comes from htmx, which never gets a full page:
// live list refreshes get rows, and actions get their notice.
func htmx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// RegisterRoutes serves customer links, Students, Trash, and settings; staff
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
	registerStudentRoutes(mux, roster, biller, staff, page)
	registerAddStudentRoutes(mux, roster, staff, page)
	registerDashboardRoutes(mux, store, roster, biller, staff, page)

	// list renders a tab's registry as a full page, or as just its rows for an
	// htmx refresh. created shows its one-time URL on its row after an
	// ordinary create form.
	list := func(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, data pageData, created *links.Entry) {
		var err error
		if data.Links, err = store.List(ctx); err != nil {
			logging.RecordError(r, "list", err)
			http.Error(w, "Could not load links. Please try again.", http.StatusInternalServerError)
			return
		}
		now := time.Now()
		data.Links = slices.DeleteFunc(data.Links, func(e links.Entry) bool {
			return e.Trashed() != data.Trash || !data.Trash && e.Archived(now)
		})
		if created != nil {
			for i := range data.Links {
				if data.Links[i].ID == created.ID {
					data.Links[i].Token = created.Token
				}
			}
		}
		name := "link-rows"
		if !htmx(r) || status != http.StatusOK {
			name = "index.html"
			data.Staff = staff(r.Context())
			if !data.Trash {
				current, err := config.Current(ctx)
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
		render(w, r, page, status, name, data)
	}

	// acknowledge answers an htmx action with its notice, plus any created
	// link's one-time URL, and has the page refresh its list. Only refreshes
	// write rows, so a slower, older render never overwrites a newer one.
	acknowledge := func(w http.ResponseWriter, r *http.Request, data pageData) {
		w.Header().Set("HX-Trigger", "management-changed")
		render(w, r, page, http.StatusOK, "result", data)
	}

	mux.Handle("GET /management/{$}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var data pageData
		if r.URL.Query().Has("program-start-date-saved") {
			data.ProgramStartNotice = "Fecha de comienzo guardada."
		}
		list(ctx, w, r, http.StatusOK, data, nil)
	}))
	mux.Handle("GET /management/trash", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		list(ctx, w, r, http.StatusOK, pageData{Trash: true}, nil)
	}))
	mux.Handle("POST /management/program-start-date", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		parseErr := r.ParseForm()
		values := r.PostForm["program_start_date"]
		var data pageData
		if len(values) > 0 {
			data.ProgramStartDate = values[0]
		}
		err := settings.ErrProgramStartDate
		if parseErr == nil && len(values) == 1 {
			err = config.SetProgramStartDate(ctx, values[0], staff(r.Context()))
		}
		switch {
		case err == nil:
			http.Redirect(w, r, "/management/?program-start-date-saved", http.StatusSeeOther)
		case errors.Is(err, settings.ErrProgramStartDate):
			data.ProgramStartError = "Ingresa una fecha válida en formato AAAA-MM-DD."
			list(ctx, w, r, http.StatusUnprocessableEntity, data, nil)
		default:
			logging.RecordError(r, "save", err)
			http.Error(w, "Could not save the program start date. Please try again.", http.StatusInternalServerError)
		}
	}))
	mux.Handle("POST /management/links", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current, err := config.Current(ctx)
		var created links.Entry
		if err == nil {
			created, err = store.Create(ctx, staff(r.Context()), time.Now().Add(current.LinkLifetime()))
		}
		if err != nil {
			logging.RecordError(r, "create", err)
			http.Error(w, "Could not generate a link. Please try again.", http.StatusInternalServerError)
			return
		}

		data := pageData{Notice: "Link created."}
		if htmx(r) {
			data.Links = []links.Entry{created}
			acknowledge(w, r, data)
			return
		}
		list(ctx, w, r, http.StatusOK, data, &created)
	}))
	mux.Handle("POST /management/links/{id}/cancel", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		canceled, err := store.Cancel(ctx, r.PathValue("id"), staff(r.Context()))
		if err != nil {
			logging.RecordError(r, "cancel", err)
			http.Error(w, "Could not cancel this link. Please try again.", http.StatusInternalServerError)
			return
		}

		data := pageData{Notice: "Link canceled."}
		if canceled != nil && canceled.Status != links.Canceled {
			data.Notice = "Link can no longer be canceled."
		}
		switch {
		case !htmx(r):
			list(ctx, w, r, http.StatusOK, data, nil)
		case canceled == nil:
			// The ID names no link, so there is nothing to report.
			w.WriteHeader(http.StatusNoContent)
		default:
			acknowledge(w, r, data)
		}
	}))
	mux.Handle("POST /management/links/cancel", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.CancelAll(ctx, staff(r.Context())); err != nil {
			logging.RecordError(r, "cancel_all", err)
			http.Error(w, "Could not cancel links. Please try again.", http.StatusInternalServerError)
			return
		}

		data := pageData{Notice: "All unfinished links canceled."}
		if htmx(r) {
			acknowledge(w, r, data)
			return
		}
		list(ctx, w, r, http.StatusOK, data, nil)
	}))

	// showSettings renders the settings page with the saved values.
	showSettings := func(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, data settingsData) {
		var err error
		if data.Current, err = config.Current(ctx); err != nil {
			logging.RecordError(r, "load", err)
			http.Error(w, "Could not load settings. Please try again.", http.StatusInternalServerError)
			return
		}
		data.Min, data.Max = settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays
		data.Staff = staff(r.Context())
		render(w, r, page, status, "settings.html", data)
	}
	mux.Handle("GET /management/settings", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var data settingsData
		if r.URL.Query().Has("saved") {
			data.Notice = "Saved."
		}
		showSettings(ctx, w, r, http.StatusOK, data)
	}))
	mux.Handle("POST /management/settings", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		// Anything that isn't a whole number is as out of range as 1 or 15.
		err := settings.ErrLinkLifetimeRange
		if days, parseErr := strconv.Atoi(r.PostFormValue("lifetime")); parseErr == nil {
			err = config.SetLinkLifetimeDays(ctx, days, staff(r.Context()))
		}
		switch {
		case err == nil:
			// Redirect so a reload doesn't resubmit the form.
			http.Redirect(w, r, "/management/settings?saved", http.StatusSeeOther)
		case errors.Is(err, settings.ErrLinkLifetimeRange):
			showSettings(ctx, w, r, http.StatusUnprocessableEntity, settingsData{
				Error: fmt.Sprintf("Choose between %d and %d days.", settings.MinLinkLifetimeDays, settings.MaxLinkLifetimeDays),
			})
		default:
			logging.RecordError(r, "save", err)
			http.Error(w, "Could not save settings. Please try again.", http.StatusInternalServerError)
		}
	}))
	return nil
}
