package customer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
)

// agreement shows the terms and records their acceptance against the program start date the
// customer saw, then continues to payments.
func (s *site) agreement(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "agreement", FieldErrors: make(map[string]string)}
	link, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if link.SubmittedAt == nil {
		redirectCustomer(w, r, data.Path+"/forms")
		return
	}
	if r.Method == http.MethodPost && link.TermsAcceptedAt != nil {
		redirectCustomer(w, r, data.Path+"/payments")
		return
	}
	editable, err := s.billing.ContactEditable(ctx, link.ID)
	if err != nil {
		logging.RecordError(r, "load", err)
		data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	data.ContactEditable = editable

	status := http.StatusOK
	data.TermsAcceptedAt, data.ProgramStartDate = link.TermsAcceptedAt, link.ProgramStartDate
	loadAgreementDate := func() bool {
		current, err := s.settings.Current(ctx)
		if err != nil {
			data.ProgramStartDate = ""
			logging.RecordError(r, "load", err)
			data.Error = "No pudimos cargar el acuerdo. Intenta de nuevo."
			s.render(w, r, "index.html", data, http.StatusInternalServerError)
			return false
		}
		data.ProgramStartDate = current.ProgramStartDate
		return true
	}
	if data.TermsAcceptedAt == nil && !loadAgreementDate() {
		return
	}
	if r.Method == http.MethodPost {
		status = parseCustomerForm(w, r)
		if status != http.StatusOK {
			data.Error = "No pudimos leer tu selección. Intenta de nuevo."
		} else {
			switch {
			case len(r.PostForm["accept_terms"]) > 1 || len(r.PostForm["program_start_date"]) != 1:
				status = http.StatusBadRequest
				data.Error = "No pudimos leer tu aceptación. Intenta de nuevo."
			case r.PostForm.Get("accept_terms") != "yes":
				status = http.StatusUnprocessableEntity
				data.FieldErrors["accept_terms"] = "Debes aceptar los términos para continuar."
			default:
				accepted, err := s.links.AcceptTerms(ctx, token, readSessionCookie(r), r.PostForm.Get("program_start_date"))
				switch {
				case errors.Is(err, links.ErrProgramStartChanged):
					if !loadAgreementDate() {
						return
					}
					status = http.StatusUnprocessableEntity
					data.Error = "La fecha de inicio cambió. Revisa el acuerdo y acepta los términos de nuevo."
				case err != nil:
					logging.RecordError(r, "save", err)
					data.Error = "No pudimos guardar tu aceptación. Intenta de nuevo."
					status = http.StatusInternalServerError
				case accepted == nil:
					s.render(w, r, "index.html", data, http.StatusNotFound)
					return
				default:
					redirectCustomer(w, r, data.Path+"/payments")
					return
				}
			}
		}
	}

	date, err := time.Parse(time.DateOnly, data.ProgramStartDate)
	if err != nil || date.Year() == 0 {
		data.ProgramStartDate = ""
		if status != http.StatusInternalServerError {
			data.Error = "El acuerdo no está disponible porque falta confirmar la fecha de inicio. Comunícate con el equipo."
		}
	} else {
		data.ProgramStartLabel = payments.SpanishDate(date)
	}
	s.render(w, r, "index.html", data, status)
}
