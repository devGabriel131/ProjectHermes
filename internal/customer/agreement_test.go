//go:build integration

package customer

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"projecthermes/internal/billing"
	"projecthermes/internal/links"
	"projecthermes/internal/payments"
	"projecthermes/internal/settings"
)

const agreementDate = "2027-02-28"

func TestAgreementRequiresExplicitConsent(t *testing.T) {
	fixture := newAgreementFixture(t)
	before, studentsBefore := stored(t, fixture.db), studentRecords(t, fixture.db)
	page := customerRequest(t, fixture.mux, http.MethodGet, fixture.path+"/agreement", "", false, fixture.sessionCookie())
	if page.Code != http.StatusOK {
		t.Fatalf("agreement GET status = %d: %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	requireNativePage(t, body, fixture.path+"/agreement")
	checkbox := regexp.MustCompile(`<input\b[^>]*\bname="accept_terms"[^>]*>`).FindString(body)
	if !strings.Contains(checkbox, `type="checkbox"`) || !strings.Contains(checkbox, `value="yes"`) ||
		!strings.Contains(checkbox, "required") || strings.Contains(checkbox, "checked") ||
		!strings.Contains(body, `name="program_start_date" value="`+agreementDate+`"`) {
		t.Fatalf("agreement lost required unchecked consent or displayed date: %s", body)
	}
	requireNoPrivate(t, body, privateFormValues(contactForm("Agreement private student"))...)
	cases := []struct {
		name, suffix, body string
		status             int
	}{
		{"unchecked", "", "program_start_date=" + agreementDate, http.StatusUnprocessableEntity},
		{"wrong checkbox value", "", "accept_terms=on&program_start_date=" + agreementDate, http.StatusUnprocessableEntity},
		{"query cannot grant consent", "?accept_terms=yes", "program_start_date=" + agreementDate, http.StatusUnprocessableEntity},
		{"duplicate consent", "", "accept_terms=yes&accept_terms=yes&program_start_date=" + agreementDate, http.StatusBadRequest},
		{"missing shown date", "", "accept_terms=yes", http.StatusBadRequest},
		{"query cannot supply shown date", "?program_start_date=" + agreementDate, "accept_terms=yes", http.StatusBadRequest},
		{"duplicate shown date", "", "accept_terms=yes&program_start_date=" + agreementDate + "&program_start_date=" + agreementDate, http.StatusBadRequest},
		{"malformed encoding", "", "accept_terms=%zz", http.StatusBadRequest},
		{"oversized body", "", "accept_terms=yes&padding=" + strings.Repeat("x", 4100), http.StatusRequestEntityTooLarge},
		{"invalid calendar date", "", "accept_terms=yes&program_start_date=2027-02-30", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement"+tc.suffix, tc.body, false, fixture.sessionCookie())
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `role="alert"`) {
				t.Fatalf("rejected consent status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			requireNativePage(t, w.Body.String(), fixture.path+"/agreement")
			requireNoPrivate(t, w.Body.String(), privateFormValues(contactForm("Agreement private student"))...)
			if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid consent changed link: before %+v, after %+v", before, after)
			}
		})
	}
	if after := studentRecords(t, fixture.db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatal("viewing or rejecting consent changed students")
	}
}

func TestAgreementAcceptanceFailureKeepsRetryForm(t *testing.T) {
	fixture := newAgreementFixture(t)
	before, studentsBefore := stored(t, fixture.db), studentRecords(t, fixture.db)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE enrollments ADD CONSTRAINT reject_acceptance CHECK (terms_accepted_at IS NULL)").Error; err != nil {
		t.Fatal(err)
	}
	w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", agreementForm(agreementDate).Encode(), false, fixture.sessionCookie())
	body := w.Body.String()
	requireHonestPageFailure(t, w.Code, w.Header().Get("Location"), body)
	if w.Header().Get("HX-Redirect") != "" ||
		!strings.Contains(body, "No pudimos guardar tu aceptación.") || strings.Contains(body, "No pudimos cargar el acuerdo.") {
		t.Fatalf("failed acceptance lost save-specific error or redirected: %s", body)
	}
	requireNativePage(t, body, fixture.path+"/agreement")
	dateControl := regexp.MustCompile(`<input\b[^>]*\bname="program_start_date"[^>]*>`).FindString(body)
	date := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(dateControl)
	if !strings.Contains(dateControl, `type="hidden"`) || len(date) != 2 || date[1] != agreementDate {
		t.Fatalf("failed acceptance lost configured retry date: %s", body)
	}
	checkbox := regexp.MustCompile(`<input\b[^>]*\bname="accept_terms"[^>]*>`).FindString(body)
	if !strings.Contains(checkbox, `type="checkbox"`) || !strings.Contains(checkbox, `value="yes"`) ||
		!strings.Contains(checkbox, "required") || strings.Contains(checkbox, "checked") || strings.Contains(checkbox, "disabled") {
		t.Fatalf("failed acceptance lost required unchecked consent: %s", body)
	}
	submit := regexp.MustCompile(`<(?:button|input)\b[^>]*\btype="submit"[^>]*>`).FindString(body)
	if submit == "" || strings.Contains(submit, "disabled") {
		t.Fatalf("failed acceptance disabled retry: %s", body)
	}
	if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed acceptance changed links: before %+v, after %+v", before, after)
	}
	if after := studentRecords(t, fixture.db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatal("failed acceptance changed students")
	}
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE enrollments DROP CONSTRAINT reject_acceptance").Error; err != nil {
		t.Fatal(err)
	}
	w = customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", agreementForm(date[1]).Encode(), false, fixture.sessionCookie())
	requireAdvance(t, w, fixture.path+"/payments", false)
}

