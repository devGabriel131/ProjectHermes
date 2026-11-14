//go:build integration

package links

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/payments"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

// staff is the acting account for tests that aren't about the audit trail.
const staff = "tester"

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestIssuedLinkAndSessionBoundary(t *testing.T) {
	db, store, ctx := testStore(t)

	first, err := store.CreateLink(ctx, staff, "", later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateLink(ctx, staff, "Custom Cohort", later)
	if err != nil {
		t.Fatal(err)
	}
	wantDate := time.Now().In(payments.PuertoRico).Format(time.DateOnly)
	if first.Name != wantDate || second.Name != "Custom Cohort" {
		t.Fatalf("names = %q, %q; want %q, %q", first.Name, second.Name, wantDate, "Custom Cohort")
	}

	e1, cookie1, err := store.StartEnrollment(ctx, first.Token, "")
	if err != nil || e1 == nil || cookie1 == "" {
		t.Fatalf("start first: e=%+v, cookie=%q, err=%v", e1, cookie1, err)
	}

	t.Run("only the session digest is stored", func(t *testing.T) {
		if first.ID == 0 || e1.ID == 0 {
			t.Fatalf("IDs should be positive: link=%d, enrollment=%d", first.ID, e1.ID)
		}
		var row map[string]any
		if err := db.WithContext(ctx).Table("enrollments").Where("session_hash = ?", digestOf(cookie1)).Take(&row).Error; err != nil {
			t.Fatalf("started session has no persisted SHA-256 digest: %v", err)
		}
		for column, value := range row {
			var stored []byte
			switch v := value.(type) {
			case string:
				stored = []byte(v)
			case []byte:
				stored = v
			}
			if bytes.Contains(stored, []byte(cookie1)) {
				t.Fatalf("column %s stores the usable cookie token", column)
			}
		}
	})

	t.Run("reuses live session and isolates across links", func(t *testing.T) {
		reused, newCookie, err := store.StartEnrollment(ctx, first.Token, cookie1)
		if err != nil || reused == nil || reused.ID != e1.ID || newCookie != "" {
			t.Fatalf("reuse session: reused=%+v, newCookie=%q, err=%v; want ID %d, empty newCookie", reused, newCookie, err, e1.ID)
		}
		// Using first link's cookie on second link must not match first link's enrollment; it starts a fresh one.
		e2, cookie2, err := store.StartEnrollment(ctx, second.Token, cookie1)
		if err != nil || e2 == nil || e2.ID == e1.ID || e2.LinkID != second.ID || cookie2 == "" || cookie2 == cookie1 {
			t.Fatalf("cross-link start: e2=%+v, cookie2=%q, err=%v", e2, cookie2, err)
		}
	})

	tampered := first.Token[:len(first.Token)-1] + "A"
	if tampered == first.Token {
		tampered = first.Token[:len(first.Token)-1] + "B"
	}
	unissued := base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))
	sqlLike := strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes))

	cases := []struct {
		name   string
		link   string
		cookie string
		valid  bool
	}{
		{"valid link and cookie", first.Token, cookie1, true},
		{"unissued link", unissued, cookie1, false},
		{"tampered link", tampered, cookie1, false},
		{"missing link", "", cookie1, false},
		{"SQL-like link", sqlLike, cookie1, false},
		{"unissued cookie", first.Token, unissued, false},
		{"missing cookie", first.Token, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := store.LookupSession(ctx, tc.link, tc.cookie)
			if err != nil {
				t.Fatal(err)
			}
			valid := sess != nil && sess.Usable
			if valid != tc.valid {
				t.Fatalf("valid = %v, want %v (sess=%+v)", valid, tc.valid, sess)
			}
			if !tc.valid {
				for _, save := range []func() (SaveResult, error){
					func() (SaveResult, error) { return store.SaveDraft(ctx, tc.link, tc.cookie, "name", "Must not save") },
					func() (SaveResult, error) {
						return store.Submit(ctx, tc.link, tc.cookie, contactDetails("Must not save"), "")
					},
				} {
					if result, err := save(); err != nil || result != Unavailable {
						t.Fatalf("invalid write: result=%v, err=%v", result, err)
					}
				}
			}
		})
	}

	t.Run("enforces MaxLinkEnrollments for new starts while keeping existing live sessions", func(t *testing.T) {
		capped, err := store.CreateLink(ctx, staff, "Capped", later)
		if err != nil {
			t.Fatal(err)
		}
		var firstCookie string
		for i := range MaxLinkEnrollments {
			started, c, err := store.StartEnrollment(ctx, capped.Token, "")
			if err != nil || started == nil || c == "" {
				t.Fatalf("start %d: started=%+v, err=%v", i, started, err)
			}
			if i == 0 {
				firstCookie = c
			}
		}
		// New visitor is refused once MaxLinkEnrollments enrollments exist.
		over, overCookie, err := store.StartEnrollment(ctx, capped.Token, "")
		if err != nil || over != nil || overCookie != "" {
			t.Fatalf("start over cap: over=%+v, cookie=%q, err=%v", over, overCookie, err)
		}
		sess, err := store.LookupSession(ctx, capped.Token, "")
		if err != nil || sess == nil || sess.LinkUsable {
			t.Fatalf("capped link should report LinkUsable=false for new visitor: %+v, err=%v", sess, err)
		}
		// Existing visitor with a live cookie still resumes.
		resumed, newC, err := store.StartEnrollment(ctx, capped.Token, firstCookie)
		if err != nil || resumed == nil || newC != "" {
			t.Fatalf("existing session on full link failed: resumed=%+v, newC=%q, err=%v", resumed, newC, err)
		}
	})

	t.Run("failed persistence does not issue a link or session", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		entry, err := store.CreateLink(cancelled, staff, "", later)
		if err == nil || entry != (LinkEntry{}) {
			t.Fatal("cancelled CreateLink must return an error and no usable link")
		}
		if started, c, err := store.StartEnrollment(cancelled, first.Token, ""); err == nil || started != nil || c != "" {
			t.Fatalf("cancelled StartEnrollment: started=%+v, c=%q, err=%v", started, c, err)
		}
		for _, save := range []func() (SaveResult, error){
			func() (SaveResult, error) {
				return store.SaveDraft(cancelled, first.Token, cookie1, "name", "Must not save")
			},
			func() (SaveResult, error) {
				return store.Submit(cancelled, first.Token, cookie1, contactDetails("Must not save"), "")
			},
		} {
			if result, err := save(); err == nil || result != Unavailable {
				t.Fatalf("failed database write: result=%v, err=%v", result, err)
			}
		}
	})
}

