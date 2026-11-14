// Package customer serves the private /customer/{token} pages: contact form with autosave, agreement, plan choice, Stripe Checkout start and the token-free payment return page. It holds handlers only; every write goes through the links store or billing. See docs/domain/customer-flow.md.
package customer

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/logging"
	"projecthermes/internal/payments"
	"projecthermes/internal/puertorico"
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
	DobDay            string
	DobMonth          string
	DobYear           string
	Error             string
	EmailConfirm      string
	Pin               string
	EmailMismatch     bool
	FieldErrors       map[string]string
	ProgramStartDate  string
	ProgramStartLabel string
	TermsAcceptedAt   *time.Time
	PaymentPlan       string
	Plans             []planView
	// Billing is the contract summary; Processing marks a checkout the provider has not finished.
	Billing    *billing.Summary
	Processing bool
	// Return is the token-free outcome shown after Stripe Checkout.
	Return *billing.ReturnStatus
	// ReturnPollURL is the payment-return page's next poll; empty ends polling.
	ReturnPollURL string
	// Editing marks the contact form reopened after submit.
	Editing         bool
	ContactEditable bool
	// Welcome state flags derived from the browser's enrollment session cookie.
	HasEnrollment bool
	Completed     bool
	CanStartNew   bool
}

// fill shows stored contact details, with the birth date split for its three controls.
func (data *pageData) fill(details students.Details) {
	data.Details = details
	data.DobDay, data.DobMonth, data.DobYear = splitDateOfBirth(details.DateOfBirth)
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

var spanishMonths = [...]string{
	"enero", "febrero", "marzo", "abril", "mayo", "junio",
	"julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre",
}

// spanishDate formats the calendar date in its existing location.
func spanishDate(date time.Time) string {
	year, month, day := date.Date()
	return fmt.Sprintf("%d de %s de %d", day, spanishMonths[month-1], year)
}

func RegisterRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, configuration *settings.Store, biller *billing.Service, secureCookies bool) error {
	page, err := template.New("index.html").Funcs(template.FuncMap{
		"municipalities": students.MunicipalityOptions,
		"trim":           strings.TrimSpace,
		"money":          payments.Money,
		"spanishDate":    spanishDate,
		"billingDate":    func(t time.Time) string { return spanishDate(t.In(puertorico.Zone)) },
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

// resolveSession resolves the customer's usable enrollment session for inner routes.
// Without one, GET/HEAD redirect to the welcome page while the link takes new enrollments; anything else renders 404.
func (s *site) resolveSession(w http.ResponseWriter, r *http.Request, name string, data *pageData, token string) (*links.Enrollment, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	enrollment, canStart, err := s.links.LookupSession(ctx, token, readSessionCookie(r))
	switch {
	case err != nil:
		logging.RecordError(r, "lookup", err)
		data.Error = openFailedMessage
		s.render(w, r, name, *data, http.StatusInternalServerError)
		return nil, false
	case enrollment != nil:
		return enrollment, true
	case canStart && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		redirectCustomer(w, r, "/customer/"+url.PathEscape(token))
		return nil, false
	default:
		s.render(w, r, name, *data, http.StatusNotFound)
		return nil, false
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
	enrollment, canStart, err := s.links.LookupSession(ctx, token, readSessionCookie(r))
	status := http.StatusOK
	switch {
	case err != nil:
		logging.RecordError(r, "lookup", err)
		data.Error = openFailedMessage
		status = http.StatusInternalServerError
	case enrollment == nil:
		if !canStart {
			status = http.StatusNotFound
		}
	case enrollment.PaidAt != nil:
		data.Completed, data.CanStartNew = true, canStart
	case enrollment.SubmittedAt != nil:
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	default:
		data.HasEnrollment = true
	}
	s.render(w, r, "index.html", data, status)
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
		data.Error = openFailedMessage
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
	enrollment, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	if enrollment.SubmittedAt != nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	}
	data.fill(enrollment.Details)
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
		data.Error = readFailedMessage
	} else if parsed, pin, ok := readSubmission(r.PostForm, &data); !ok {
		status = http.StatusUnprocessableEntity
	} else {
		result, err := s.links.Submit(ctx, token, cookieToken, parsed, pin)
		switch {
		case err != nil:
			logging.RecordError(r, "save", err)
			data.Error = saveFailedMessage
			s.render(w, r, name, data, http.StatusInternalServerError)
			return
		case result == links.Saved, result == links.AlreadySubmitted:
			redirectCustomer(w, r, data.Path+"/agreement")
			return
		case result == links.PinTaken:
			// Another submitted enrollment or enrolled student holds the PIN: redraw below with what was typed.
			data.FieldErrors["pin"] = pinTakenMessage
			status = http.StatusUnprocessableEntity
		case result == links.PinLimited:
			data.FieldErrors["pin"] = pinLimitMessage
			status = http.StatusTooManyRequests
		default:
			s.render(w, r, name, data, http.StatusNotFound)
			return
		}
	}

	enrollment, ok := s.resolveSession(w, r, name, &data, token)
	if !ok {
		return
	}
	if enrollment.SubmittedAt != nil {
		redirectCustomer(w, r, data.Path+"/agreement")
		return
	}
	// A 422 keeps the attempted values; any other rejection shows what is stored.
	if status != http.StatusUnprocessableEntity {
		data.fill(enrollment.Details)
	}
	s.render(w, r, name, data, status)
}

// draft autosaves one field on change and answers only with status, so it never redraws fields in use.
// An unknown field is refused before the body is read or the database is touched. Rejected input still asks
// the store whether the form is open, so 404 and 409 precede 400, 413 and 422.
func (s *site) draft(w http.ResponseWriter, r *http.Request) {
	token, key, cookieToken := r.PathValue("token"), r.PathValue("field"), readSessionCookie(r)
	field, known := students.FindField(key)
	if !known {
		s.render(w, r, "save-status", pageData{}, http.StatusNotFound)
		return
	}
	var data pageData
	status := parseCustomerForm(w, r)
	if status == http.StatusOK && len(r.PostForm[key]) != 1 {
		status = http.StatusBadRequest
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	var result links.SaveResult
	var err error
	stage, failure := "lookup", openFailedMessage
	if status != http.StatusOK {
		data.Error = readFailedMessage
		result, err = s.links.FormState(ctx, token, cookieToken)
	} else if value, invalid := field.Parse(r.PostForm.Get(key), false); invalid != nil {
		data.FieldErrors = map[string]string{key: fieldMessages[key]}
		status = http.StatusUnprocessableEntity
		result, err = s.links.FormState(ctx, token, cookieToken)
	} else {
		stage, failure = "save", saveFailedMessage
		result, err = s.links.SaveDraft(ctx, token, cookieToken, key, value)
	}
	switch {
	case err != nil:
		logging.RecordError(r, stage, err)
		s.render(w, r, "save-status", pageData{Error: failure}, http.StatusInternalServerError)
	case result == links.Unavailable:
		s.render(w, r, "save-status", data, http.StatusNotFound)
	case result == links.AlreadySubmitted:
		locked(w, r)
	default:
		s.render(w, r, "save-status", data, status)
	}
}

// locked answers an autosave against a submitted form: the page is stale, so HTMX reloads it.
func locked(w http.ResponseWriter, r *http.Request) {
	if htmx(r) {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusConflict)
}

// edit reopens the contact form of a submitted enrollment until payment is confirmed or in flight.
// The edit has no autosave: one POST updates the enrollment's contact snapshot, then returns to the agreement.
// Submission time, expiry and accepted terms never change.
func (s *site) edit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := pageData{Path: "/customer/" + url.PathEscape(token), View: "form", Editing: true}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	enrollment, ok := s.resolveSession(w, r, "index.html", &data, token)
	if !ok {
		return
	}
	if enrollment.SubmittedAt == nil {
		redirectCustomer(w, r, data.Path+"/forms")
		return
	}
	editable, err := s.billing.ContactEditable(ctx, enrollment)
	if err != nil {
		logging.RecordError(r, "load", err)
		data.Error = openFailedMessage
		s.render(w, r, "index.html", data, http.StatusInternalServerError)
		return
	}
	if !editable {
		redirectCustomer(w, r, data.Path+"/payments")
		return
	}
	status := http.StatusOK
	data.fill(enrollment.Details)
	if r.Method == http.MethodPost {
		if status = parseCustomerForm(w, r); status != http.StatusOK {
			data.Error = readFailedMessage
		} else if parsed, ok := readContact(r.PostForm, &data); !ok {
			status = http.StatusUnprocessableEntity
		} else if parsed == enrollment.Details {
			// Nothing changed: skip the write, the management refresh and the provider sync.
			redirectCustomer(w, r, data.Path+"/agreement")
			return
		} else {
			err = s.billing.UpdateContact(ctx, enrollment.ID, parsed)
			switch {
			case errors.Is(err, billing.ErrContactLocked):
				redirectCustomer(w, r, data.Path+"/payments")
				return
			case errors.Is(err, billing.ErrUnavailable):
				s.render(w, r, "index.html", data, http.StatusNotFound)
				return
			case err != nil:
				logging.RecordError(r, "save", err)
				data.Error = saveFailedMessage
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
// and reports the validated details and whether every field and the email confirmation passed.
func readContact(form url.Values, data *pageData) (parsed students.Details, ok bool) {
	dateOfBirth, day, month, year := readDateOfBirth(form)
	data.Details = students.Details{
		Name: form.Get("name"), Email: form.Get("email"),
		Phone: form.Get("phone"), Municipality: form.Get("municipality"),
		DateOfBirth: dateOfBirth, Gender: form.Get("gender"),
	}
	data.DobDay, data.DobMonth, data.DobYear = day, month, year
	data.EmailConfirm = form.Get("email_confirm")
	data.FieldErrors = make(map[string]string)
	parse := func(key, raw string, parser func(string, bool) (string, error)) string {
		value, err := parser(raw, true)
		if err != nil {
			data.FieldErrors[key] = fieldMessages[key]
		}
		return value
	}
	parsed = students.Details{
		Name:         parse("name", data.Name, students.ParseName),
		Email:        parse("email", data.Email, students.ParseEmail),
		Phone:        parse("phone", data.Phone, students.ParsePhone),
		Municipality: parse("municipality", data.Municipality, students.ParseMunicipality),
		DateOfBirth:  parse("date_of_birth", data.DateOfBirth, students.ParseDateOfBirth),
		Gender:       parse("gender", data.Gender, students.ParseGender),
	}
	if confirmation, err := students.ParseEmail(data.EmailConfirm, true); err != nil {
		data.FieldErrors["email_confirm"] = fieldMessages["email"]
	} else if data.FieldErrors["email"] == "" && confirmation != parsed.Email {
		data.EmailMismatch = true
		data.FieldErrors["email_confirm"] = "Los correos electrónicos no coinciden."
	}
	return parsed, len(data.FieldErrors) == 0
}

// readSubmission is readContact plus the PIN, which only the first submit takes: it is never
// drafted and the edit form has no PIN.
func readSubmission(form url.Values, data *pageData) (parsed students.Details, pin string, ok bool) {
	parsed, ok = readContact(form, data)
	data.Pin = form.Get("pin")
	pin, err := students.ParsePin(data.Pin, true)
	if err != nil {
		data.FieldErrors["pin"] = fieldMessages["pin"]
		ok = false
	}
	return parsed, pin, ok
}

// checkPin tells the form whether a PIN is free before the final submit. It saves nothing but the spent check:
// it asks whether a student or a submitted enrollment holds the PIN, and the unique index decides at submit.
// It is a POST so the PIN stays out of URLs and access logs. A readable PIN spends its check with no prior
// lookup; anything refused asks the store whether the form is open, so 404 and 409 precede 400, 413, 422 and 429.
func (s *site) checkPin(w http.ResponseWriter, r *http.Request) {
	token, cookieToken := r.PathValue("token"), readSessionCookie(r)
	var data pageData
	status := parseCustomerForm(w, r)
	if status == http.StatusOK && len(r.PostForm["pin"]) != 1 {
		status = http.StatusBadRequest
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if status != http.StatusOK {
		data.Error = readFailedMessage
	} else if pin, invalid := students.ParsePin(r.PostForm.Get("pin"), true); invalid != nil {
		data.FieldErrors = map[string]string{"pin": fieldMessages["pin"]}
		status = http.StatusUnprocessableEntity
	} else {
		spent, err := s.links.SpendPinCheck(ctx, token, cookieToken)
		taken := false
		if err == nil && spent {
			taken, err = s.roster.PinTaken(ctx, pin)
		}
		switch {
		case err != nil:
			logging.RecordError(r, "lookup", err)
			s.render(w, r, "pin-status", pageData{Error: "No pudimos verificar el PIN. Intenta de nuevo."}, http.StatusInternalServerError)
			return
		case spent:
			if taken {
				data.FieldErrors = map[string]string{"pin": pinTakenMessage}
				status = http.StatusUnprocessableEntity
			}
			s.render(w, r, "pin-status", data, status)
			return
		}
		// Out of PIN answers, or the form is not open: the form state below tells which.
		data.Error = pinLimitMessage
		status = http.StatusTooManyRequests
	}
	state, err := s.links.FormState(ctx, token, cookieToken)
	switch {
	case err != nil:
		logging.RecordError(r, "lookup", err)
		s.render(w, r, "pin-status", pageData{Error: openFailedMessage}, http.StatusInternalServerError)
	case state == links.Unavailable:
		s.render(w, r, "pin-status", data, http.StatusNotFound)
	case state == links.AlreadySubmitted:
		locked(w, r)
	default:
		s.render(w, r, "pin-status", data, status)
	}
}

// fieldMessages tell the customer how to fix a rejected field, by field key.
var fieldMessages = map[string]string{
	"name":          "Escribe el nombre y los apellidos del estudiante usando solo letras y espacios.",
	"email":         "Escribe un correo electrónico válido.",
	"phone":         "Escribe un número válido de Puerto Rico o Estados Unidos, con código de área.",
	"municipality":  "Selecciona un municipio o “Fuera de Puerto Rico”.",
	"date_of_birth": "Escribe una fecha de nacimiento válida.",
	"gender":        "Selecciona masculino o femenino.",
	"pin":           "Escribe un PIN de 4 a 6 dígitos.",
}

// Messages more than one handler shows.
const (
	openFailedMessage      = "No pudimos abrir este enlace. Intenta de nuevo."
	readFailedMessage      = "No pudimos leer tu información. Intenta de nuevo."
	saveFailedMessage      = "No pudimos guardar tu información. Intenta de nuevo."
	selectionFailedMessage = "No pudimos leer tu selección. Intenta de nuevo."
	pinTakenMessage        = "Este PIN ya está en uso. Elige otro."
	pinLimitMessage        = "Este enlace alcanzó el límite de intentos de PIN. Comunícate con el equipo."
)

func splitDateOfBirth(raw string) (day, month, year string) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 3 || !asciiDigits(parts[0], 4, 4) || !asciiDigits(parts[1], 1, 2) || !asciiDigits(parts[2], 1, 2) {
		return "", "", ""
	}
	if len(parts[1]) == 1 {
		parts[1] = "0" + parts[1]
	}
	return parts[2], parts[1], parts[0]
}

// readDateOfBirth joins the day, month and year controls into YYYY-MM-DD, zero-padding day and month, and leaves
// judging the date to students.ParseDateOfBirth. A lone date_of_birth field is the fallback tests and load tests post.
func readDateOfBirth(form url.Values) (raw, day, month, year string) {
	if !form.Has("dob_day") && !form.Has("dob_month") && !form.Has("dob_year") {
		raw = form.Get("date_of_birth")
		day, month, year = splitDateOfBirth(raw)
		return raw, day, month, year
	}
	day, month, year = form.Get("dob_day"), form.Get("dob_month"), form.Get("dob_year")
	d, m, y := strings.TrimSpace(day), strings.TrimSpace(month), strings.TrimSpace(year)
	if d+m+y == "" {
		return "", day, month, year
	}
	pad := func(part string) string {
		if len(part) == 1 {
			return "0" + part
		}
		return part
	}
	return y + "-" + pad(m) + "-" + pad(d), day, month, year
}

func asciiDigits(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