func TestAgreementDateChangeRequiresFreshConsentAndReplaysKeepSnapshot(t *testing.T) {
	fixture := newAgreementFixture(t)
	const changedDate = "2027-03-15"
	if err := fixture.configuration.SetProgramStartDate(t.Context(), changedDate, "staff"); err != nil {
		t.Fatal(err)
	}
	w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", agreementForm(agreementDate).Encode(), false, fixture.sessionCookie())
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `name="program_start_date" value="`+changedDate+`"`) {
		t.Fatalf("stale date failed to reload latest agreement: status %d, %s", w.Code, w.Body.String())
	}
	checkbox := regexp.MustCompile(`<input\b[^>]*\bname="accept_terms"[^>]*>`).FindString(w.Body.String())
	if strings.Contains(checkbox, "checked") || stored(t, fixture.db)[0].TermsAcceptedAt != nil {
		t.Fatal("stale date kept consent or accepted old agreement")
	}
	w = customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", agreementForm(changedDate).Encode(), false, fixture.sessionCookie())
	requireAdvance(t, w, fixture.path+"/payments", false, privateFormValues(contactForm("Agreement private student"))...)
	before, studentsBefore := stored(t, fixture.db), studentRecords(t, fixture.db)
	if before[0].TermsAcceptedAt == nil || before[0].ProgramStartDate != changedDate {
		t.Fatalf("acceptance snapshot missing: %+v", before[0])
	}
	if err := fixture.configuration.SetProgramStartDate(t.Context(), "2028-04-01", "other staff"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{agreementForm(agreementDate).Encode(), "", "accept_terms=%zz", "padding=" + strings.Repeat("x", 5000)} {
		w = customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", body, false, fixture.sessionCookie())
		requireAdvance(t, w, fixture.path+"/payments", false)
	}
	// Reviewing an accepted agreement must not depend on current configuration.
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE settings RENAME TO unavailable_settings").Error; err != nil {
		t.Fatal(err)
	}
	w = customerRequest(t, fixture.mux, http.MethodGet, fixture.path+"/agreement", "", false, fixture.sessionCookie())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "15 de marzo de 2027") ||
		strings.Contains(w.Body.String(), "2028") || strings.Contains(w.Body.String(), `name="accept_terms"`) {
		t.Fatalf("accepted review lost immutable snapshot: status %d, %s", w.Code, w.Body.String())
	}
	if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("agreement replay or review changed snapshot: before %+v, after %+v", before, after)
	}
	if after := studentRecords(t, fixture.db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatal("agreement replay changed student enrollment")
	}
}