func TestStatusFollowsCustomerProgress(t *testing.T) {
	db, store, ctx := testStore(t)
	link, err := store.CreateLink(ctx, staff, "", later)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateLink(ctx, staff, "", later)
	if err != nil {
		t.Fatal(err)
	}
	otherEnrollment, _, err := store.StartEnrollment(ctx, other.Token, "")
	if err != nil || otherEnrollment == nil {
		t.Fatalf("start other: %v", err)
	}

	var eID uint64
	var cookie string
	details := contactDetails("Ana Pérez")
	refused := func(save func() (SaveResult, error)) func() error {
		return func() error {
			if saved, err := save(); err != nil || saved != AlreadySubmitted {
				return fmt.Errorf("submitted form replay: saved=%v, err=%v", saved, err)
			}
			return nil
		}
	}
	saved := func(save func() (SaveResult, error)) func() error {
		return func() error {
			if result, err := save(); err != nil || result != Saved {
				return fmt.Errorf("save: result=%v, err=%v", result, err)
			}
			return nil
		}
	}

	steps := []struct {
		name   string
		act    func() error
		status Status
		saved  string
	}{
		{"started", func() error {
			started, c, err := store.StartEnrollment(ctx, link.Token, "")
			if err != nil || started == nil || c == "" {
				return fmt.Errorf("start: %+v, %q, %v", started, c, err)
			}
			eID, cookie = started.ID, c
			return nil
		}, InProgress, ""},
		{"empty autosave", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, cookie, "name", "") }), InProgress, ""},
		{"autosaved", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, cookie, "name", "Ana") }), InProgress, "Ana"},
		{"submitted", saved(func() (SaveResult, error) { return store.Submit(ctx, link.Token, cookie, details, "") }), AwaitingPayment, details.Name},
		{"autosave after submit is refused", refused(func() (SaveResult, error) {
			return store.SaveDraft(ctx, link.Token, cookie, "name", "Changed after submit")
		}), AwaitingPayment, details.Name},
		{"second submit is refused", refused(func() (SaveResult, error) {
			return store.Submit(ctx, link.Token, cookie, contactDetails("Changed after submit"), "")
		}), AwaitingPayment, details.Name},
		{"cancel cannot revoke submitted enrollment", func() error { _, err := store.CancelEnrollment(ctx, eID, staff); return err }, AwaitingPayment, details.Name},
		{"canceling parent link keeps submitted enrollment awaiting payment", func() error {
			l, err := store.CancelLink(ctx, link.ID, staff)
			if err != nil || l == nil || l.Status != LinkCanceled {
				return fmt.Errorf("cancel link: %+v, %v", l, err)
			}
			sess, err := store.LookupSession(ctx, link.Token, cookie)
			if err != nil || sess == nil || !sess.Usable || sess.LinkUsable {
				return fmt.Errorf("submitted session after link cancel: %+v, %v", sess, err)
			}
			return nil
		}, AwaitingPayment, details.Name},
		{"agreement accepted", func() error { return acceptAgreement(ctx, db, store, link.Token, cookie, programStart) }, AwaitingPayment, details.Name},
		{"enrolled on payment", func() error {
			return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return Enroll(tx, eID, time.Now().UTC())
			})
		}, Finished, details.Name},
	}

	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) {
			if err := step.act(); err != nil {
				t.Fatal(err)
			}
			listed := listByID(t, store, ctx)
			got := listed[eID]
			if got.Status != step.status || got.Name != step.saved {
				t.Fatalf("listed %+v, want status %q, name %q", got, step.status, step.saved)
			}
			if listed[otherEnrollment.ID].Status != InProgress {
				t.Fatalf("other enrollment changed to %q", listed[otherEnrollment.ID].Status)
			}
			var row Enrollment
			if err := db.WithContext(ctx).Where("id = ?", eID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			enrolled := studentsFor(t, db, ctx, eID)
			if step.status == Finished {
				if len(enrolled) != 1 || enrolled[0].Details != details || row.Details != details || got.Phone != details.Phone || got.PaidAt == nil ||
					!enrolled[0].CreatedAt.Equal(*got.PaidAt) || row.CanceledAt != nil || row.CanceledBy != "" ||
					got.Cancelable() || got.Trashed() {
					t.Fatalf("completion changed or lost enrollment: entry=%+v, row=%+v, students=%+v", got, row, enrolled)
				}
			} else if step.status == AwaitingPayment {
				if len(enrolled) != 0 || row.Details != details || got.Phone != details.Phone || got.SubmittedAt == nil ||
					got.PaidAt != nil || got.Trashed() {
					t.Fatalf("awaiting payment state invalid: entry=%+v, row=%+v, students=%+v", got, row, enrolled)
				}
			} else if len(enrolled) != 0 || row.SubmittedAt != nil {
				t.Fatalf("unfinished form enrolled a student: row=%+v, students=%+v", row, enrolled)
			}
		}) {
			return
		}
	}

	// Student retains its source enrollment even when staff independently edit students.
	if err := db.WithContext(ctx).Where("id = ?", eID).Delete(&Enrollment{}).Error; err == nil {
		t.Fatal("student did not restrict deletion of its source enrollment")
	}
	if err := db.WithContext(ctx).Model(&Enrollment{}).Where("id = ?", eID).
		Update("id", eID+1000).Error; err == nil {
		t.Fatal("student did not restrict changing its source enrollment id")
	}
}

func TestStartEnrollmentObservesConcurrentLinkCancellation(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.CreateLink(ctx, staff, "", later)
	if err != nil {
		t.Fatal(err)
	}
	blocker := db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	startCtx, cancelStart := context.WithCancel(ctx)
	type result struct {
		e      *Enrollment
		cookie string
		err    error
	}
	var results chan result
	defer func() {
		blocker.Rollback()
		cancelStart()
		if results != nil {
			for range results {
			}
		}
	}()
	canceled, err := New(blocker, nil).CancelLink(ctx, issued.ID, "canceler")
	if err != nil || canceled == nil || canceled.Status != LinkCanceled {
		t.Fatalf("hold cancellation: entry=%+v, err=%v", canceled, err)
	}

	results = make(chan result, 1)
	go func() {
		defer close(results)
		e, cookie, err := store.StartEnrollment(startCtx, issued.Token, "")
		results <- result{e: e, cookie: cookie, err: err}
	}()
	testdb.AwaitBlocked(t, ctx, db, blocker, results)
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.err != nil {
		t.Fatalf("StartEnrollment after cancellation: %v", got.err)
	}
	var count int64
	if err := db.WithContext(ctx).Model(&Enrollment{}).Where("link_id = ?", issued.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if got.e != nil || got.cookie != "" || count != 0 {
		t.Fatalf("StartEnrollment ignored committed cancellation: e=%+v, cookie=%q, count=%d", got.e, got.cookie, count)
	}
}

func TestConcurrentSubmissionSubmitsOnce(t *testing.T) {
	db, store, ctx := testStore(t)
	link, e, cookie := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.SaveDraft(ctx, link.Token, cookie, "name", "Draft only"); err != nil || saved != Saved {
		t.Fatalf("draft: saved=%v, err=%v", saved, err)
	}
	if enrolled := studentsFor(t, db, ctx, e.ID); len(enrolled) != 0 {
		t.Fatalf("draft enrolled students: %+v", enrolled)
	}
	type result struct {
		details students.Details
		saved   SaveResult
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, details := range []students.Details{
		{Name: "First final answer", Email: "first@example.com", Phone: "+17872345678", Municipality: "San Juan"},
		{Name: "Second final answer", Email: "second@example.com", Phone: "+12025550123", Municipality: "Fuera de Puerto Rico"},
	} {
		go func() {
			<-start
			saved, err := store.Submit(ctx, link.Token, cookie, details, "")
			results <- result{details: details, saved: saved, err: err}
		}()
	}
	close(start)
	var winner students.Details
	replays := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		switch got.saved {
		case Saved:
			if winner != (students.Details{}) {
				t.Fatal("competing submissions both succeeded")
			}
			winner = got.details
		case AlreadySubmitted:
			replays++
		default:
			t.Fatalf("competing submission returned %v, want Saved or AlreadySubmitted", got.saved)
		}
	}
	if replays != 1 {
		t.Fatalf("competing submissions returned %d replays, want one", replays)
	}
	completed := lookupEnrollment(t, store, ctx, link.Token, cookie)
	enrolled := studentsFor(t, db, ctx, e.ID)
	if winner == (students.Details{}) || completed == nil || completed.Details != winner || completed.SubmittedAt == nil ||
		len(enrolled) != 0 || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatalf("completion not atomic: winner=%+v, enrollment=%+v, students=%+v", winner, completed, enrolled)
	}
}

