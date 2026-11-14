package management

import (
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
	LinksCreated       int // Reusable links created within the window, whatever became of them.
	EnrollmentsStarted int // Customer enrollment sessions started within the window.
	FormsCompleted     int // Enrollments whose customer submitted the form within the window, paid or not.
	ActiveLinks        int // Reusable links still open to new enrollments right now.
	LinksInProgress    int // Enrollments in progress right now.
	Billing            billing.Overview
}

// NeedsAttention reports whether anything on the dashboard calls for staff action.
func (d dashboardData) NeedsAttention() bool {
	return d.Students.Suspended+d.Billing.OverdueInstallments+d.Billing.FailedInstallments+
		d.Billing.ContractsInReview+d.Billing.PaymentsInReview > 0
}

func (s *site) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	since := now.Add(-dashboardWindow)
	data := dashboardData{Staff: s.staff(ctx), Days: int(dashboardWindow / (24 * time.Hour))}
	groups, err := s.links.ListGroups(ctx)
	if err == nil {
		data.Students, err = s.roster.Count(ctx)
	}
	if err == nil {
		data.Billing, err = s.billing.Overview(ctx, now, dashboardWindow)
	}
	if err != nil {
		logging.RecordError(r, "load", err)
		http.Error(w, "Could not load the dashboard. Please try again.", http.StatusInternalServerError)
		return
	}
	for _, g := range groups {
		if !g.Link.CreatedAt.Before(since) {
			data.LinksCreated++
		}
		if g.Link.Status == links.LinkActive {
			data.ActiveLinks++
		}
		for _, entry := range g.Enrollments {
			if entry.Status == links.InProgress {
				data.LinksInProgress++
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
	s.render(w, r, http.StatusOK, name, data)
}