func TestUnconfiguredAgreementCannotBeAccepted(t *testing.T) {
	fixture := newAgreementFixture(t)
	if err := fixture.db.WithContext(t.Context()).Where("id = 1").Delete(&settings.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	before := stored(t, fixture.db)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := customerRequest(t, fixture.mux, method, fixture.path+"/agreement", agreementForm(agreementDate).Encode(), false, fixture.sessionCookie())
		want := http.StatusOK
		if method == http.MethodPost {
			want = http.StatusUnprocessableEntity
		}
		if w.Code != want || !strings.Contains(w.Body.String(), `role="alert"`) ||
			strings.Contains(w.Body.String(), `name="accept_terms"`) || strings.Contains(w.Body.String(), `type="submit"`) ||
			strings.Contains(w.Body.String(), agreementDate) {
			t.Fatalf("unconfigured agreement invented date or offered acceptance: status %d, %s", w.Code, w.Body.String())
		}
	}
	if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
		t.Fatal("unconfigured acceptance changed link")
	}
}

func TestPaymentChoiceValidatesPersistsAndRemainsIsolated(t *testing.T) {
	fixture := newAgreementFixture(t)
	fixture.accept(t)
	other, err := fixture.store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	_, otherCookie, err := fixture.store.StartEnrollment(t.Context(), other.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := fixture.store.Submit(t.Context(), other.Token, otherCookie, contactDetails("Other private student"), ""); err != nil || result != links.Saved {
		t.Fatalf("submit other: %v, %v", result, err)
	}
	if accepted, err := fixture.store.AcceptTerms(t.Context(), other.Token, otherCookie, agreementDate); err != nil || !accepted {
		t.Fatalf("accept other: %v, %v", accepted, err)
	}
	otherBefore, _, err := fixture.store.LookupSession(t.Context(), other.Token, otherCookie)
	if err != nil || otherBefore == nil {
		t.Fatal(err)
	}
	original, _, err := fixture.store.LookupSession(t.Context(), fixture.entry.Token, fixture.cookie)
	if err != nil || original == nil {
		t.Fatal(err)
	}
	before, studentsBefore := stored(t, fixture.db), studentRecords(t, fixture.db)
	for _, tc := range []struct {
		name, suffix, body string
		status             int
	}{
		{"missing", "", "", http.StatusUnprocessableEntity},
		{"query cannot choose", "?payment_plan=full", "", http.StatusUnprocessableEntity},
		{"unknown", "", "payment_plan=other", http.StatusUnprocessableEntity},
		{"case sensitive", "", "payment_plan=FULL", http.StatusUnprocessableEntity},
		{"duplicate", "", "payment_plan=full&payment_plan=full", http.StatusBadRequest},
		{"multiple options", "", "payment_plan=full&payment_plan=six", http.StatusBadRequest},
		{"malformed", "", "payment_plan=%zz", http.StatusBadRequest},
		{"oversized", "", "payment_plan=full&padding=" + strings.Repeat("x", 4100), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/payments"+tc.suffix, tc.body, false, fixture.sessionCookie())
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `role="alert"`) {
				t.Fatalf("invalid choice status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
				t.Fatal("invalid choice changed saved links")
			}
		})
	}
	for _, plan := range payments.Plans() {
		w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/payments", url.Values{"payment_plan": {plan.ID}}.Encode(), false, fixture.sessionCookie())
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "https://checkout.stripe.com") {
			t.Fatalf("expected redirect to Stripe Checkout, got code=%d, loc=%q, body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
		}
		selected, _, err := fixture.store.LookupSession(t.Context(), fixture.entry.Token, fixture.cookie)
		if err != nil || selected == nil {
			t.Fatalf("lookup after choice: %+v, %v", selected, err)
		}
		if !reflect.DeepEqual(selected, original) {
			t.Fatal("choice changed contact data or agreement snapshot")
		}
		w = customerRequest(t, fixture.mux, http.MethodGet, fixture.path+"/payments", "", false, fixture.sessionCookie())
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, plan.Name) {
			t.Fatalf("choice reload missing saved state: %d, %s", w.Code, body)
		}
		requireNativePage(t, body, fixture.path+"/payments")
		radios := regexp.MustCompile(`<input\b[^>]*\bname="payment_plan"[^>]*>`).FindAllString(body, -1)
		if len(radios) != 3 {
			t.Fatalf("payment choices = %d, want 3", len(radios))
		}
		for _, radio := range radios {
			if !strings.Contains(radio, `type="radio"`) || !strings.Contains(radio, "required") ||
				strings.Contains(radio, "checked") != strings.Contains(radio, `value="`+plan.ID+`"`) {
				t.Fatalf("required radio state wrong for %s: %s", plan.ID, radio)
			}
		}
		for _, option := range payments.Plans() {
			if !strings.Contains(body, payments.Money(option.TotalCents())) {
				t.Errorf("missing total for %s", option.ID)
			}
		}
		if strings.Count(body, `class="payment-installment-amount"`) != 10 {
			t.Fatal("payment options hid one or more installments")
		}
		requireNoPrivate(t, body, privateFormValues(contactForm("Agreement private student"))...)
	}
	if otherAfter, _, err := fixture.store.LookupSession(t.Context(), other.Token, otherCookie); err != nil || !reflect.DeepEqual(otherAfter, otherBefore) {
		t.Fatal("choice changed another token")
	}
	if after := studentRecords(t, fixture.db); !reflect.DeepEqual(after, studentsBefore) {
		t.Fatal("payment choice changed student enrollment")
	}
}