// A PIN belongs to one submitted enrollment at a time: of two visitors racing for it, one submits and the other stays open with its draft, free to pick another PIN.
func TestConcurrentSubmissionsCannotShareAPin(t *testing.T) {
	db, store, ctx := testStore(t)
	link, err := store.CreateLink(ctx, staff, "", later)
	if err != nil {
		t.Fatal(err)
	}
	// Two visitors using the SAME reusable link concurrently!
	cookies := make([]string, 2)
	for i := range cookies {
		_, c, err := store.StartEnrollment(ctx, link.Token, "")
		if err != nil || c == "" {
			t.Fatalf("start %d: %v", i, err)
		}
		if saved, err := store.SaveDraft(ctx, link.Token, c, "name", fmt.Sprintf("Draft %d", i)); err != nil || saved != Saved {
			t.Fatalf("draft: saved=%v, err=%v", saved, err)
		}
		cookies[i] = c
	}
	start := make(chan struct{})
	results := make(chan SaveResult, len(cookies))
	for _, c := range cookies {
		go func() {
			<-start
			saved, err := store.Submit(ctx, link.Token, c, contactDetails("Final answer"), "012345")
			if err != nil {
				t.Error(err)
			}
			results <- saved
		}()
	}
	close(start)
	outcomes := map[SaveResult]int{}
	for range cookies {
		outcomes[<-results]++
	}
	if outcomes[Saved] != 1 || outcomes[PinTaken] != 1 {
		t.Fatalf("outcomes = %v, want one Saved and one PinTaken", outcomes)
	}
	var holders int64
	if err := db.WithContext(ctx).Model(&Enrollment{}).Where("pin = ?", "012345").Count(&holders).Error; err != nil || holders != 1 {
		t.Fatalf("enrollments holding the PIN = %d, err=%v; want 1", holders, err)
	}
	for _, c := range cookies {
		e := lookupEnrollment(t, store, ctx, link.Token, c)
		if e == nil {
			t.Fatal("missing enrollment")
		}
		if e.SubmittedAt != nil {
			continue
		}
		if !strings.HasPrefix(e.Name, "Draft ") {
			t.Fatalf("losing enrollment lost its draft: %+v", e.Details)
		}
		if saved, err := store.Submit(ctx, link.Token, c, contactDetails("Second try"), "543210"); err != nil || saved != Saved {
			t.Fatalf("losing enrollment could not take another PIN: saved=%v, err=%v", saved, err)
		}
	}
}

func TestSubmissionFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conflict   bool
		constraint string
	}{
		{"enrollment UPDATE check", false, "CHECK (name <> 'Rejected submission')"},
		{"deferred commit constraint", true, "UNIQUE (name) DEFERRABLE INITIALLY DEFERRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, ctx := testStore(t)
			details := contactDetails("Rejected submission")
			if tc.conflict {
				exLink, _, exCookie := createAndStart(t, store, ctx, staff, later)
				if saved, err := store.Submit(ctx, exLink.Token, exCookie, details, ""); err != nil || saved != Saved {
					t.Fatalf("seed existing student: saved=%v, err=%v", saved, err)
				}
			}
			link, e, cookie := createAndStart(t, store, ctx, staff, later)
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			for _, field := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, link.Token, cookie, field.Key, field.Value(draft)); err != nil || saved != Saved {
					t.Fatalf("save %s draft: saved=%v, err=%v", field.Key, saved, err)
				}
			}
			before := lookupEnrollment(t, store, ctx, link.Token, cookie)
			if before == nil {
				t.Fatal("load draft: nil")
			}
			// Constraint SQL is fixed test data, scoped to this test's schema.
			if err := db.WithContext(ctx).Exec("ALTER TABLE enrollments ADD CONSTRAINT test_link_rejection " + tc.constraint).Error; err != nil {
				t.Fatal(err)
			}
			changes := 0
			store = New(db, func() { changes++ })
			if saved, err := store.Submit(ctx, link.Token, cookie, details, ""); err == nil || saved != Unavailable {
				t.Fatalf("failed transaction reported success: saved=%v, err=%v", saved, err)
			}
			after := lookupEnrollment(t, store, ctx, link.Token, cookie)
			if !reflect.DeepEqual(after, before) || len(studentsFor(t, db, ctx, e.ID)) != 0 || changes != 0 {
				t.Fatalf("failed transaction changed draft/submission/expiry: before=%+v, after=%+v", before, after)
			}
			if err := db.WithContext(ctx).Exec("ALTER TABLE enrollments DROP CONSTRAINT test_link_rejection").Error; err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, link.Token, cookie, details, ""); err != nil || saved != Saved {
				t.Fatalf("retry failed: saved=%v, err=%v", saved, err)
			}
			completed := lookupEnrollment(t, store, ctx, link.Token, cookie)
			enrolled := studentsFor(t, db, ctx, e.ID)
			if completed == nil || completed.Details != details || completed.SubmittedAt == nil || changes != 1 ||
				len(enrolled) != 0 || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
				t.Fatalf("retry did not commit completion: enrollment=%+v, students=%+v", completed, enrolled)
			}
		})
	}
}

