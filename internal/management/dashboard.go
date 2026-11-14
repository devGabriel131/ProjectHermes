package management

import (
	"context"
	"html/template"
	"net/http"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/students"
)

// dashboardWindow is how far the dashboard looks back at enrollment and ahead at
// installments coming due. Installments are monthly, so a month shows the next collection.
const dashboardWindow = 30 * 24 * time.Hour

type dashboardData struct {
	Staff              string
	Days               int // The window, in days, for headings.
	Students           students.Counts
	LinksSent          int // Reusable links created within the window, whatever became of them.
	EnrollmentsStarted int // Customer enrollment sessions started within the window.
	FormsCompleted     int // Enrollments whose customer submitted the form within the window, paid or not.
	ActiveLinks        int // Reusable links still open to new enrollments right now.
	LinksWaiting       int // Enrollments in progress right now.
	Billing            billing.Overview
}

// NeedsAttention reports whether anything on the dashboard calls for staff action.
func (d dashboardData) NeedsAttention() bool {
	return d.Students.Suspended+d.Billing.OverdueInstallments+d.Billing.FailedInstallments+
		d.Billing.ContractsInReview+d.Billing.PaymentsInReview > 0
}

// plural picks the noun form that agrees with n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func registerDashboardRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, biller *billing.Service, staff func(context.Context) string, page *template.Template) {
	mux.Handle("GET /management/dashboard", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		now := time.Now().UTC()
		since := now.Add(-dashboardWindow)
		data := dashboardData{Staff: staff(r.Context()), Days: int(dashboardWindow / (24 * time.Hour))}
		groups, err := store.ListGroups(ctx)
		if err == nil {
			data.Students, err = roster.Count(ctx)
		}
		if err == nil {
			data.Billing, err = biller.Overview(ctx, now, dashboardWindow)
		}
		if err != nil {
			logging.RecordError(r, "load", err)
			http.Error(w, "Could not load the dashboard. Please try again.", http.StatusInternalServerError)
			return
		}
		for _, g := range groups {
			if !g.Link.CreatedAt.Before(since) {
				data.LinksSent++
			}
			if g.Link.Status == links.LinkActive {
				data.ActiveLinks++
			}
			for _, entry := range g.Enrollments {
				if entry.Status == links.InProgress {
					data.LinksWaiting++
				}
				if !entry.CreatedAt.Before(since) {
					data.EnrollmentsStarted++
				}
				if entry.SubmittedAt != nil && !entry.SubmittedAt.Before(since) {
					data.FormsCompleted++
				}
			}
		}
		name := "dashboard.html"
		if htmx(r) {
			name = "dashboard-content" // A live refresh of just the figures.
		}
		render(w, r, page, http.StatusOK, name, data)
	}))
}