func TestPaymentChoiceFailurePreservesSavedAndAttemptedState(t *testing.T) {
	full, ok := payments.Find("full")
	if !ok {
		t.Fatal("full payment plan missing")
	}
	for _, tc := range []struct {
		name, body, checked, errorText string
		status                         int
		rejectChange                   bool
	}{
		{"duplicate choices", "payment_plan=three&payment_plan=six", "three", "una sola opción de pago", http.StatusBadRequest, false},
		{"unknown choice", "payment_plan=other", "", "una de las tres opciones de pago", http.StatusUnprocessableEntity, false},
		{"persistence failure", "payment_plan=six", "six", "iniciar el pago seguro", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAgreementFixture(t)
			fixture.accept(t)
			contract := billing.Contract{EnrollmentID: stored(t, fixture.db)[0].ID, Reference: rand.Text(), PlanID: full.ID, PlanName: full.Name, InstallmentCount: len(full.Amounts), TotalCents: full.TotalCents()}
			if err := fixture.db.Create(&contract).Error; err != nil {
				t.Fatalf("save original plan: %v", err)
			}
			before, studentsBefore := stored(t, fixture.db), studentRecords(t, fixture.db)
			if tc.rejectChange {
				if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE billing_checkout_attempts ADD CONSTRAINT reject_attempt CHECK (false)").Error; err != nil {
					t.Fatal(err)
				}
			}
			w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/payments", tc.body, false, fixture.sessionCookie())
			body := w.Body.String()
			if w.Code != tc.status || w.Header().Get("Location") != "" || w.Header().Get("HX-Redirect") != "" ||
				!strings.Contains(body, `role="alert"`) || !strings.Contains(body, tc.errorText) {
				t.Fatalf("failed plan change status = %d, want %d, Location=%q, HX-Redirect=%q: %s",
					w.Code, tc.status, w.Header().Get("Location"), w.Header().Get("HX-Redirect"), body)
			}
			saved := regexp.MustCompile(`(?s)<[^>]+\bclass="payment-saved"[^>]*>.*?</[^>]+>`).FindString(body)
			if !strings.Contains(saved, `role="status"`) || !strings.Contains(saved, full.Name) {
				t.Fatalf("failed change lost previous saved plan: %s", body)
			}
			requireNativePage(t, body, fixture.path+"/payments")
			radios := regexp.MustCompile(`<input\b[^>]*\bname="payment_plan"[^>]*>`).FindAllString(body, -1)
			if len(radios) != len(payments.Plans()) {
				t.Fatalf("payment choices = %d, want %d: %s", len(radios), len(payments.Plans()), body)
			}
			for _, radio := range radios {
				wantChecked := tc.checked != "" && strings.Contains(radio, `value="`+tc.checked+`"`)
				if !strings.Contains(radio, `type="radio"`) || !strings.Contains(radio, "required") ||
					strings.Contains(radio, "disabled") || strings.Contains(radio, "checked") != wantChecked {
					t.Fatalf("failed change lost attempted selection %q: %s", tc.checked, radio)
				}
			}
			submit := regexp.MustCompile(`<(?:button|input)\b[^>]*\btype="submit"[^>]*>`).FindString(body)
			if submit == "" || strings.Contains(submit, "disabled") {
				t.Fatalf("failed plan change disabled retry: %s", body)
			}
			if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed plan change altered saved links: before %+v, after %+v", before, after)
			}
			if after := studentRecords(t, fixture.db); !reflect.DeepEqual(after, studentsBefore) {
				t.Fatal("failed plan change altered students")
			}
		})
	}
}