func TestSubmissionCancellationRace(t *testing.T) {
	db, store, ctx := testStore(t)
	link, e, cookie := createAndStart(t, store, ctx, staff, later)
	draft := students.Student{Details: students.Details{
		Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
	}}
	final := contactDetails("Final answer")
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, link.Token, cookie, field.Key, field.Value(draft)); err != nil || saved != Saved {
			t.Fatalf("draft %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	type result struct {
		saved SaveResult
		entry *EnrollmentEntry
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		saved, err := store.Submit(ctx, link.Token, cookie, final, "")
		results <- result{saved: saved, err: err}
	}()
	go func() {
		<-start
		entry, err := store.CancelEnrollment(ctx, e.ID, "canceler")
		results <- result{entry: entry, err: err}
	}()
	close(start)
	var submitted SaveResult
	var canceled *EnrollmentEntry
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.entry != nil {
			canceled = got.entry
		} else {
			submitted = got.saved
		}
	}
	var row Enrollment
	if err := db.WithContext(ctx).Where("id = ?", e.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, e.ID)
	if canceled == nil {
		t.Fatal("cancel did not return an entry")
	}
	switch {
	case row.SubmittedAt != nil:
		if submitted != Saved || row.CanceledAt != nil || row.CanceledBy != "" || row.Details != final ||
			len(enrolled) != 0 || canceled.Status != AwaitingPayment || row.ExpiresAt.Sub(*row.SubmittedAt) != 168*time.Hour {
			t.Fatalf("invalid submission-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	case row.CanceledAt != nil:
		if submitted != Unavailable || row.CanceledBy != "canceler" || row.Details != draft.Details ||
			len(enrolled) != 0 || canceled.Status != Canceled || row.ExpiresAt.Sub(later).Abs() > time.Microsecond {
			t.Fatalf("invalid cancellation-wins outcome: submitted=%v, row=%+v, students=%+v, cancel=%+v", submitted, row, enrolled, canceled)
		}
	default:
		t.Fatalf("race left neither completion nor cancellation: %+v", row)
	}
}

func TestCancelLinkAndEnrollment(t *testing.T) {
	db, store, ctx := testStore(t)
	l0, e0, c0 := createAndStart(t, store, ctx, staff, later)
	l1, e1, c1 := createAndStart(t, store, ctx, staff, later)
	const canceledName, activeName = "Canceled customer", "Still active"
	if ok, err := store.SaveDraft(ctx, l0.Token, c0, "name", canceledName); err != nil || ok != Saved {
		t.Fatalf("save 0: %v, %v", ok, err)
	}
	if ok, err := store.SaveDraft(ctx, l1.Token, c1, "name", activeName); err != nil || ok != Saved {
		t.Fatalf("save 1: %v, %v", ok, err)
	}

	t.Run("database failure returns no row", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if got, err := store.CancelEnrollment(cancelled, e0.ID, staff); err == nil || got != nil {
			t.Fatalf("enrollment=%+v, err=%v; want error and nil", got, err)
		}
		if got, err := store.CancelLink(cancelled, l0.ID, staff); err == nil || got != nil {
			t.Fatalf("link=%+v, err=%v; want error and nil", got, err)
		}
	})

	t.Run("anonymous actions are refused", func(t *testing.T) {
		if _, err := store.CreateLink(ctx, "", "", later); !errors.Is(err, errNoStaff) {
			t.Errorf("CreateLink: err = %v, want errNoStaff", err)
		}
		if got, err := store.CancelLink(ctx, l1.ID, ""); !errors.Is(err, errNoStaff) || got != nil {
			t.Errorf("CancelLink: entry = %+v, err = %v; want no row, errNoStaff", got, err)
		}
		if got, err := store.CancelEnrollment(ctx, e1.ID, ""); !errors.Is(err, errNoStaff) || got != nil {
			t.Errorf("CancelEnrollment: entry = %+v, err = %v; want no row, errNoStaff", got, err)
		}
	})

	// Cancel enrollment 0; repeats keep first audit; unknown/zero/overflow IDs return nil.
	var canceledAt *time.Time
	for _, tc := range []struct {
		name  string
		id    uint64
		staff string
		found bool
	}{
		{"issued enrollment", e0.ID, "alice", true},
		{"already canceled enrollment keeps its audit", e0.ID, "bob", true},
		{"zero ID", 0, staff, false},
		{"unknown enrollment", 999999, staff, false},
		{"overflow ID", 1 << 63, staff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.CancelEnrollment(ctx, tc.id, tc.staff)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.found {
				if got != nil {
					t.Fatalf("returned %+v for unknown ID", got)
				}
				return
			}
			if got == nil || got.ID != e0.ID || got.Status != Canceled || got.Name != canceledName ||
				got.CanceledBy != "alice" || got.Cancelable() {
				t.Fatalf("returned %+v; want canceled by alice", got)
			}
			var row Enrollment
			if err := db.WithContext(ctx).Where("id = ?", e0.ID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.CanceledAt == nil {
				t.Fatal("canceled enrollment has no persisted cancellation time")
			}
			if canceledAt == nil {
				canceledAt = row.CanceledAt
			}
			if !row.CanceledAt.Equal(*canceledAt) {
				t.Fatalf("canceled_at moved from %v to %v", *canceledAt, *row.CanceledAt)
			}
		})
	}

	// Canceled enrollment rejects writes, yet parent link l0 still accepts a NEW visitor!
	if saved, err := store.SaveDraft(ctx, l0.Token, c0, "name", "Must not revive"); err != nil || saved != Unavailable {
		t.Fatalf("canceled enrollment accepted draft: saved=%v, err=%v", saved, err)
	}
	fresh, freshCookie, err := store.StartEnrollment(ctx, l0.Token, c0)
	if err != nil || fresh == nil || fresh.ID == e0.ID || freshCookie == "" {
		t.Fatalf("start after enrollment cancel: fresh=%+v, err=%v", fresh, err)
	}

	// Active enrollment on l1 is untouched.
	if sess, err := store.LookupSession(ctx, l1.Token, c1); err != nil || sess == nil || !sess.Usable || sess.Enrollment.Name != activeName {
		t.Fatalf("canceling e0 affected l1: sess=%+v, err=%v", sess, err)
	}

	// CancelLink on l1 revokes l1 and makes unsubmitted enrollment e1 derive Canceled from l1, while submitting l1's sibling first preserves that sibling!
	e1b, c1b, err := store.StartEnrollment(ctx, l1.Token, "")
	if err != nil || e1b == nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, l1.Token, c1b, contactDetails("Submitted Sibling"), ""); err != nil || saved != Saved {
		t.Fatalf("submit sibling: saved=%v, err=%v", saved, err)
	}
	cl, err := store.CancelLink(ctx, l1.ID, "bob")
	if err != nil || cl == nil || cl.Status != LinkCanceled || cl.CanceledBy != "bob" {
		t.Fatalf("CancelLink: %+v, err=%v", cl, err)
	}
	// Repeat CancelLink keeps first audit.
	cl2, err := store.CancelLink(ctx, l1.ID, "carol")
	if err != nil || cl2 == nil || cl2.CanceledBy != "bob" {
		t.Fatalf("repeat CancelLink changed audit: %+v, err=%v", cl2, err)
	}
	// New visitor cannot start on l1.
	if newE, newC, err := store.StartEnrollment(ctx, l1.Token, ""); err != nil || newE != nil || newC != "" {
		t.Fatalf("StartEnrollment on canceled link succeeded: %+v, %q, %v", newE, newC, err)
	}
	listed := listByID(t, store, ctx)
	if listed[e1.ID].Status != Canceled || listed[e1.ID].CanceledBy != "bob" {
		t.Fatalf("unsubmitted child of canceled link: %+v", listed[e1.ID])
	}
	if listed[e1b.ID].Status != AwaitingPayment || listed[e1b.ID].CanceledBy != "" {
		t.Fatalf("submitted child of canceled link lost status: %+v", listed[e1b.ID])
	}
}

