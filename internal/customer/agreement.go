package customer

import (
	"context"
	"net/http"
	"net/url"

	"projecthermes/internal/logging"
	"projecthermes/internal/settings"
)

// agreement shows the terms and records their acceptance against the program start date the
// customer saw, then continues to payments. A POST validates and writes first; the page's reads
// run only for a GET, or to answer a POST that accepted nothing.
func (s *site) agreement(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "agreement", FieldErrors: make(map[string]string)}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = parseCustomerForm(w, r)
		switch {
		case status != http.StatusOK:
			data.Error = selectionFailedMessage
		case len(r.PostForm["accept_terms"]) > 1 || len(r.PostForm["program_start_date"]) != 1:
			status = http.StatusBadRequest
			data.Error = "No pudimos leer tu aceptación. Intenta de nuevo."
		case r.PostForm.Get("accept_terms") != "yes":
			status = http.StatusUnprocessableEntity
			data.FieldErrors["accept_terms"] = "Debes aceptar los términos para continuar."
		default:
			ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
			accepted, err := s.links.AcceptTerms(ctx, token, readSessionCookie(r), r.PostForm.Get("program_start_date"))
			cancel()
			switch {
			case accepted:
				redirectCustomer(w, r, data.Path+"/payments")
				return
			case err != nil:
				logging.RecordError(r, "accept", err)
				status = http.StatusInternalServerError
				data.Error = "No pudimos guardar tu aceptación. Intenta de nuevo."
			default:
				// No row took the acceptance. The lookup below tells an unusable session and a replay from a changed date.
				status = http.StatusUnprocessableEntity
				data.Error = "La fecha de inicio cambió. Revisa el acuerdo y acepta los términos de nuevo."
			}
		}
	}

	enrollment, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	if enrollment.SubmittedAt == nil {
		redirectCustomer(w, r, data.Path+"/forms")
		return
	}
	if r.Method == http.MethodPost && enrollment.TermsAcceptedAt != nil {
		// A replay, or another tab accepted first: the first acceptance stands for any body.
		redirectCustomer(w, r, data.Path+"/payments")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	editable, err := s.billing.ContactEditable(ctx, enrollment)
	if err != nil {
		logging.RecordError(r, "load", err)
		data.Error = openFailedMessage
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	data.ContactEditable = editable
	// An accepted agreement shows its own snapshot; an open one shows the current date.
	data.TermsAcceptedAt, data.ProgramStartDate = enrollment.TermsAcceptedAt, enrollment.ProgramStartDate
	if data.TermsAcceptedAt == nil {
		current, err := s.settings.Current(ctx)
		if err != nil {
			logging.RecordError(r, "load", err)
			data.Error = "No pudimos cargar el acuerdo. Intenta de nuevo."
			status = http.StatusInternalServerError
		}
		data.ProgramStartDate = current.ProgramStartDate
	}
	if date, err := settings.ParseDate(data.ProgramStartDate); err == nil {
		data.ProgramStartLabel = spanishDate(date)
	} else {
		data.ProgramStartDate = ""
		if status != http.StatusInternalServerError {
			data.Error = "El acuerdo no está disponible porque falta confirmar la fecha de inicio. Comunícate con el equipo."
		}
	}
	s.render(w, r, "index.html", data, status)
}
