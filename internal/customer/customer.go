// Package customer serves the private /customer/{token} pages: contact form with autosave, agreement, plan choice, Stripe Checkout start and the token-free payment return page. It holds handlers only; every write goes through the links store or billing. See docs/domain/customer-flow.md.
package customer

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
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

type pageData struct {
	students.Details
	Path              string
	View              string
	Error             string
	EmailConfirm      string
	Pin               string
	EmailMismatch     bool
	FieldErrors       map[string]string
	ProgramStartDate  string
	ProgramStartLabel string
	TermsAcceptedAt   *time.Time
	PaymentPlan       string
	SelectedPlan      string
	Plans             []planView
	// Billing is the contract summary; Processing marks a checkout the provider has not finished.
	Billing    *billing.Summary
	Processing bool
	// Return is the token-free outcome shown after Stripe Checkout.
	Return *billing.ReturnStatus
	// ReturnSessionID and ReturnPollsNext rebuild the payment-return poll URL;
	// ReturnStopPoll ends polling after maxReturnPolls requests.
	ReturnSessionID string
	ReturnPollsNext int
	ReturnStopPoll  bool
	// Editing marks the contact form reopened after submit.
	Editing         bool
	ContactEditable bool
	// Welcome state flags derived from the browser's enrollment session cookie.
	HasEnrollment bool
	Completed     bool
	CanStartNew   bool
}

// site is the customer-facing handler set: one template set over the stores the pages read and write.
type site struct {
	page          *template.Template
	links         *links.Store
	roster        *students.Store
	settings      *settings.Store
	billing       *billing.Service
	secureCookies bool
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, configuration *settings.Store, biller *billing.Service, secureCookies bool) error {
	page, err := template.New("index.html").Funcs(template.FuncMap{
		"municipalities": students.MunicipalityOptions,
		"trim":           strings.TrimSpace,
		"money":          payments.Money,
		"spanishDate":    payments.SpanishDate,
		"billingDate":    func(t time.Time) string { return payments.SpanishDate(t.In(payments.PuertoRico)) },
	}).ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}

	s := &site{page: page, links: store, roster: roster, settings: configuration, billing: biller, secureCookies: secureCookies}
	mux.Handle("GET /customer/{token}", withHeaders(s.welcome))
	mux.Handle("POST /customer/{token}/start", withHeaders(s.start))
	mux.Handle("GET /customer/{token}/forms", withHeaders(s.forms))
	mux.Handle("POST /customer/{token}", withHeaders(s.submit))
	mux.Handle("POST /customer/{token}/draft/{field}", withHeaders(s.draft))
	mux.Handle("POST /customer/{token}/pin", withHeaders(s.checkPin))
	mux.Handle("GET /customer/{token}/edit", withHeaders(s.edit))
	mux.Handle("POST /customer/{token}/edit", withHeaders(s.edit))
	mux.Handle("GET /customer/{token}/agreement", withHeaders(s.agreement))
	mux.Handle("POST /customer/{token}/agreement", withHeaders(s.agreement))
	mux.Handle("GET /customer/{token}/payments", withHeaders(s.payments))
	mux.Handle("POST /customer/{token}/payments", withHeaders(s.payments))
	mux.Handle("GET /payments/return", withHeaders(s.paymentReturn))
	return nil
}

const sessionCookieName = "enrollment_session"

func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func setSessionCookie(w http.ResponseWriter, linkToken, sessionToken string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/customer/" + url.PathEscape(linkToken),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   30 * 24 * 60 * 60,
	})
}

func htmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// withHeaders sets the customer response headers before h runs, so every outcome carries them.
func withHeaders(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Vary", "HX-Request")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h(w, r)
	})
}