func TestExpiredLinksStopWorking(t *testing.T) {
	db, store, ctx := testStore(t)
	issuedExpired, err := store.CreateLink(ctx, staff, "", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if started, c, err := store.StartEnrollment(ctx, issuedExpired.Token, ""); err != nil || started != nil || c != "" {
		t.Fatalf("StartEnrollment on expired link: %+v, %q, %v", started, c, err)
	}

	midLink, midE, midCookie := createAndStart(t, store, ctx, staff, later)
	finLink, finE, finCookie := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.SaveDraft(ctx, midLink.Token, midCookie, "name", "Half done"); err != nil || saved != Saved {
		t.Fatalf("draft before expiry: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.Submit(ctx, finLink.Token, finCookie, contactDetails("Done final"), ""); err != nil || saved != Saved {
		t.Fatalf("submit before expiry: saved=%v, err=%v", saved, err)
	}
	if err := acceptAgreement(ctx, db, store, finLink.Token, finCookie, programStart); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, finE.ID, now)
	}); err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, finE.ID)
	if len(enrolled) != 1 {
		t.Fatalf("finished enrollment has %d students, want one", len(enrolled))
	}
	// Both pass their deadline. The draft stops working; the paid enrollment stays readable and takes no writes.
	if err := db.WithContext(ctx).Model(&Enrollment{}).Where("id IN ?", []uint64{midE.ID, finE.ID}).
		Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		l, c   string
		usable bool
	}{
		"mid-form":           {midLink.Token, midCookie, false},
		"paid past deadline": {finLink.Token, finCookie, true},
	} {
		t.Run(name, func(t *testing.T) {
			if sess, err := store.LookupSession(ctx, tc.l, tc.c); err != nil || sess == nil || sess.Usable != tc.usable {
				t.Errorf("lookup: sess=%+v, err=%v; want usable=%v", sess, err, tc.usable)
			}
			if saved, err := store.SaveDraft(ctx, tc.l, tc.c, "name", "After expiry"); err != nil || saved != Unavailable {
				t.Errorf("draft: saved=%v, err=%v; want refused", saved, err)
			}
			if saved, err := store.Submit(ctx, tc.l, tc.c, contactDetails("After expiry"), ""); err != nil || saved != Unavailable {
				t.Errorf("submit: saved=%v, err=%v; want refused", saved, err)
			}
		})
	}

	// CancelEnrollment returns the expired row as saved; it never turns into Canceled.
	if got, err := store.CancelEnrollment(ctx, midE.ID, "bob"); err != nil || got == nil || got.ID != midE.ID ||
		got.Status != Expired || got.Name != "Half done" || got.CanceledBy != "" {
		t.Fatalf("cancel expired enrollment: entry=%+v, err=%v; want expired with saved name", got, err)
	}
	listed := listByID(t, store, ctx)
	for _, tc := range []struct {
		name   string
		id     uint64
		status Status
		saved  string
	}{
		{"expiry keeps saved data", midE.ID, Expired, "Half done"},
		{"finished stays finished", finE.ID, Finished, "Done final"},
	} {
		got := listed[tc.id]
		if got.Status != tc.status || got.Name != tc.saved || got.CanceledBy != "" || got.Cancelable() ||
			got.ExpiresAt.After(time.Now()) || got.Trashed() != (tc.status == Expired) {
			t.Errorf("%s: listed %+v; want status %q, name %q, not canceled or cancelable", tc.name, got, tc.status, tc.saved)
		}
	}
	if after := studentsFor(t, db, ctx, finE.ID); !reflect.DeepEqual(after, enrolled) {
		t.Fatalf("source expiry changed enrollment: before=%+v, after=%+v", enrolled, after)
	}
}

// Open management pages refresh only when told: every committed customer or
// staff write must announce itself, and writes that change nothing must not.
func TestCommittedWritesAnnounceChanges(t *testing.T) {
	db, _, ctx := testStore(t)
	changes := 0
	store := New(db, func() { changes++ })
	step := func(name string, want int, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changes != want {
			t.Fatalf("%s: changes = %d, want %d", name, changes, want)
		}
	}

	first, err := store.CreateLink(ctx, staff, "", later)
	step("create link", 1, err)
	e1, c1, err := store.StartEnrollment(ctx, first.Token, "")
	step("first start", 2, err)
	_, _, err = store.StartEnrollment(ctx, first.Token, c1)
	step("resume session", 2, err)
	_, err = store.SaveDraft(ctx, first.Token, c1, "name", "Draft")
	step("draft", 3, err)
	_, err = store.SaveDraft(ctx, first.Token, c1, "name", "Draft")
	step("repeat draft", 3, err)
	_, err = store.Submit(ctx, first.Token, c1, contactDetails("Final answer"), "")
	step("submit", 4, err)
	_, err = store.Submit(ctx, first.Token, c1, contactDetails("Replay answer"), "")
	step("replayed submit", 4, err)
	_, err = store.CancelEnrollment(ctx, e1.ID, staff)
	step("cancel submitted enrollment", 4, err)
	_, e2, _ := createAndStart(t, store, ctx, staff, later)
	step("second create+start", 6, nil)
	_, err = store.CancelEnrollment(ctx, e2.ID, staff)
	step("cancel enrollment", 7, err)
	_, err = store.CancelLink(ctx, first.ID, staff)
	step("cancel link", 8, err)
	_, err = store.CancelLink(ctx, first.ID, staff)
	step("repeat cancel link", 8, err)
}

func TestDraftFieldIsolationAndConcurrentClearing(t *testing.T) {
	db, store, ctx := testStore(t)
	link, e, cookie := createAndStart(t, store, ctx, staff, later)
	otherLink, _, otherCookie := createAndStart(t, store, ctx, staff, later)
	initial := students.Student{Details: contactDetails("Original draft")}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, link.Token, cookie, field.Key, field.Value(initial)); err != nil || saved != Saved {
			t.Fatalf("seed %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	before := lookupEnrollment(t, store, ctx, link.Token, cookie)
	otherBefore := lookupEnrollment(t, store, ctx, otherLink.Token, otherCookie)
	type write struct{ key, value string }
	for _, step := range []struct {
		name   string
		writes []write
		want   students.Details
		status Status
	}{
		{
			name: "concurrent name and email", writes: []write{{"name", "Updated"}, {"email", "updated@example.com"}},
			want:   students.Details{Name: "Updated", Email: "updated@example.com", Phone: initial.Phone, Municipality: initial.Municipality, DateOfBirth: initial.DateOfBirth, Gender: initial.Gender},
			status: InProgress,
		},
		{
			name: "clear name while changing phone", writes: []write{{"name", ""}, {"phone", "+12025550123"}},
			want:   students.Details{Email: "updated@example.com", Phone: "+12025550123", Municipality: initial.Municipality, DateOfBirth: initial.DateOfBirth, Gender: initial.Gender},
			status: InProgress,
		},
		{
			name: "clear email while changing municipality", writes: []write{{"email", ""}, {"municipality", "Fuera de Puerto Rico"}},
			want:   students.Details{Phone: "+12025550123", Municipality: "Fuera de Puerto Rico", DateOfBirth: initial.DateOfBirth, Gender: initial.Gender},
			status: InProgress,
		},
		{
			name: "clear last fields", writes: []write{{"phone", ""}, {"municipality", ""}, {"date_of_birth", ""}, {"gender", ""}},
			want: students.Details{}, status: InProgress,
		},
	} {
		if !t.Run(step.name, func(t *testing.T) {
			type result struct {
				key   string
				saved SaveResult
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, len(step.writes))
			for _, change := range step.writes {
				go func() {
					<-start
					saved, err := store.SaveDraft(ctx, link.Token, cookie, change.key, change.value)
					results <- result{key: change.key, saved: saved, err: err}
				}()
			}
			close(start)
			for range step.writes {
				got := <-results
				if got.err != nil || got.saved != Saved {
					t.Errorf("save %s: result=%v, err=%v", got.key, got.saved, got.err)
				}
			}
			if t.Failed() {
				return
			}
			after := lookupEnrollment(t, store, ctx, link.Token, cookie)
			want := *before
			want.Details = step.want
			if after == nil || !reflect.DeepEqual(*after, want) {
				t.Fatalf("draft changed unrelated data: enrollment=%+v, want=%+v", after, want)
			}
			if entry := listByID(t, store, ctx)[e.ID]; entry.Status != step.status ||
				entry.Name != step.want.Name || entry.Phone != step.want.Phone {
				t.Fatalf("draft projection: %+v, want status=%v, details=%+v", entry, step.status, step.want)
			}
			otherAfter := lookupEnrollment(t, store, ctx, otherLink.Token, otherCookie)
			if !reflect.DeepEqual(otherAfter, otherBefore) {
				t.Fatalf("draft changed another enrollment: before=%+v, after=%+v", otherBefore, otherAfter)
			}
			if enrolled := studentsFor(t, db, ctx, e.ID); len(enrolled) != 0 {
				t.Fatalf("draft created students: %+v", enrolled)
			}
		}) {
			return
		}
	}
}

func TestDraftRejectsUnknownFieldBeforeSQL(t *testing.T) {
	changes := 0
	store := New(nil, func() { changes++ })
	token := base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes))
	for _, key := range []string{"", "Name", "email_confirm", "session_hash", "submitted_at", "name; DROP TABLE enrollments"} {
		t.Run(key, func(t *testing.T) {
			if saved, err := store.SaveDraft(t.Context(), token, token, key, "Must not save"); saved != Unavailable ||
				!errors.Is(err, students.ErrUnknownField) {
				t.Fatalf("unknown field reached persistence: result=%v, err=%v", saved, err)
			}
		})
	}
	if changes != 0 {
		t.Fatalf("rejected fields announced %d changes", changes)
	}
}

