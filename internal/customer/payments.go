package customer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
)

type planView struct {
	payments.Plan
	Schedule []payments.Installment
}

// payments serves plan selection and starts the hosted first payment.
// Link usability, contact and consent gate every request before billing is touched;
// only POST can create or resume a checkout.
func (s *site) payments(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "payments", FieldErrors: make(map[string]string)}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	link, err := s.links.Lookup(ctx, token)
	if !s.found(w, r, "index.html", &data, link, err) {
		return
	}
	if link.SubmittedAt == nil {
		redirectCustomer(w, r, data.Path+"/forms")
		return
	}
	if link.TermsAcceptedAt == nil {
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	}

	status := http.StatusOK
	posted := false
	if r.Method == http.MethodPost {
		status = parseCustomerForm(w, r)
		if status != http.StatusOK {
			data.Error = "No pudimos leer tu selección. Intenta de nuevo."
		} else {
			data.PaymentPlan, posted = r.PostForm.Get("payment_plan"), true
			_, allowed := payments.Find(data.PaymentPlan)
			switch {
			case len(r.PostForm["payment_plan"]) > 1:
				status = http.StatusBadRequest
				data.Error = "Selecciona una sola opción de pago."
			case len(r.PostForm["payment_plan"]) != 1 || !allowed:
				status = http.StatusUnprocessableEntity
				data.FieldErrors["payment_plan"] = "Selecciona una de las tres opciones de pago."
			default:
				// StartCheckout bounds its own provider work; the request context only carries disconnects.
				result, err := s.billing.StartCheckout(r.Context(), link.TokenHash, data.PaymentPlan)
				switch {
				case errors.Is(err, billing.ErrUnavailable):
					s.render(w, r, "index.html", data, http.StatusNotFound)
					return
				case errors.Is(err, billing.ErrPlanLocked):
					status = http.StatusConflict
					data.Error = "Tu plan de pago ya está confirmado y no se puede cambiar."
				case err != nil:
					logging.RecordError(r, "checkout", err)
					status = http.StatusInternalServerError
					data.Error = "No pudimos iniciar el pago seguro. Intenta de nuevo."
				case result.URL != "":
					redirectCustomer(w, r, result.URL)
					return
				case result.Funded:
					redirectCustomer(w, r, data.Path+"/payments")
					return
				default:
					status = http.StatusAccepted
					data.Processing = true
				}
			}
		}
	}

	summaryCtx, cancelSummary := context.WithTimeout(r.Context(), dbTimeout)
	defer cancelSummary()
	summary, err := s.billing.ForLink(summaryCtx, link.TokenHash)
	switch {
	case errors.Is(err, billing.ErrUnavailable):
		s.render(w, r, "index.html", data, http.StatusNotFound)
		return
	case err != nil:
		logging.RecordError(r, "load", err)
		status = http.StatusInternalServerError
		data.Error = "No pudimos cargar tu información de pago. Intenta de nuevo."
	default:
		data.Billing = summary
		if !posted && summary != nil {
			data.PaymentPlan = summary.PlanID
		}
	}
	// A funded contract shows its persisted plan instead of a way to pay again.
	if data.Billing == nil || !data.Billing.Funded {
		// A processing checkout may still change the plan, so no plan is announced yet.
		if !data.Processing && summary != nil && summary.PlanName != "" {
			data.SelectedPlan = summary.PlanName
		}
		now := time.Now()
		for _, plan := range payments.Plans() {
			data.Plans = append(data.Plans, planView{Plan: plan, Schedule: plan.Schedule(now)})
		}
	}
	s.render(w, r, "index.html", data, status)
}

// maxReturnPolls caps the return page's polling: 80 polls at 1.5s is about two minutes.
const maxReturnPolls = 80

// paymentReturn shows the outcome of a hosted Checkout redirect. It is token-free: it reads one
// persisted attempt and never exposes student identity, contact, or payment history. Visiting it
// (or a cancelled return) never changes financial state. While the payment is pending, HTMX polls
// this route and swaps in only the status block; a failed poll swaps in a retry block that keeps
// polling until maxReturnPolls, and an unknown session reloads the page.
func (s *site) paymentReturn(w http.ResponseWriter, r *http.Request) {
	data := pageData{View: "payment-return"}
	name := "index.html"
	if htmx(r) {
		name = "payment-return-status"
	}
	query := r.URL.Query()
	polls, _ := strconv.Atoi(query.Get("polls")) // malformed input reads as 0
	polls = min(max(polls, 0), maxReturnPolls)
	data.ReturnSessionID, data.ReturnPollsNext, data.ReturnStopPoll = query.Get("session_id"), polls+1, polls >= maxReturnPolls
	status := http.StatusOK
	switch {
	case data.ReturnSessionID != "":
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		defer cancel()
		result, err := s.billing.ReturnStatus(ctx, data.ReturnSessionID)
		switch {
		case errors.Is(err, billing.ErrUnavailable):
			status = http.StatusNotFound
		case err != nil:
			logging.RecordError(r, "load", err)
			data.Error = "No pudimos confirmar el estado de tu pago. Intenta de nuevo."
			status = http.StatusInternalServerError
		default:
			data.Return = &result
		}
	case query.Get("cancelled") == "1":
		data.Return = &billing.ReturnStatus{Status: "not_completed"}
	default:
		status = http.StatusNotFound
	}
	s.render(w, r, name, data, status)
}