func TestAgreementPaymentStageAndUsabilityCannotBeBypassed(t *testing.T) {
	fixture := newFixture(t)
	db, store, entry, path, mux := fixture.db, fixture.store, fixture.entry, fixture.path, fixture.mux
	for _, stage := range []string{"before contact", "before agreement"} {
		if stage == "before agreement" {
			if result, err := store.Submit(t.Context(), entry.Token, fixture.cookie, contactDetails("Stage private student"), ""); err != nil || result != links.Saved {
				t.Fatalf("submit stage: %v, %v", result, err)
			}
		}
		before := stored(t, db)
		for _, view := range []string{"agreement", "payments"} {
			if stage == "before agreement" && view == "agreement" {
				continue
			}
			for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
				for _, body := range []string{"accept_terms=yes&program_start_date=" + agreementDate + "&payment_plan=full", "payment_plan=%zz", strings.Repeat("x", 5000)} {
					w := customerRequest(t, mux, method, path+"/"+view, body, false, fixture.sessionCookie())
					target := path + "/forms"
					if stage == "before agreement" {
						target = path + "/agreement"
					}
					requireAdvance(t, w, target, false)
				}
			}
		}
		if after := stored(t, db); !reflect.DeepEqual(after, before) {
			t.Fatal("wrong-stage request changed link")
		}
	}
	canceled, err := store.CreateLink(t.Context(), "staff", "", later)
	if err != nil {
		t.Fatal(err)
	}
	_, canceledCookie, err := store.StartEnrollment(t.Context(), canceled.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelLink(t.Context(), canceled.ID, "staff"); err != nil {
		t.Fatal(err)
	}
	if err := settings.New(db).SetProgramStartDate(t.Context(), agreementDate, "staff"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.AcceptTerms(t.Context(), entry.Token, fixture.cookie, agreementDate); err != nil || !accepted {
		t.Fatalf("accept before expiry: %v, %v", accepted, err)
	}
	if err := db.WithContext(t.Context()).Model(&links.Enrollment{}).Where("submitted_at IS NOT NULL").Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(t.Context()).Model(&links.Link{}).Where("id = ?", entry.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	before := stored(t, db)
	for _, tc := range []struct{ tok, cook string }{
		{entry.Token, fixture.cookie},
		{canceled.Token, canceledCookie},
		{strings.Repeat("A", len(entry.Token)), fixture.cookie},
		{"bad-token", ""},
	} {
		for _, view := range []string{"agreement", "payments"} {
			for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
				for _, body := range []string{"accept_terms=yes&program_start_date=" + agreementDate + "&payment_plan=full", "payment_plan=%zz", strings.Repeat("x", 5000)} {
					w := customerRequest(t, mux, method, "/customer/"+tc.tok+"/"+view, body, false, &http.Cookie{Name: sessionCookieName, Value: tc.cook})
					if w.Code != http.StatusNotFound || (method != http.MethodHead && !strings.Contains(w.Body.String(), "customer-not-found")) {
						t.Fatalf("unusable %s %s status = %d: %s", method, view, w.Code, w.Body.String())
					}
				}
			}
		}
	}
	if after := stored(t, db); !reflect.DeepEqual(after, before) {
		t.Fatal("unusable request changed link")
	}
}

func TestAgreementAndPaymentHeadRemainReadOnly(t *testing.T) {
	fixture := newAgreementFixture(t)
	server := serve(t, fixture.db)
	headKeeps(t, fixture.db, server, fixture.path+"/agreement", http.StatusOK, fixture.cookie)
	headKeeps(t, fixture.db, server, fixture.path+"/payments", http.StatusSeeOther, fixture.cookie)
	fixture.accept(t)
	for _, suffix := range []string{"", "/forms", "/agreement", "/payments"} {
		headKeeps(t, fixture.db, server, fixture.path+suffix, http.StatusOK, fixture.cookie)
	}
}

func TestAgreementAndPaymentFailuresNeverClaimSuccess(t *testing.T) {
	fixture := newAgreementFixture(t)
	fixture.accept(t)
	before := stored(t, fixture.db)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE billing_checkout_attempts ADD CONSTRAINT reject_attempt CHECK (false)").Error; err != nil {
		t.Fatal(err)
	}
	w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/payments", "payment_plan=full", false, fixture.sessionCookie())
	requireHonestPageFailure(t, w.Code, w.Header().Get("Location"), w.Body.String())
	if strings.Contains(w.Body.String(), `class="payment-saved"`) {
		t.Fatal("failed payment choice claimed saved state")
	}
	if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
		t.Fatal("failed payment choice changed link")
	}
	failing := newMux(t, failingDB(t, fixture.db))
	for _, view := range []string{"agreement", "payments"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead} {
			w = customerRequest(t, failing, method, fixture.path+"/"+view, "payment_plan=full", false, fixture.sessionCookie())
			if w.Code != http.StatusInternalServerError || w.Header().Get("Location") != "" || (method == http.MethodHead && w.Body.Len() != 0) {
				t.Fatalf("lookup failure claimed a stage or success: %d, %s", w.Code, w.Body.String())
			}
		}
	}
}