func TestDraftSubmissionRaceKeepsFinalSnapshot(t *testing.T) {
	for _, field := range students.Fields() {
		t.Run(field.Key, func(t *testing.T) {
			db, store, ctx := testStore(t)
			link, e, cookie := createAndStart(t, store, ctx, staff, later)
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			final := contactDetails("Final answer")
			for _, seed := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, link.Token, cookie, seed.Key, seed.Value(draft)); err != nil || saved != Saved {
					t.Fatalf("seed %s: result=%v, err=%v", seed.Key, saved, err)
				}
			}
			type result struct {
				draft bool
				saved SaveResult
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			go func() {
				<-start
				saved, err := store.SaveDraft(ctx, link.Token, cookie, field.Key, "")
				results <- result{draft: true, saved: saved, err: err}
			}()
			go func() {
				<-start
				saved, err := store.Submit(ctx, link.Token, cookie, final, "")
				results <- result{saved: saved, err: err}
			}()
			close(start)
			for range 2 {
				got := <-results
				if got.err != nil || (!got.draft && got.saved != Saved) ||
					(got.draft && got.saved != Saved && got.saved != AlreadySubmitted) {
					t.Errorf("draft=%v: result=%v, err=%v", got.draft, got.saved, got.err)
				}
			}
			if t.Failed() {
				return
			}
			completed := lookupEnrollment(t, store, ctx, link.Token, cookie)
			enrolled := studentsFor(t, db, ctx, e.ID)
			if completed == nil || completed.Details != final || completed.SubmittedAt == nil ||
				completed.CanceledAt != nil || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour ||
				len(enrolled) != 0 {
				t.Fatalf("racing draft changed final snapshot: enrollment=%+v, students=%+v", completed, enrolled)
			}
			if saved, err := store.SaveDraft(ctx, link.Token, cookie, field.Key, ""); err != nil || saved != AlreadySubmitted {
				t.Fatalf("late clear: result=%v, err=%v", saved, err)
			}
			if saved, err := store.Submit(ctx, link.Token, cookie, draft.Details, ""); err != nil || saved != AlreadySubmitted {
				t.Fatalf("final replay: result=%v, err=%v", saved, err)
			}
			after := lookupEnrollment(t, store, ctx, link.Token, cookie)
			if !reflect.DeepEqual(after, completed) || !reflect.DeepEqual(studentsFor(t, db, ctx, e.ID), enrolled) {
				t.Fatalf("replay changed racing completion: before=%+v, after=%+v", completed, after)
			}
		})
	}
}

func TestAcceptTermsRequiresContactAndCurrentDisplayedDate(t *testing.T) {
	db, store, ctx := testStore(t)
	link, e, cookie := createAndStart(t, store, ctx, staff, later)
	const firstDate, currentDate = "2026-11-10", "2026-11-17"
	options := settings.New(db)
	if err := options.SetProgramStartDate(ctx, firstDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"unopened", "draft"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "draft" {
				if saved, err := store.SaveDraft(ctx, link.Token, cookie, "name", "Draft contact"); err != nil || saved != Saved {
					t.Fatalf("draft: saved=%v, err=%v", saved, err)
				}
			}
			before := lookupEnrollment(t, store, ctx, link.Token, cookie)
			if got, err := store.AcceptTerms(ctx, link.Token, cookie, firstDate); err != nil || got != nil {
				t.Fatalf("accept before submission: enrollment=%+v, err=%v", got, err)
			}
			if after := lookupEnrollment(t, store, ctx, link.Token, cookie); !reflect.DeepEqual(after, before) ||
				len(studentsFor(t, db, ctx, e.ID)) != 0 {
				t.Fatalf("premature agreement changed contact: enrollment=%+v", after)
			}
		})
	}
	if saved, err := store.Submit(ctx, link.Token, cookie, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	before := lookupEnrollment(t, store, ctx, link.Token, cookie)
	enrolled := studentsFor(t, db, ctx, e.ID)
	writes := 0
	store = New(db, func() { writes++ })
	if err := options.SetProgramStartDate(ctx, currentDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{"", "not-a-date", "2026-02-30", firstDate} {
		t.Run("reject displayed "+shown, func(t *testing.T) {
			if got, err := store.AcceptTerms(ctx, link.Token, cookie, shown); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("stale acceptance: enrollment=%+v, err=%v", got, err)
			}
		})
	}
	if after := lookupEnrollment(t, store, ctx, link.Token, cookie); !reflect.DeepEqual(after, before) || writes != 0 {
		t.Fatalf("rejected agreement changed enrollment: %+v, writes=%d", after, writes)
	}
	accepted, err := store.AcceptTerms(ctx, link.Token, cookie, currentDate)
	if err != nil || accepted == nil || accepted.TermsAcceptedAt == nil || accepted.ProgramStartDate != currentDate || writes != 1 {
		t.Fatalf("accept: enrollment=%+v, writes=%d, err=%v", accepted, writes, err)
	}
	want := *before
	want.TermsAcceptedAt, want.ProgramStartDate = accepted.TermsAcceptedAt, currentDate
	if !reflect.DeepEqual(*accepted, want) || accepted.TermsAcceptedAt.Before(*accepted.SubmittedAt) ||
		!reflect.DeepEqual(studentsFor(t, db, ctx, e.ID), enrolled) {
		t.Fatalf("acceptance changed submission or enrollment: got=%+v, want=%+v", accepted, want)
	}
	if persisted := lookupEnrollment(t, store, ctx, link.Token, cookie); !reflect.DeepEqual(persisted, accepted) {
		t.Fatalf("acceptance not persisted: %+v", persisted)
	}
	if err := options.SetProgramStartDate(ctx, "2027-01-05", staff); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{currentDate, "2027-01-05", "", "invalid replay"} {
		if got, err := store.AcceptTerms(ctx, link.Token, cookie, shown); err != nil || !reflect.DeepEqual(got, accepted) || writes != 1 {
			t.Fatalf("accepted replay changed agreement: %+v, writes=%d, err=%v", got, writes, err)
		}
	}
	if saved, err := store.Submit(ctx, link.Token, cookie, contactDetails("Replacement contact"), ""); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted replay: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.SaveDraft(ctx, link.Token, cookie, "name", "Replacement draft"); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted draft: saved=%v, err=%v", saved, err)
	}
	if after := lookupEnrollment(t, store, ctx, link.Token, cookie); !reflect.DeepEqual(after, accepted) || writes != 1 {
		t.Fatalf("contact replay changed agreement: %+v, writes=%d", after, writes)
	}
}