// render writes the named template, or the branded 404 that makes HTMX reload the page.
func (s *site) render(w http.ResponseWriter, r *http.Request, name string, data pageData, status int) {
	if status == http.StatusNotFound {
		name = "not-found.html"
		if htmx(r) {
			w.Header().Set("HX-Refresh", "true")
		}
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if err := s.page.ExecuteTemplate(w, name, data); err != nil {
		logging.RecordError(r, "render", err)
	}
}

// resolveSession resolves the customer's enrollment session for inner routes.
// Missing/unusable sessions redirect GET/HEAD to the welcome page when the link is still usable, and render 404 otherwise.
func (s *site) resolveSession(w http.ResponseWriter, r *http.Request, name string, data *pageData, token string) (*links.Enrollment, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	sess, err := s.links.LookupSession(ctx, token, readSessionCookie(r))
	switch {
	case err != nil:
		logging.RecordError(r, "lookup", err)
		data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
		s.render(w, r, name, *data, http.StatusInternalServerError)
		return nil, false
	case sess == nil:
		s.render(w, r, name, *data, http.StatusNotFound)
		return nil, false
	case sess.Enrollment == nil || !sess.Usable:
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && sess.LinkUsable {
			redirectCustomer(w, r, "/customer/"+url.PathEscape(token))
			return nil, false
		}
		s.render(w, r, name, *data, http.StatusNotFound)
		return nil, false
	default:
		return sess.Enrollment, true
	}
}

// Redirects carry no contact data, including the HTMX submission response.
func redirectCustomer(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method == http.MethodPost && htmx(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}

// welcome serves the reusable link landing page without creating database rows.
func (s *site) welcome(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "welcome"}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	sess, err := s.links.LookupSession(ctx, token, readSessionCookie(r))
	if err != nil {
		logging.RecordError(r, "lookup", err)
		data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	if sess == nil {
		s.render(w, r, "index.html", data, http.StatusNotFound)
		return
	}
	if sess.Enrollment != nil {
		switch sess.EnrollmentStatus {
		case links.AwaitingPayment:
			if sess.Usable {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				redirectCustomer(w, r, data.Path+"/agreement")
				return
			}
		case links.Finished:
			data.Completed = true
			data.CanStartNew = sess.LinkUsable
			s.render(w, r, "index.html", data, http.StatusOK)
			return
		case links.InProgress:
			if sess.Usable {
				data.HasEnrollment = true
				s.render(w, r, "index.html", data, http.StatusOK)
				return
			}
		}
	}
	if !sess.LinkUsable {
		s.render(w, r, "index.html", data, http.StatusNotFound)
		return
	}
	s.render(w, r, "index.html", data, http.StatusOK)
}

// start begins or resumes a browser's enrollment session on a reusable link and sets its cookie.
func (s *site) start(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	path := "/customer/" + url.PathEscape(token)
	data := pageData{Path: path, View: "welcome"}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	enrollment, newCookie, err := s.links.StartEnrollment(ctx, token, readSessionCookie(r))
	if err != nil {
		logging.RecordError(r, "start", err)
		data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	if enrollment == nil {
		s.render(w, r, "index.html", data, http.StatusNotFound)
		return
	}
	if newCookie != "" {
		setSessionCookie(w, token, newCookie, s.secureCookies)
	}
	if enrollment.SubmittedAt != nil {
		redirectCustomer(w, r, path+"/agreement")
		return
	}
	redirectCustomer(w, r, path+"/forms")
}

// forms serves the contact form page for an active enrollment session.
func (s *site) forms(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "form"}
	link, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	if link.SubmittedAt != nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	}
	data.Details = link.Details
	s.render(w, r, "index.html", data, http.StatusOK)
}

// submit commits the final contact form and PIN and advances to the agreement. Valid input is written with
// no prior lookup, so a database failure is recorded as a save failure; anything else observes
// link usability and completion first. The HTMX request swaps in only the content section.
func (s *site) submit(w http.ResponseWriter, r *http.Request) {
	token, cookieToken := r.PathValue("token"), readSessionCookie(r)
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "form"}
	name := "index.html"
	if htmx(r) {
		name = "customer-content"
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	status := parseCustomerForm(w, r)
	if status != http.StatusOK {
		data.Error = "No pudimos leer tu información. Intenta de nuevo."
	} else if parsed, pin, ok := readSubmission(r.PostForm, &data); !ok {
		status = http.StatusUnprocessableEntity
	} else if allowed, err := s.links.SpendPinCheck(ctx, token, cookieToken); err != nil {
		logging.RecordError(r, "save", err)
		data.Error = "No pudimos guardar tu información. Intenta de nuevo."
		s.render(w, r, name, data, http.StatusInternalServerError)
		return
	} else if !allowed {
		// Out of PIN answers, or no usable open enrollment: the lookup below tells which.
		data.FieldErrors["pin"] = pinLimitMessage
		status = http.StatusTooManyRequests
	} else {
		result, err := s.links.Submit(ctx, token, cookieToken, parsed, pin)
		switch {
		case err != nil:
			logging.RecordError(r, "save", err)
			data.Error = "No pudimos guardar tu información. Intenta de nuevo."
			s.render(w, r, name, data, http.StatusInternalServerError)
			return
		case result == links.Saved, result == links.AlreadySubmitted:
			redirectCustomer(w, r, data.Path+"/agreement")
			return
		case result == links.PinTaken:
			// Another submitted enrollment or enrolled student holds the PIN: redraw below with what was typed.
			data.FieldErrors["pin"] = pinTakenMessage
			status = http.StatusUnprocessableEntity
		default:
			s.render(w, r, name, data, http.StatusNotFound)
			return
		}
	}

	link, ok := s.resolveSession(w, r, name, &data, token)
	if !ok {
		return
	}
	if link.SubmittedAt != nil {
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	}
	// A 422 keeps the attempted values; any other rejection shows what is stored.
	if status != http.StatusUnprocessableEntity {
		data.Details = link.Details
	}
	s.render(w, r, name, data, status)
}

// draft autosaves one field on blur. It answers only with status so it never redraws fields in use.
// An unknown field is refused before the body is read or the database is touched.
func (s *site) draft(w http.ResponseWriter, r *http.Request) {
	token, key, cookieToken := r.PathValue("token"), r.PathValue("field"), readSessionCookie(r)
	data := pageData{View: "save-status"}
	field, known := students.FindField(key)
	if !known {
		s.render(w, r, "save-status", data, http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	status := parseCustomerForm(w, r)
	if status == http.StatusOK && len(r.PostForm[key]) != 1 {
		status = http.StatusBadRequest
	}
	if status != http.StatusOK {
		data.Error = "No pudimos leer tu información. Intenta de nuevo."
	} else if value, err := field.Parse(r.PostForm.Get(key), false); err != nil {
		data.FieldErrors = map[string]string{key: fieldError(key)}
		status = http.StatusUnprocessableEntity
	} else {
		result, err := s.links.SaveDraft(ctx, token, cookieToken, key, value)
		switch {
		case err != nil:
			logging.RecordError(r, "save", err)
			data.Error = "No pudimos guardar tu información. Intenta de nuevo."
			s.render(w, r, "save-status", data, http.StatusInternalServerError)
		case result == links.Saved:
			s.render(w, r, "save-status", data, http.StatusOK)
		case result == links.AlreadySubmitted:
			locked(w, r)
		default:
			s.render(w, r, "save-status", data, http.StatusNotFound)
		}
		return
	}

	link, ok := s.resolveSession(w, r, "save-status", &data, token)
	if !ok {
		return
	}
	if link.SubmittedAt != nil {
		locked(w, r)
		return
	}
	s.render(w, r, "save-status", data, status)
}

// locked answers an autosave against a submitted form: the page is stale, so HTMX reloads it.
func locked(w http.ResponseWriter, r *http.Request) {
	if htmx(r) {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusConflict)
}

// edit reopens the contact form of a submitted link until payment is confirmed or in flight.
// The edit has no autosave: one POST updates the link's contact snapshot, then returns to the agreement.
// Submission time, expiry and accepted terms never change.
func (s *site) edit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "form", Editing: true}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	link, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	if link.SubmittedAt == nil {
		redirectCustomer(w, r, data.Path+"/forms")
		return
	}
	editable, err := s.billing.ContactEditable(ctx, link.ID)
	if err != nil {
		logging.RecordError(r, "load", err)
		data.Error = "No pudimos abrir este enlace. Intenta de nuevo."
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	if !editable {
		redirectCustomer(w, r, data.Path+"/payments")
		return
	}
	status := http.StatusOK
	data.Details = link.Details
	if r.Method == http.MethodPost {
		if status = parseCustomerForm(w, r); status != http.StatusOK {
			data.Error = "No pudimos leer tu información. Intenta de nuevo."
		} else if parsed, ok := readContact(r.PostForm, &data); !ok {
			status = http.StatusUnprocessableEntity
		} else if parsed == link.Details {
			// Nothing changed: skip the write, the management refresh and the provider sync.
			redirectCustomer(w, r, data.Path+"/agreement")
			return
		} else {
			err = s.billing.UpdateContact(ctx, link.ID, parsed)
			switch {
			case errors.Is(err, billing.ErrContactLocked):
				redirectCustomer(w, r, data.Path+"/payments")
				return
			case errors.Is(err, billing.ErrUnavailable):
				s.render(w, r, "index.html", data, http.StatusNotFound)
				return
			case err != nil:
				logging.RecordError(r, "save", err)
				data.Error = "No pudimos guardar tu información. Intenta de nuevo."
				status = http.StatusInternalServerError
			default:
				redirectCustomer(w, r, data.Path+"/agreement")
				return
			}
		}
	}
	s.render(w, r, "index.html", data, status)
}

func parseCustomerForm(w http.ResponseWriter, r *http.Request) int {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusBadRequest
	}
	return http.StatusOK
}

// readContact records the attempted contact form on data, so a rejection redraws what was typed,
// and reports the validated details and whether every field passed.
func readContact(form url.Values, data *pageData) (parsed students.Details, ok bool) {
	data.Details = students.Details{
		Name: form.Get("name"), Email: form.Get("email"),
		Phone: form.Get("phone"), Municipality: form.Get("municipality"),
		DateOfBirth: form.Get("date_of_birth"), Gender: form.Get("gender"),
	}
	data.EmailConfirm = form.Get("email_confirm")
	parsed, data.FieldErrors, data.EmailMismatch = parseContact(form)
	return parsed, len(data.FieldErrors) == 0
}

// readSubmission is readContact plus the PIN, which only the first submit takes: it is never
// drafted and the edit form has no PIN.
func readSubmission(form url.Values, data *pageData) (parsed students.Details, pin string, ok bool) {
	parsed, ok = readContact(form, data)
	data.Pin = form.Get("pin")
	pin, err := students.ParsePin(data.Pin, true)
	if err != nil {
		data.FieldErrors["pin"] = fieldError("pin")
		ok = false
	}
	return parsed, pin, ok
}

// checkPin tells the form whether a PIN is free before the final submit. It saves nothing:
// it checks whether the PIN is held by a student or a submitted link, and the unique index decides at submit. It is a POST
// so the PIN stays out of URLs and access logs.
func (s *site) checkPin(w http.ResponseWriter, r *http.Request) {
	token, cookieToken := r.PathValue("token"), readSessionCookie(r)
	data := pageData{View: "pin-status"}
	link, ok := s.resolveSession(w, r, "pin-status", &data, token)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if link.SubmittedAt != nil {
		locked(w, r)
		return
	}
	status := parseCustomerForm(w, r)
	if status == http.StatusOK && len(r.PostForm["pin"]) != 1 {
		status = http.StatusBadRequest
	}
	if status != http.StatusOK {
		data.Error = "No pudimos leer tu información. Intenta de nuevo."
		s.render(w, r, "pin-status", data, status)
		return
	}
	pin, err := students.ParsePin(r.PostForm.Get("pin"), true)
	if err != nil {
		data.FieldErrors = map[string]string{"pin": fieldError("pin")}
		s.render(w, r, "pin-status", data, http.StatusUnprocessableEntity)
		return
	}
	allowed, err := s.links.SpendPinCheck(ctx, token, cookieToken)
	if err != nil {
		logging.RecordError(r, "lookup", err)
		data.Error = "No pudimos verificar el PIN. Intenta de nuevo."
		s.render(w, r, "pin-status", data, http.StatusInternalServerError)
		return
	}
	if !allowed {
		data.Error = pinLimitMessage
		s.render(w, r, "pin-status", data, http.StatusTooManyRequests)
		return
	}
	taken, err := s.roster.PinTaken(ctx, pin)
	switch {
	case err != nil:
		logging.RecordError(r, "lookup", err)
		data.Error = "No pudimos verificar el PIN. Intenta de nuevo."
		status = http.StatusInternalServerError
	case taken:
		data.FieldErrors = map[string]string{"pin": pinTakenMessage}
		status = http.StatusUnprocessableEntity
	}
	s.render(w, r, "pin-status", data, status)
}

// parseContact validates every contact field and the email confirmation, collecting all errors.
func parseContact(form url.Values) (parsed students.Details, fieldErrors map[string]string, emailMismatch bool) {
	fieldErrors = make(map[string]string)
	for _, field := range students.Fields() {
		value, err := field.Parse(form.Get(field.Key), true)
		if err != nil {
			fieldErrors[field.Key] = fieldError(field.Key)
		}
		switch field.Key {
		case "name":
			parsed.Name = value
		case "email":
			parsed.Email = value
		case "phone":
			parsed.Phone = value
		case "municipality":
			parsed.Municipality = value
		case "date_of_birth":
			parsed.DateOfBirth = value
		case "gender":
			parsed.Gender = value
		}
	}
	confirmation, err := students.ParseEmail(form.Get("email_confirm"), true)
	if err != nil {
		fieldErrors["email_confirm"] = fieldError("email")
	} else if fieldErrors["email"] == "" && confirmation != parsed.Email {
		emailMismatch = true
		fieldErrors["email_confirm"] = "Los correos electrónicos no coinciden."
	}
	return parsed, fieldErrors, emailMismatch
}

func fieldError(key string) string {
	switch key {
	case "name":
		return "Escribe el nombre y los apellidos del estudiante usando solo letras y espacios."
	case "email":
		return "Escribe un correo electrónico válido."
	case "phone":
		return "Escribe un número válido de Puerto Rico o Estados Unidos, con código de área."
	case "municipality":
		return "Selecciona un municipio o “Fuera de Puerto Rico”."
	case "date_of_birth":
		return "Escribe una fecha de nacimiento válida."
	case "gender":
		return "Selecciona masculino o femenino."
	case "pin":
		return "Escribe un PIN de 4 a 6 dígitos."
	}
	return ""
}

const pinTakenMessage = "Este PIN ya está en uso. Elige otro."
const pinLimitMessage = "Este enlace alcanzó el límite de intentos de PIN. Comunícate con el equipo."