func TestAgreementSettingsFailureIsServerError(t *testing.T) {
	fixture := newAgreementFixture(t)
	if err := fixture.db.WithContext(t.Context()).Exec("ALTER TABLE settings RENAME TO unavailable_settings").Error; err != nil {
		t.Fatal(err)
	}
	before := stored(t, fixture.db)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := customerRequest(t, fixture.mux, method, fixture.path+"/agreement", agreementForm(agreementDate).Encode(), false, fixture.sessionCookie())
		requireHonestPageFailure(t, w.Code, w.Header().Get("Location"), w.Body.String())
		if strings.Contains(w.Body.String(), `name="accept_terms"`) {
			t.Fatal("unavailable settings offered an invented agreement")
		}
	}
	if after := stored(t, fixture.db); !reflect.DeepEqual(after, before) {
		t.Fatal("settings failure changed acceptance")
	}
}

type agreementFixture struct {
	fixture
	configuration *settings.Store
}

func newAgreementFixture(t *testing.T) agreementFixture {
	t.Helper()
	f := newFixture(t)
	configuration := settings.New(f.db)
	if err := configuration.SetProgramStartDate(t.Context(), agreementDate, "staff"); err != nil {
		t.Fatal(err)
	}
	if result, err := f.store.Submit(t.Context(), f.entry.Token, f.cookie, contactDetails("Agreement private student"), ""); err != nil || result != links.Saved {
		t.Fatalf("submit fixture: %v, %v", result, err)
	}
	return agreementFixture{fixture: f, configuration: configuration}
}

func (fixture agreementFixture) accept(t *testing.T) {
	t.Helper()
	w := customerRequest(t, fixture.mux, http.MethodPost, fixture.path+"/agreement", agreementForm(agreementDate).Encode(), false, fixture.sessionCookie())
	requireAdvance(t, w, fixture.path+"/payments", false)
}

func agreementForm(date string) url.Values {
	return url.Values{"accept_terms": {"yes"}, "program_start_date": {date}}
}

func requireNativePage(t *testing.T, body, action string) {
	t.Helper()
	if !strings.Contains(body, `action="`+action+`" method="post"`) || strings.Contains(body, "<script") || strings.Contains(body, "hx-post") {
		t.Fatalf("new page lost native, script-free form: %s", body)
	}
}

func requireHonestPageFailure(t *testing.T, status int, location, body string) {
	t.Helper()
	if status != http.StatusInternalServerError || location != "" || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("database failure claimed success: status %d, location %q, %s", status, location, body)
	}
	requireNoPrivate(t, body, privateFormValues(contactForm("Agreement private student"))...)
}