func TestAcceptTermsRejectsUnconfiguredStartDate(t *testing.T) {
	db, store, ctx := testStore(t)
	link, _, cookie := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link.Token, cookie, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	for _, stage := range []string{"missing settings row", "empty date"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "empty date" {
				if err := settings.New(db).SetLinkLifetimeDays(ctx, 7, staff); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := store.AcceptTerms(ctx, link.Token, cookie, "2026-11-10"); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("unconfigured date accepted: %+v, err=%v", got, err)
			}
		})
	}
}

func TestAcceptTermsObservesConcurrentStartDateChange(t *testing.T) {
	db, store, ctx := testStore(t)
	link, _, cookie := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link.Token, cookie, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	blocker := db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	acceptCtx, cancelAccept := context.WithCancel(ctx)
	type result struct {
		e   *Enrollment
		err error
	}
	var results chan result
	defer func() {
		blocker.Rollback()
		cancelAccept()
		if results != nil {
			for range results {
			}
		}
	}()
	if err := settings.New(blocker).SetProgramStartDate(ctx, "2026-11-17", staff); err != nil {
		t.Fatal(err)
	}
	results = make(chan result, 1)
	go func() {
		defer close(results)
		e, err := store.AcceptTerms(acceptCtx, link.Token, cookie, "2026-11-10")
		results <- result{e: e, err: err}
	}()
	testdb.AwaitBlocked(t, ctx, db, blocker, results)
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.e != nil || !errors.Is(got.err, ErrProgramStartChanged) {
		t.Fatalf("acceptance ignored committed date change: e=%+v, err=%v", got.e, got.err)
	}
	if persisted := lookupEnrollment(t, store, ctx, link.Token, cookie); persisted == nil || persisted.TermsAcceptedAt != nil || persisted.ProgramStartDate != "" {
		t.Fatalf("stale agreement persisted: %+v", persisted)
	}
}

func TestAgreementWritesRejectUnusableTokens(t *testing.T) {
	db, store, ctx := testStore(t)
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	type pair struct{ l, c string }
	var unusable []pair
	for _, stage := range []string{"expired submission", "expired acceptance", "canceled acceptance"} {
		l, e, c := createAndStart(t, store, ctx, staff, later)
		if saved, err := store.Submit(ctx, l.Token, c, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
			t.Fatalf("submit: saved=%v, err=%v", saved, err)
		}
		if stage != "expired submission" {
			if got, err := store.AcceptTerms(ctx, l.Token, c, "2026-11-10"); err != nil || got == nil {
				t.Fatalf("accept: %+v, err=%v", got, err)
			}
		}
		column := "expires_at"
		if stage == "canceled acceptance" {
			column = "canceled_at"
		}
		if err := db.WithContext(ctx).Model(&Enrollment{}).Where("id = ?", e.ID).Update(column, time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		unusable = append(unusable, pair{l.Token, c})
	}
	cl, ce, cc := createAndStart(t, store, ctx, staff, later)
	if got, err := store.CancelEnrollment(ctx, ce.ID, staff); err != nil || got == nil || got.Status != Canceled {
		t.Fatalf("cancel unfinished contact: entry=%+v, err=%v", got, err)
	}
	unusable = append(unusable, pair{cl.Token, cc}, pair{"", ""}, pair{"malformed", "malformed"},
		pair{strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)), cc},
		pair{base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)), cc})

	var rowsBefore []Enrollment
	var studentsBefore []students.Student
	if err := db.WithContext(ctx).Order("id").Find(&rowsBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	store = New(db, func() { writes++ })
	for _, p := range unusable {
		if got, err := store.AcceptTerms(ctx, p.l, p.c, "2026-11-10"); err != nil || got != nil {
			t.Fatalf("unusable acceptance: %+v, err=%v", got, err)
		}
	}
	var rowsAfter []Enrollment
	var studentsAfter []students.Student
	if err := db.WithContext(ctx).Order("id").Find(&rowsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if writes != 0 || !reflect.DeepEqual(rowsAfter, rowsBefore) || !reflect.DeepEqual(studentsAfter, studentsBefore) {
		t.Fatalf("unusable agreement writes changed retained rows: writes=%d", writes)
	}
}

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{}, &Enrollment{}, &students.Student{}, &settings.Settings{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, New(db, nil), ctx
}

func createAndStart(t *testing.T, store *Store, ctx context.Context, by string, exp time.Time) (LinkEntry, *Enrollment, string) {
	t.Helper()
	link, err := store.CreateLink(ctx, by, "", exp)
	if err != nil {
		t.Fatal(err)
	}
	e, cookie, err := store.StartEnrollment(ctx, link.Token, "")
	if err != nil || e == nil || cookie == "" {
		t.Fatalf("start enrollment: %+v, %q, %v", e, cookie, err)
	}
	return link, e, cookie
}

func lookupEnrollment(t *testing.T, store *Store, ctx context.Context, linkToken, cookieToken string) *Enrollment {
	t.Helper()
	sess, err := store.LookupSession(ctx, linkToken, cookieToken)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		return nil
	}
	return sess.Enrollment
}

func listByID(t *testing.T, store *Store, ctx context.Context) map[uint64]EnrollmentEntry {
	t.Helper()
	groups, err := store.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[uint64]EnrollmentEntry)
	for _, g := range groups {
		for _, e := range g.Enrollments {
			byID[e.ID] = e
		}
	}
	return byID
}

func digestOf(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func studentsFor(t *testing.T, db *gorm.DB, ctx context.Context, enrollmentID uint64) []students.Student {
	t.Helper()
	var enrolled []students.Student
	if err := db.WithContext(ctx).Where("enrollment_id = ?", enrollmentID).Find(&enrolled).Error; err != nil {
		t.Fatal(err)
	}
	return enrolled
}

func contactDetails(name string) students.Details {
	return students.Details{
		Name: name, Email: "student@example.com", Phone: "+17872345678", Municipality: "San Juan", DateOfBirth: "2005-06-15", Gender: "Female",
	}
}

// programStart is the agreement start date tests accept before Enroll.
const programStart = "2026-11-10"

// acceptAgreement configures date as the program start and accepts the submitted agreement.
func acceptAgreement(ctx context.Context, db *gorm.DB, store *Store, linkToken, cookieToken, date string) error {
	if err := settings.New(db).SetProgramStartDate(ctx, date, staff); err != nil {
		return err
	}
	if accepted, err := store.AcceptTerms(ctx, linkToken, cookieToken, date); err != nil || accepted == nil {
		return fmt.Errorf("accept agreement: %+v, err=%v", accepted, err)
	}
	return nil
}

func TestPayBeforeStudentInvariants(t *testing.T) {
	db, store, ctx := testStore(t)

	// (a) after Submit with a PIN, students has 0 rows, the enrollment row holds details and PIN, ListGroups reports AwaitingPayment
	link1, e1, c1 := createAndStart(t, store, ctx, staff, later)
	details1 := contactDetails("Customer One")
	details1.Phone = "+17875551111"
	const pin = "789012"
	saved, err := store.Submit(ctx, link1.Token, c1, details1, pin)
	if err != nil || saved != Saved {
		t.Fatalf("submit 1: saved=%v, err=%v", saved, err)
	}
	var count int64
	db.Model(&students.Student{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 students after submit, got %d", count)
	}
	row1 := lookupEnrollment(t, store, ctx, link1.Token, c1)
	if row1 == nil || row1.Details != details1 || row1.Pin != pin || row1.PaidAt != nil || row1.SubmittedAt == nil {
		t.Fatalf("enrollment 1 state unexpected: %+v", row1)
	}
	entry1, ok := listByID(t, store, ctx)[e1.ID]
	if !ok || entry1.Status != AwaitingPayment || entry1.Name != details1.Name || entry1.Phone != details1.Phone {
		t.Fatalf("enrollment 1 list entry unexpected: %+v", entry1)
	}

	// (b) a second enrollment submitting the same PIN gets PinTaken and stays unsubmitted
	link2, _, c2 := createAndStart(t, store, ctx, staff, later)
	details2 := contactDetails("Customer Two")
	saved2, err := store.Submit(ctx, link2.Token, c2, details2, pin)
	if err != nil || saved2 != PinTaken {
		t.Fatalf("submit 2 with duplicate pin: saved=%v, err=%v; want PinTaken", saved2, err)
	}
	row2 := lookupEnrollment(t, store, ctx, link2.Token, c2)
	if row2 == nil || row2.SubmittedAt != nil || row2.Pin != "" {
		t.Fatalf("enrollment 2 should stay unsubmitted: %+v", row2)
	}

	// (c) Enroll in a tx creates exactly one student with details, PIN, created_at == paidAt, Básico,
	// and the accepted start date's month as cohort even after staff move the date;
	// sets enrollments.paid_at, clears enrollments.pin, status becomes Finished; second Enroll returns error
	if err := acceptAgreement(ctx, db, store, link1.Token, c1, programStart); err != nil {
		t.Fatal(err)
	}
	if err := settings.New(db).SetProgramStartDate(ctx, "2027-01-05", staff); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, e1.ID, paidAt)
	}); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}
	var student students.Student
	if err := db.Where("enrollment_id = ?", e1.ID).Take(&student).Error; err != nil {
		t.Fatalf("find enrolled student: %v", err)
	}
	if student.Details != details1 || student.Pin != pin || !student.CreatedAt.Equal(paidAt) ||
		student.Group != "Básico" || student.Cohort != "2026-11" {
		t.Fatalf("enrolled student mismatch: %+v", student)
	}
	row1After := lookupEnrollment(t, store, ctx, link1.Token, c1)
	if row1After == nil || row1After.Pin != "" || row1After.PaidAt == nil || !row1After.PaidAt.Equal(paidAt) {
		t.Fatalf("enrollment 1 after enroll: %+v", row1After)
	}
	if listByID(t, store, ctx)[e1.ID].Status != Finished {
		t.Fatalf("entry status after enroll = %v; want Finished", listByID(t, store, ctx)[e1.ID].Status)
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, e1.ID, paidAt)
	})
	if err == nil {
		t.Fatal("expected second Enroll to fail")
	}
	db.Model(&students.Student{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 student after second enroll, got %d", count)
	}

	// (c2) Enroll cleared enrollment 1's PIN, but the Student holds it: another enrollment cannot submit it
	link4, _, c4 := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link4.Token, c4, contactDetails("Customer Four"), pin); err != nil || saved != PinTaken {
		t.Fatalf("submit with an enrolled student's pin: saved=%v, err=%v; want PinTaken", saved, err)
	}
	if row4 := lookupEnrollment(t, store, ctx, link4.Token, c4); row4 == nil || row4.SubmittedAt != nil || row4.Pin != "" {
		t.Fatalf("enrollment 4 should stay unsubmitted: %+v", row4)
	}

	// (d) a submitted unpaid enrollment past expires_at is Expired and Trashed(), but the same row with PaidAt set is Finished and not trashed
	link3, _, c3 := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link3.Token, c3, contactDetails("Customer Three"), "112233"); err != nil || saved != Saved {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	row3 := lookupEnrollment(t, store, ctx, link3.Token, c3)
	row3.ExpiresAt = past
	parent3 := Link{ExpiresAt: later}
	e3Unpaid := row3.entry(parent3, time.Now())
	if e3Unpaid.Status != Expired || !e3Unpaid.Trashed() {
		t.Fatalf("unpaid expired enrollment status=%v, trashed=%v; want Expired, true", e3Unpaid.Status, e3Unpaid.Trashed())
	}
	now := time.Now()
	row3.PaidAt = &now
	e3Paid := row3.entry(parent3, now)
	if e3Paid.Status != Finished || e3Paid.Trashed() {
		t.Fatalf("paid expired enrollment status=%v, trashed=%v; want Finished, false", e3Paid.Status, e3Paid.Trashed())
	}
}

func TestRenameLink(t *testing.T) {
	db, _, ctx := testStore(t)
	changes := 0
	store := New(db, func() { changes++ })
	created, err := store.CreateLink(ctx, staff, "Initial", later)
	if err != nil {
		t.Fatal(err)
	}
	changes = 0

	if _, err := store.RenameLink(ctx, created.ID, "Valid", ""); !errors.Is(err, errNoStaff) {
		t.Fatalf("empty staff: err=%v, want errNoStaff", err)
	}
	for _, bad := range []string{"", "   ", "\t\n", strings.Repeat("a", maxLinkNameRunes+1), "bad\x00name"} {
		if got, err := store.RenameLink(ctx, created.ID, bad, staff); !errors.Is(err, ErrInvalidLinkName) || got != nil {
			t.Fatalf("bad name %q: got=%+v, err=%v", bad, got, err)
		}
	}
	for _, id := range []uint64{0, 999999, 1 << 63} {
		if got, err := store.RenameLink(ctx, id, "Ignored", staff); err != nil || got != nil {
			t.Fatalf("missing id %d: got=%+v, err=%v", id, got, err)
		}
	}
	// No-op rename changes nothing and announces nothing.
	if got, err := store.RenameLink(ctx, created.ID, "  Initial  ", staff); err != nil || got == nil || got.Name != "Initial" || changes != 0 {
		t.Fatalf("no-op rename: got=%+v, changes=%d, err=%v", got, changes, err)
	}
	renamed, err := store.RenameLink(ctx, created.ID, "  Fall Cohort 2026  ", staff)
	if err != nil || renamed == nil || renamed.Name != "Fall Cohort 2026" || changes != 1 {
		t.Fatalf("rename: got=%+v, changes=%d, err=%v", renamed, changes, err)
	}
}

func TestCancelEnrollmentTx(t *testing.T) {
	db, store, ctx := testStore(t)
	link, e1, c1 := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link.Token, c1, contactDetails("Submitted"), "654321"); err != nil || saved != Saved {
		t.Fatalf("submit 1: %v, %v", saved, err)
	}
	var got *EnrollmentEntry
	var changed bool
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		got, changed, err = CancelEnrollmentTx(tx, e1.ID, "admin")
		return err
	}); err != nil || !changed || got == nil || got.Status != Canceled || got.HasPin || got.CanceledBy != "admin" {
		t.Fatalf("CancelEnrollmentTx awaiting payment: got=%+v, changed=%v, err=%v", got, changed, err)
	}

	// Cleared PIN can be reused immediately.
	link2, e2, c2 := createAndStart(t, store, ctx, staff, later)
	if saved, err := store.Submit(ctx, link2.Token, c2, contactDetails("Expired Pin"), "654321"); err != nil || saved != Saved {
		t.Fatalf("submit 2 with freed pin: saved=%v, err=%v", saved, err)
	}
	if err := db.WithContext(ctx).Model(&Enrollment{}).Where("id = ?", e2.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		got, changed, err = CancelEnrollmentTx(tx, e2.ID, "admin")
		return err
	}); err != nil || !changed || got == nil || got.Status != Expired || got.HasPin || got.CanceledAt != nil {
		t.Fatalf("CancelEnrollmentTx expired pin: got=%+v, changed=%v, err=%v", got, changed, err)
	}
}
