//go:build integration

package links

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

// staff is the acting account for tests that aren't about the audit trail.
const staff = "tester"

// later keeps test links usable for any test's duration.
var later = time.Now().Add(time.Hour)

func TestIssuedTokenBoundary(t *testing.T) {
	db, store, ctx := testStore(t)

	first, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("only the token digest is stored", func(t *testing.T) {
		if first.ID != hex.EncodeToString(digestOf(first.Token)) {
			t.Fatalf("entry ID %q does not name the token digest", first.ID)
		}
		var row map[string]any
		if err := db.WithContext(ctx).Table("links").Where("token_hash = ?", digestOf(first.Token)).Take(&row).Error; err != nil {
			t.Fatalf("issued token has no persisted SHA-256 digest: %v", err)
		}
		for column, value := range row {
			var stored []byte
			switch v := value.(type) {
			case string:
				stored = []byte(v)
			case []byte:
				stored = v
			}
			if bytes.Contains(stored, []byte(first.Token)) {
				t.Fatalf("column %s stores the usable token", column)
			}
		}
	})

	tampered := first.Token[:len(first.Token)-1] + "A"
	if tampered == first.Token {
		tampered = first.Token[:len(first.Token)-1] + "B"
	}
	cases := []struct {
		name  string
		token string
		valid bool
	}{
		{"earlier link remains valid", first.Token, true},
		{"newer link is valid", second.Token, true},
		{"unissued token", base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)), false},
		{"tampered token", tampered, false},
		{"missing token", "", false},
		{"SQL-like input", strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, err := store.Open(ctx, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if valid := link != nil; valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
			if !tc.valid {
				for _, save := range []func() (SaveResult, error){
					func() (SaveResult, error) { return store.SaveDraft(ctx, tc.token, "name", "Must not save") },
					func() (SaveResult, error) { return store.Submit(ctx, tc.token, contactDetails("Must not save"), "") },
				} {
					if result, err := save(); err != nil || result != Unavailable {
						t.Fatalf("invalid token write: result=%v, err=%v", result, err)
					}
				}
			}
		})
	}

	t.Run("failed persistence does not issue a token", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		entry, err := store.Create(cancelled, staff, later)
		if err == nil || entry != (Entry{}) {
			t.Fatal("cancelled database write must return an error and no usable link")
		}
		for _, save := range []func() (SaveResult, error){
			func() (SaveResult, error) { return store.SaveDraft(cancelled, first.Token, "name", "Must not save") },
			func() (SaveResult, error) {
				return store.Submit(cancelled, first.Token, contactDetails("Must not save"), "")
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
	link, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	details := contactDetails("Ana Pérez")
	refused := func(save func() (SaveResult, error)) func() error {
		return func() error {
			if saved, err := save(); err != nil || saved != AlreadySubmitted {
				return fmt.Errorf("submitted form replay: saved=%v, err=%v", saved, err)
			}
			return nil
		}
	}
	lookedUp := func() error {
		// Open's first-visit update scopes by the returned digest.
		if found, err := store.Lookup(ctx, link.Token); err != nil || found == nil || !bytes.Equal(found.TokenHash, digestOf(link.Token)) {
			return fmt.Errorf("lookup did not return the usable link: link=%+v, err=%v", found, err)
		}
		return nil
	}
	// Open returns the first visit immediately and preserves it on later reads.
	var firstVisit *time.Time
	opened := func() error {
		found, err := store.Open(ctx, link.Token)
		if err != nil || found == nil || found.OpenedAt == nil {
			return fmt.Errorf("open did not return its first visit: link=%+v, err=%v", found, err)
		}
		if firstVisit == nil {
			firstVisit = found.OpenedAt
		} else if !found.OpenedAt.Equal(*firstVisit) {
			return fmt.Errorf("open moved first visit from %v to %v", *firstVisit, found.OpenedAt)
		}
		return nil
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
		{"issued", func() error { return nil }, NotOpened, ""},
		{"looked up before opening", lookedUp, NotOpened, ""},
		{"opened", opened, Opened, ""},
		{"reopened", opened, Opened, ""},
		{"empty autosave", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "") }), Opened, ""},
		{"autosaved", saved(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "Ana") }), InProgress, "Ana"},
		{"reopened draft", opened, InProgress, "Ana"},
		{"submitted", saved(func() (SaveResult, error) { return store.Submit(ctx, link.Token, details, "") }), AwaitingPayment, details.Name},
		{"looked up after submit", lookedUp, AwaitingPayment, details.Name},
		{"autosave after submit is refused", refused(func() (SaveResult, error) { return store.SaveDraft(ctx, link.Token, "name", "Changed after submit") }), AwaitingPayment, details.Name},
		{"second submit is refused", refused(func() (SaveResult, error) {
			return store.Submit(ctx, link.Token, contactDetails("Changed after submit"), "")
		}), AwaitingPayment, details.Name},
		{"cancel cannot revoke submitted link", func() error { _, err := store.Cancel(ctx, link.ID, staff); return err }, AwaitingPayment, details.Name},
		{"agreement accepted", func() error { return acceptAgreement(ctx, db, store, link.Token, programStart) }, AwaitingPayment, details.Name},
		{"enrolled on payment", func() error {
			return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return Enroll(tx, digestOf(link.Token), time.Now().UTC())
			})
		}, Finished, details.Name},
		{"finished link still opens success", lookedUp, Finished, details.Name},
	}
	// The first visit is recorded once; no later step may move it.
	for _, step := range steps {
		if !t.Run(step.name, func(t *testing.T) {
			if err := step.act(); err != nil {
				t.Fatal(err)
			}
			listed := listByID(t, store, ctx)
			got := listed[link.ID]
			if got.Status != step.status || got.Name != step.saved || got.Token != "" {
				t.Fatalf("listed %+v, want status %q, name %q, no token", got, step.status, step.saved)
			}
			if listed[other.ID].Status != NotOpened {
				t.Fatalf("other link changed to %q", listed[other.ID].Status)
			}
			var row Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(link.Token)).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if firstVisit != nil && (row.OpenedAt == nil || !row.OpenedAt.Equal(*firstVisit)) {
				t.Fatalf("first visit %v changed to %v", *firstVisit, row.OpenedAt)
			}
			firstVisit = row.OpenedAt
			enrolled := studentsFor(t, db, ctx, link.Token)
			if step.status == Finished {
				if len(enrolled) != 1 || enrolled[0].Details != details || row.Details != details || got.Phone != details.Phone || got.PaidAt == nil ||
					!enrolled[0].CreatedAt.Equal(*got.PaidAt) || row.CanceledAt != nil || row.CanceledBy != "" ||
					got.Cancelable() || got.Trashed() {
					t.Fatalf("completion changed or lost enrollment: entry=%+v, row=%+v, students=%+v", got, row, enrolled)
				}
			} else if step.status == AwaitingPayment {
				if len(enrolled) != 0 || row.Details != details || got.Phone != details.Phone || got.SubmittedAt == nil ||
					got.PaidAt != nil || got.Cancelable() || got.Trashed() {
					t.Fatalf("awaiting payment state invalid: entry=%+v, row=%+v, students=%+v", got, row, enrolled)
				}
			} else if len(enrolled) != 0 || row.SubmittedAt != nil {
				t.Fatalf("unfinished form enrolled a student: row=%+v, students=%+v", row, enrolled)
			}
		}) {
			return
		}
	}

	// Enrollment retains its source even when staff independently edit students.
	digest := digestOf(link.Token)
	if err := db.WithContext(ctx).Where("token_hash = ?", digest).Delete(&Link{}).Error; err == nil {
		t.Fatal("student enrollment did not restrict deletion of its source link")
	}
	replacementDigest := digestOf("unused replacement source link hash")
	if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digest).
		Update("token_hash", replacementDigest).Error; err == nil {
		t.Fatal("student enrollment did not restrict changing its source link hash")
	}
}

func TestOpenObservesConcurrentCancellation(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	blocker := db.WithContext(ctx).Begin()
	if blocker.Error != nil {
		t.Fatal(blocker.Error)
	}
	openCtx, cancelOpen := context.WithCancel(ctx)
	type result struct {
		link *Link
		err  error
	}
	var results chan result
	defer func() {
		blocker.Rollback()
		cancelOpen()
		if results != nil {
			for range results {
			}
		}
	}()
	canceled, err := New(blocker, nil).Cancel(ctx, issued.ID, "canceler")
	if err != nil || canceled == nil || canceled.Status != Canceled {
		t.Fatalf("hold cancellation: entry=%+v, err=%v", canceled, err)
	}

	results = make(chan result, 1)
	go func() {
		defer close(results)
		opened, err := store.Open(openCtx, issued.Token)
		results <- result{link: opened, err: err}
	}()
	testdb.AwaitBlocked(t, ctx, db, blocker, results)
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.err != nil {
		t.Fatalf("Open after cancellation: %v", got.err)
	}
	var row Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if got.link != nil || row.CanceledAt == nil || row.CanceledBy != "canceler" ||
		row.OpenedAt != nil || row.SubmittedAt != nil {
		t.Fatalf("Open ignored committed cancellation: link=%+v, row=%+v", got.link, row)
	}
}

func TestConcurrentSubmissionSubmitsOnce(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, issued.Token, "name", "Draft only"); err != nil || saved != Saved {
		t.Fatalf("draft: saved=%v, err=%v", saved, err)
	}
	if enrolled := studentsFor(t, db, ctx, issued.Token); len(enrolled) != 0 {
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
			saved, err := store.Submit(ctx, issued.Token, details, "")
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
	completed, err := store.Lookup(ctx, issued.Token)
	enrolled := studentsFor(t, db, ctx, issued.Token)
	if err != nil || winner == (students.Details{}) || completed == nil || completed.Details != winner || completed.SubmittedAt == nil ||
		len(enrolled) != 0 || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
		t.Fatalf("completion not atomic: winner=%+v, link=%+v, students=%+v, err=%v", winner, completed, enrolled, err)
	}
}

// A PIN belongs to one submitted link at a time: of two links racing for it, one submits and the other stays open with its draft, free to pick another PIN.
func TestConcurrentSubmissionsCannotShareAPin(t *testing.T) {
	db, store, ctx := testStore(t)
	tokens := make([]string, 2)
	for i := range tokens {
		issued, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := store.SaveDraft(ctx, issued.Token, "name", fmt.Sprintf("Draft %d", i)); err != nil || saved != Saved {
			t.Fatalf("draft: saved=%v, err=%v", saved, err)
		}
		tokens[i] = issued.Token
	}
	start := make(chan struct{})
	results := make(chan SaveResult, len(tokens))
	for _, token := range tokens {
		go func() {
			<-start
			saved, err := store.Submit(ctx, token, contactDetails("Final answer"), "012345")
			if err != nil {
				t.Error(err)
			}
			results <- saved
		}()
	}
	close(start)
	outcomes := map[SaveResult]int{}
	for range tokens {
		outcomes[<-results]++
	}
	if outcomes[Saved] != 1 || outcomes[PinTaken] != 1 {
		t.Fatalf("outcomes = %v, want one Saved and one PinTaken", outcomes)
	}
	var holders int64
	if err := db.WithContext(ctx).Model(&Link{}).Where("pin = ?", "012345").Count(&holders).Error; err != nil || holders != 1 {
		t.Fatalf("links holding the PIN = %d, err=%v; want 1", holders, err)
	}
	for _, token := range tokens {
		link, err := store.Lookup(ctx, token)
		if err != nil || link == nil {
			t.Fatalf("lookup: %v, %v", link, err)
		}
		if link.SubmittedAt != nil {
			continue
		}
		if !strings.HasPrefix(link.Name, "Draft ") {
			t.Fatalf("losing link lost its draft: %+v", link.Details)
		}
		if saved, err := store.Submit(ctx, token, contactDetails("Second try"), "543210"); err != nil || saved != Saved {
			t.Fatalf("losing link could not take another PIN: saved=%v, err=%v", saved, err)
		}
	}
}

func TestSubmissionFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conflict   bool
		constraint string
	}{
		{"link UPDATE check", false, "CHECK (name <> 'Rejected submission')"},
		{"deferred commit constraint", true, "UNIQUE (name) DEFERRABLE INITIALLY DEFERRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, ctx := testStore(t)
			details := contactDetails("Rejected submission")
			if tc.conflict {
				existing, err := store.Create(ctx, staff, later)
				if err != nil {
					t.Fatal(err)
				}
				if saved, err := store.Submit(ctx, existing.Token, details, ""); err != nil || saved != Saved {
					t.Fatalf("seed existing student: saved=%v, err=%v", saved, err)
				}
			}
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			for _, field := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(draft)); err != nil || saved != Saved {
					t.Fatalf("save %s draft: saved=%v, err=%v", field.Key, saved, err)
				}
			}
			before, err := store.Lookup(ctx, issued.Token)
			if err != nil || before == nil {
				t.Fatalf("load draft: link=%+v, err=%v", before, err)
			}
			// Constraint SQL is fixed test data, scoped to this test's schema.
			if err := db.WithContext(ctx).Exec("ALTER TABLE links ADD CONSTRAINT test_link_rejection " + tc.constraint).Error; err != nil {
				t.Fatal(err)
			}
			changes := 0
			store = New(db, func() { changes++ })
			if saved, err := store.Submit(ctx, issued.Token, details, ""); err == nil || saved != Unavailable {
				t.Fatalf("failed transaction reported success: saved=%v, err=%v", saved, err)
			}
			after, err := store.Lookup(ctx, issued.Token)
			if err != nil || !reflect.DeepEqual(after, before) || len(studentsFor(t, db, ctx, issued.Token)) != 0 || changes != 0 {
				t.Fatalf("failed transaction changed draft/submission/expiry: before=%+v, after=%+v, err=%v", before, after, err)
			}
			if err := db.WithContext(ctx).Exec("ALTER TABLE links DROP CONSTRAINT test_link_rejection").Error; err != nil {
				t.Fatal(err)
			}
			if saved, err := store.Submit(ctx, issued.Token, details, ""); err != nil || saved != Saved {
				t.Fatalf("retry failed: saved=%v, err=%v", saved, err)
			}
			completed, err := store.Lookup(ctx, issued.Token)
			enrolled := studentsFor(t, db, ctx, issued.Token)
			if err != nil || completed == nil || completed.Details != details || completed.SubmittedAt == nil || changes != 1 ||
				len(enrolled) != 0 || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour {
				t.Fatalf("retry did not commit completion: link=%+v, students=%+v, err=%v", completed, enrolled, err)
			}
		})
	}
}

func TestSubmissionCancellationRace(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	draft := students.Student{Details: students.Details{
		Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
	}}
	final := contactDetails("Final answer")
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(draft)); err != nil || saved != Saved {
			t.Fatalf("draft %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	type result struct {
		saved SaveResult
		entry *Entry
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		saved, err := store.Submit(ctx, issued.Token, final, "")
		results <- result{saved: saved, err: err}
	}()
	go func() {
		<-start
		entry, err := store.Cancel(ctx, issued.ID, "canceler")
		results <- result{entry: entry, err: err}
	}()
	close(start)
	var submitted SaveResult
	var canceled *Entry
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
	var row Link
	if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(issued.Token)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, issued.Token)
	if canceled == nil || canceled.Token != "" {
		t.Fatalf("cancel did not return a token-free saved entry: %+v", canceled)
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

func TestCancel(t *testing.T) {
	db, store, ctx := testStore(t)
	var entries [2]Entry
	for i := range entries {
		entry, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	const canceledName, name = "Canceled customer", "Still active"
	for i, saved := range []string{canceledName, name} {
		if ok, err := store.SaveDraft(ctx, entries[i].Token, "name", saved); err != nil || ok != Saved {
			t.Fatalf("save link %d name: saved=%v, err=%v", i, ok, err)
		}
	}

	t.Run("database failure returns no row", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if got, err := store.Cancel(cancelled, entries[0].ID, staff); err == nil || got != nil {
			t.Fatalf("entry=%+v, err=%v; want an error and no row", got, err)
		}
	})

	// The first cancellation's persisted time; repeats must not move it.
	var canceledAt *time.Time
	for _, tc := range []struct {
		name, id, staff string
		found           bool
	}{
		{"issued link", entries[0].ID, "alice", true},
		{"already canceled link keeps its audit", entries[0].ID, "bob", true},
		{"unknown link", strings.Repeat("0", hex.EncodedLen(sha256.Size)), staff, false},
		{"malformed ID", "not-issued", staff, false},
		{"customer token is not an ID", entries[1].Token, staff, false},
		{"SQL-like input", strings.Repeat("'", hex.EncodedLen(sha256.Size)), staff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Cancel(ctx, tc.id, tc.staff)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.found {
				if got != nil {
					t.Fatalf("returned %+v for an ID naming no link", got)
				}
				return
			}
			if got == nil || got.ID != entries[0].ID || got.Status != Canceled || got.Name != canceledName ||
				got.CreatedBy != staff || got.CanceledBy != "alice" || got.Token != "" || got.Cancelable() {
				t.Fatalf("returned %+v; want the saved row canceled by alice, without its token", got)
			}
			var row Link
			if err := db.WithContext(ctx).Where("token_hash = ?", digestOf(entries[0].Token)).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.CanceledAt == nil {
				t.Fatal("canceled link has no persisted cancellation time")
			}
			if canceledAt == nil {
				canceledAt = row.CanceledAt
			}
			if !row.CanceledAt.Equal(*canceledAt) {
				t.Fatalf("canceled_at moved from %v to %v", *canceledAt, *row.CanceledAt)
			}
		})
	}

	canceled, err := store.Open(ctx, entries[0].Token)
	if err != nil || canceled != nil {
		t.Fatalf("canceled link remains usable: link=%v, err=%v", canceled, err)
	}
	active, err := store.Open(ctx, entries[1].Token)
	if err != nil || active == nil {
		t.Fatalf("canceling one link revoked another: link=%v, err=%v", active, err)
	}
	if active.Name != name {
		t.Fatalf("other link's name = %q, want %q", active.Name, name)
	}
	if saved, err := store.SaveDraft(ctx, entries[0].Token, "name", "Must not revive"); err != nil || saved != Unavailable {
		t.Fatalf("canceled link accepted a draft: saved=%v, err=%v", saved, err)
	}
}

func TestAuditRecordsStaff(t *testing.T) {
	_, store, ctx := testStore(t)
	var issued [3]Entry
	for i, by := range []string{"alice", "bob", "alice"} {
		entry, err := store.Create(ctx, by, later)
		if err != nil {
			t.Fatal(err)
		}
		issued[i] = entry
	}
	if _, err := store.Cancel(ctx, issued[0].ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelAll(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range issued[1:] {
		if link, err := store.Open(ctx, entry.Token); err != nil || link != nil {
			t.Fatalf("unfinished link still opens after CancelAll: link=%+v, err=%v", link, err)
		}
	}

	t.Run("anonymous actions are refused", func(t *testing.T) {
		if _, err := store.Create(ctx, "", later); !errors.Is(err, errNoStaff) {
			t.Errorf("Create: err = %v, want errNoStaff", err)
		}
		if got, err := store.Cancel(ctx, issued[1].ID, ""); !errors.Is(err, errNoStaff) || got != nil {
			t.Errorf("Cancel: entry = %+v, err = %v; want no row, errNoStaff", got, err)
		}
		if err := store.CancelAll(ctx, ""); !errors.Is(err, errNoStaff) {
			t.Errorf("CancelAll: err = %v, want errNoStaff", err)
		}
	})

	listed := listByID(t, store, ctx)
	for i, want := range []struct{ createdBy, canceledBy string }{
		{"alice", "bob"}, // CancelAll must not overwrite an earlier cancellation.
		{"bob", "alice"},
		{"alice", "alice"},
	} {
		got := listed[issued[i].ID]
		if got.CreatedBy != want.createdBy || got.CanceledBy != want.canceledBy {
			t.Errorf("link %d: created by %q, canceled by %q; want %q, %q",
				i, got.CreatedBy, got.CanceledBy, want.createdBy, want.canceledBy)
		}
	}
	if len(listed) != len(issued) {
		t.Errorf("listed %d links, want %d: an anonymous Create persisted", len(listed), len(issued))
	}
}

func TestExpiredLinksStopWorking(t *testing.T) {
	db, store, ctx := testStore(t)
	issuedExpired, err := store.Create(ctx, staff, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	midForm, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveDraft(ctx, midForm.Token, "name", "Half done"); err != nil || saved != Saved {
		t.Fatalf("draft before expiry: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.Submit(ctx, finished.Token, contactDetails("Done final"), ""); err != nil || saved != Saved {
		t.Fatalf("submit before expiry: saved=%v, err=%v", saved, err)
	}
	if err := acceptAgreement(ctx, db, store, finished.Token, programStart); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, digestOf(finished.Token), now)
	}); err != nil {
		t.Fatal(err)
	}
	enrolled := studentsFor(t, db, ctx, finished.Token)
	if len(enrolled) != 1 {
		t.Fatalf("finished link has %d students, want one", len(enrolled))
	}
	// Both lapse while the customer could still be on the page.
	if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash IN ?", [][]byte{digestOf(midForm.Token), digestOf(finished.Token)}).
		Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"expired at issue": issuedExpired.Token, "mid-form": midForm.Token, "expired success page": finished.Token} {
		t.Run(name, func(t *testing.T) {
			if link, err := store.Open(ctx, token); err != nil || link != nil {
				t.Errorf("open: link=%v, err=%v; want none", link, err)
			}
			if saved, err := store.SaveDraft(ctx, token, "name", "After expiry"); err != nil || saved != Unavailable {
				t.Errorf("draft: saved=%v, err=%v; want refused", saved, err)
			}
			if saved, err := store.Submit(ctx, token, contactDetails("After expiry"), ""); err != nil || saved != Unavailable {
				t.Errorf("submit: saved=%v, err=%v; want refused", saved, err)
			}
		})
	}

	// Cancel returns the expired row as saved; it never turns into Canceled.
	if got, err := store.Cancel(ctx, midForm.ID, "bob"); err != nil || got == nil || got.ID != midForm.ID ||
		got.Status != Expired || got.Name != "Half done" || got.CanceledBy != "" || got.Token != "" {
		t.Fatalf("cancel expired link: entry=%+v, err=%v; want it expired with its saved name, without a token", got, err)
	}
	if err := store.CancelAll(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	listed := listByID(t, store, ctx)
	for _, tc := range []struct {
		name   string
		entry  Entry
		status Status
		saved  string
	}{
		{"expired at issue", issuedExpired, Expired, ""},
		{"expiry keeps saved data", midForm, Expired, "Half done"},
		{"finished stays finished", finished, Finished, "Done final"},
	} {
		got := listed[tc.entry.ID]
		if got.Status != tc.status || got.Name != tc.saved || got.CanceledBy != "" || got.Cancelable() ||
			got.ExpiresAt.After(time.Now()) || got.Trashed() != (tc.status == Expired) {
			t.Errorf("%s: listed %+v; want status %q, name %q, not canceled or cancelable", tc.name, got, tc.status, tc.saved)
		}
	}
	if after := studentsFor(t, db, ctx, finished.Token); !reflect.DeepEqual(after, enrolled) {
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

	first, err := store.Create(ctx, staff, later)
	step("create", 1, err)
	_, err = store.Open(ctx, first.Token)
	step("first visit", 2, err)
	_, err = store.Open(ctx, first.Token)
	step("repeat visit", 2, err)
	_, err = store.SaveDraft(ctx, first.Token, "name", "Draft")
	step("draft", 3, err)
	_, err = store.SaveDraft(ctx, first.Token, "name", "Draft")
	step("repeat draft", 3, err)
	_, err = store.Submit(ctx, first.Token, contactDetails("Final answer"), "")
	step("submit", 4, err)
	_, err = store.Submit(ctx, first.Token, contactDetails("Replay answer"), "")
	step("replayed submit", 4, err)
	_, err = store.Cancel(ctx, first.ID, staff)
	step("cancel finished", 4, err)
	second, err := store.Create(ctx, staff, later)
	step("second create", 5, err)
	_, err = store.Cancel(ctx, second.ID, staff)
	step("cancel", 6, err)
	_, err = store.Create(ctx, staff, later)
	step("third create", 7, err)
	err = store.CancelAll(ctx, staff)
	step("cancel all", 8, err)
	err = store.CancelAll(ctx, staff)
	step("nothing left to cancel", 8, err)
}

func TestDraftFieldIsolationAndConcurrentClearing(t *testing.T) {
	db, store, ctx := testStore(t)
	issued, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	initial := students.Student{Details: contactDetails("Original draft")}
	for _, field := range students.Fields() {
		if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, field.Value(initial)); err != nil || saved != Saved {
			t.Fatalf("seed %s: saved=%v, err=%v", field.Key, saved, err)
		}
	}
	before, err := store.Open(ctx, issued.Token)
	if err != nil || before == nil || before.OpenedAt == nil {
		t.Fatalf("open draft: link=%+v, err=%v", before, err)
	}
	otherBefore, err := store.Lookup(ctx, other.Token)
	if err != nil || otherBefore == nil {
		t.Fatalf("load other link: link=%+v, err=%v", otherBefore, err)
	}
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
			want: students.Details{}, status: Opened,
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
					saved, err := store.SaveDraft(ctx, issued.Token, change.key, change.value)
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
			after, err := store.Lookup(ctx, issued.Token)
			want := *before
			want.Details = step.want
			if err != nil || after == nil || !reflect.DeepEqual(*after, want) {
				t.Fatalf("draft changed unrelated data: link=%+v, want=%+v, err=%v", after, want, err)
			}
			if entry := listByID(t, store, ctx)[issued.ID]; entry.Status != step.status ||
				entry.Name != step.want.Name || entry.Phone != step.want.Phone || entry.Token != "" {
				t.Fatalf("draft projection: %+v, want status=%v, details=%+v", entry, step.status, step.want)
			}
			otherAfter, err := store.Lookup(ctx, other.Token)
			if err != nil || !reflect.DeepEqual(otherAfter, otherBefore) {
				t.Fatalf("draft changed another link: before=%+v, after=%+v, err=%v", otherBefore, otherAfter, err)
			}
			if enrolled := studentsFor(t, db, ctx, issued.Token); len(enrolled) != 0 {
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
	for _, key := range []string{"", "Name", "email_confirm", "token_hash", "submitted_at", "name; DROP TABLE links"} {
		t.Run(key, func(t *testing.T) {
			if saved, err := store.SaveDraft(t.Context(), token, key, "Must not save"); saved != Unavailable ||
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
			issued, err := store.Create(ctx, staff, later)
			if err != nil {
				t.Fatal(err)
			}
			draft := students.Student{Details: students.Details{
				Name: "Original draft", Email: "draft@example.com", Phone: "+12025550123", Municipality: "Ponce",
			}}
			final := contactDetails("Final answer")
			for _, seed := range students.Fields() {
				if saved, err := store.SaveDraft(ctx, issued.Token, seed.Key, seed.Value(draft)); err != nil || saved != Saved {
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
				saved, err := store.SaveDraft(ctx, issued.Token, field.Key, "")
				results <- result{draft: true, saved: saved, err: err}
			}()
			go func() {
				<-start
				saved, err := store.Submit(ctx, issued.Token, final, "")
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
			completed, err := store.Lookup(ctx, issued.Token)
			enrolled := studentsFor(t, db, ctx, issued.Token)
			if err != nil || completed == nil || completed.Details != final || completed.SubmittedAt == nil ||
				completed.CanceledAt != nil || completed.ExpiresAt.Sub(*completed.SubmittedAt) != 168*time.Hour ||
				len(enrolled) != 0 {
				t.Fatalf("racing draft changed final snapshot: link=%+v, students=%+v, err=%v", completed, enrolled, err)
			}
			if saved, err := store.SaveDraft(ctx, issued.Token, field.Key, ""); err != nil || saved != AlreadySubmitted {
				t.Fatalf("late clear: result=%v, err=%v", saved, err)
			}
			if saved, err := store.Submit(ctx, issued.Token, draft.Details, ""); err != nil || saved != AlreadySubmitted {
				t.Fatalf("final replay: result=%v, err=%v", saved, err)
			}
			after, err := store.Lookup(ctx, issued.Token)
			if err != nil || !reflect.DeepEqual(after, completed) || !reflect.DeepEqual(studentsFor(t, db, ctx, issued.Token), enrolled) {
				t.Fatalf("replay changed racing completion: before=%+v, after=%+v, err=%v", completed, after, err)
			}
		})
	}
}

func TestAcceptTermsRequiresContactAndCurrentDisplayedDate(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	const firstDate, currentDate = "2026-11-10", "2026-11-17"
	options := settings.New(db)
	if err := options.SetProgramStartDate(ctx, firstDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"unopened", "draft"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "draft" {
				if saved, err := store.SaveDraft(ctx, entry.Token, "name", "Draft contact"); err != nil || saved != Saved {
					t.Fatalf("draft: saved=%v, err=%v", saved, err)
				}
			}
			before, err := store.Lookup(ctx, entry.Token)
			if err != nil || before == nil {
				t.Fatalf("load contact: link=%+v, err=%v", before, err)
			}
			if got, err := store.AcceptTerms(ctx, entry.Token, firstDate); err != nil || got != nil {
				t.Fatalf("accept before submission: link=%+v, err=%v", got, err)
			}
			if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, before) ||
				len(studentsFor(t, db, ctx, entry.Token)) != 0 {
				t.Fatalf("premature agreement changed contact: link=%+v, err=%v", after, err)
			}
		})
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	before, err := store.Lookup(ctx, entry.Token)
	if err != nil || before == nil {
		t.Fatalf("load submission: link=%+v, err=%v", before, err)
	}
	enrolled := studentsFor(t, db, ctx, entry.Token)
	writes := 0
	store = New(db, func() { writes++ })
	if err := options.SetProgramStartDate(ctx, currentDate, staff); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{"", "not-a-date", "2026-02-30", firstDate} {
		t.Run("reject displayed "+shown, func(t *testing.T) {
			if got, err := store.AcceptTerms(ctx, entry.Token, shown); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("stale acceptance: link=%+v, err=%v", got, err)
			}
		})
	}
	if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, before) || writes != 0 {
		t.Fatalf("rejected agreement changed link: link=%+v, writes=%d, err=%v", after, writes, err)
	}
	accepted, err := store.AcceptTerms(ctx, entry.Token, currentDate)
	if err != nil || accepted == nil || accepted.TermsAcceptedAt == nil || accepted.ProgramStartDate != currentDate || writes != 1 {
		t.Fatalf("accept: link=%+v, writes=%d, err=%v", accepted, writes, err)
	}
	want := *before
	want.TermsAcceptedAt, want.ProgramStartDate = accepted.TermsAcceptedAt, currentDate
	if !reflect.DeepEqual(*accepted, want) || accepted.TermsAcceptedAt.Before(*accepted.SubmittedAt) ||
		!reflect.DeepEqual(studentsFor(t, db, ctx, entry.Token), enrolled) {
		t.Fatalf("acceptance changed submission or enrollment: link=%+v, want=%+v", accepted, want)
	}
	if persisted, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(persisted, accepted) {
		t.Fatalf("acceptance not persisted: link=%+v, err=%v", persisted, err)
	}
	if err := options.SetProgramStartDate(ctx, "2027-01-05", staff); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{currentDate, "2027-01-05", "", "invalid replay"} {
		if got, err := store.AcceptTerms(ctx, entry.Token, shown); err != nil || !reflect.DeepEqual(got, accepted) || writes != 1 {
			t.Fatalf("accepted replay changed agreement: link=%+v, writes=%d, err=%v", got, writes, err)
		}
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Replacement contact"), ""); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted replay: saved=%v, err=%v", saved, err)
	}
	if saved, err := store.SaveDraft(ctx, entry.Token, "name", "Replacement draft"); err != nil || saved != AlreadySubmitted {
		t.Fatalf("submitted draft: saved=%v, err=%v", saved, err)
	}
	if after, err := store.Lookup(ctx, entry.Token); err != nil || !reflect.DeepEqual(after, accepted) || writes != 1 {
		t.Fatalf("contact replay changed agreement: link=%+v, writes=%d, err=%v", after, writes, err)
	}
}

func TestAcceptTermsRejectsUnconfiguredStartDate(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}
	for _, stage := range []string{"missing settings row", "empty date"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "empty date" {
				if err := settings.New(db).SetLinkLifetimeDays(ctx, 7, staff); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10"); got != nil || !errors.Is(err, ErrProgramStartChanged) {
				t.Fatalf("unconfigured date accepted: link=%+v, err=%v", got, err)
			}
		})
	}
}

func TestAcceptTermsObservesConcurrentStartDateChange(t *testing.T) {
	db, store, ctx := testStore(t)
	entry, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
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
		link *Link
		err  error
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
		link, err := store.AcceptTerms(acceptCtx, entry.Token, "2026-11-10")
		results <- result{link: link, err: err}
	}()
	testdb.AwaitBlocked(t, ctx, db, blocker, results)
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-results
	if got.link != nil || !errors.Is(got.err, ErrProgramStartChanged) {
		t.Fatalf("acceptance ignored committed date change: link=%+v, err=%v", got.link, got.err)
	}
	if persisted, err := store.Lookup(ctx, entry.Token); err != nil || persisted == nil || persisted.TermsAcceptedAt != nil || persisted.ProgramStartDate != "" {
		t.Fatalf("stale agreement persisted: link=%+v, err=%v", persisted, err)
	}
}

func TestAgreementWritesRejectUnusableTokens(t *testing.T) {
	db, store, ctx := testStore(t)
	if err := settings.New(db).SetProgramStartDate(ctx, "2026-11-10", staff); err != nil {
		t.Fatal(err)
	}
	var unusable []string
	for _, stage := range []string{"expired submission", "expired acceptance", "canceled acceptance"} {
		entry, err := store.Create(ctx, staff, later)
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := store.Submit(ctx, entry.Token, contactDetails("Submitted contact"), ""); err != nil || saved != Saved {
			t.Fatalf("submit: saved=%v, err=%v", saved, err)
		}
		if stage != "expired submission" {
			if got, err := store.AcceptTerms(ctx, entry.Token, "2026-11-10"); err != nil || got == nil {
				t.Fatalf("accept: link=%+v, err=%v", got, err)
			}
		}
		column := "expires_at"
		if stage == "canceled acceptance" {
			column = "canceled_at"
		}
		if err := db.WithContext(ctx).Model(&Link{}).Where("token_hash = ?", digestOf(entry.Token)).Update(column, time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		unusable = append(unusable, entry.Token)
	}
	canceled, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Cancel(ctx, canceled.ID, staff); err != nil || got == nil || got.Status != Canceled {
		t.Fatalf("cancel unfinished contact: entry=%+v, err=%v", got, err)
	}
	unusable = append(unusable, canceled.Token)
	unusable = append(unusable, "", "malformed", strings.Repeat("'", base64.RawURLEncoding.EncodedLen(tokenBytes)),
		base64.RawURLEncoding.EncodeToString(make([]byte, tokenBytes)))
	var linksBefore []Link
	var studentsBefore []students.Student
	if err := db.WithContext(ctx).Order("token_hash").Find(&linksBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	store = New(db, func() { writes++ })
	for _, token := range unusable {
		if got, err := store.AcceptTerms(ctx, token, "2026-11-10"); err != nil || got != nil {
			t.Fatalf("unusable acceptance: link=%+v, err=%v", got, err)
		}
	}
	var linksAfter []Link
	var studentsAfter []students.Student
	if err := db.WithContext(ctx).Order("token_hash").Find(&linksAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Order("id").Find(&studentsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if writes != 0 || !reflect.DeepEqual(linksAfter, linksBefore) || !reflect.DeepEqual(studentsAfter, studentsBefore) {
		t.Fatalf("unusable agreement writes changed retained rows: writes=%d", writes)
	}
}

func testStore(t *testing.T) (*gorm.DB, *Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &Link{}, &students.Student{}, &settings.Settings{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, New(db, nil), ctx
}

func listByID(t *testing.T, store *Store, ctx context.Context) map[string]Entry {
	t.Helper()
	entries, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	return byID
}

func digestOf(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func studentsFor(t *testing.T, db *gorm.DB, ctx context.Context, token string) []students.Student {
	t.Helper()
	var enrolled []students.Student
	if err := db.WithContext(ctx).Where("source_link_hash = ?", digestOf(token)).Find(&enrolled).Error; err != nil {
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

// acceptAgreement configures date as the program start and accepts token's submitted agreement
// at it, as checkout requires before payment can enroll the student.
func acceptAgreement(ctx context.Context, db *gorm.DB, store *Store, token, date string) error {
	if err := settings.New(db).SetProgramStartDate(ctx, date, staff); err != nil {
		return err
	}
	if accepted, err := store.AcceptTerms(ctx, token, date); err != nil || accepted == nil {
		return fmt.Errorf("accept agreement: link=%+v, err=%v", accepted, err)
	}
	return nil
}

func TestPayBeforeStudentInvariants(t *testing.T) {
	db, store, ctx := testStore(t)

	// (a) after Submit with a PIN, students has 0 rows, the link row holds details and PIN, List reports AwaitingPayment
	issued1, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	details1 := contactDetails("Customer One")
	details1.Phone = "+17875551111"
	const pin = "789012"
	saved, err := store.Submit(ctx, issued1.Token, details1, pin)
	if err != nil || saved != Saved {
		t.Fatalf("submit link 1: saved=%v, err=%v", saved, err)
	}
	var count int64
	db.Model(&students.Student{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 students after submit, got %d", count)
	}
	link1, err := store.Lookup(ctx, issued1.Token)
	if err != nil || link1 == nil {
		t.Fatalf("lookup link 1: %v, %v", link1, err)
	}
	if link1.Details != details1 || link1.Pin != pin || link1.PaidAt != nil || link1.SubmittedAt == nil {
		t.Fatalf("link 1 state unexpected: %+v", link1)
	}
	entries, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entry1 *Entry
	for i := range entries {
		if entries[i].ID == issued1.ID {
			entry1 = &entries[i]
		}
	}
	if entry1 == nil || entry1.Status != AwaitingPayment || entry1.Name != details1.Name || entry1.Phone != details1.Phone {
		t.Fatalf("link 1 list entry unexpected: %+v", entry1)
	}

	// (b) a second link submitting the same PIN gets PinTaken and stays unsubmitted
	issued2, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	details2 := contactDetails("Customer Two")
	saved2, err := store.Submit(ctx, issued2.Token, details2, pin)
	if err != nil || saved2 != PinTaken {
		t.Fatalf("submit link 2 with duplicate pin: saved=%v, err=%v; want PinTaken", saved2, err)
	}
	link2, err := store.Lookup(ctx, issued2.Token)
	if err != nil || link2 == nil {
		t.Fatalf("lookup link 2: %v, %v", link2, err)
	}
	if link2.SubmittedAt != nil || link2.Pin != "" {
		t.Fatalf("link 2 should stay unsubmitted: %+v", link2)
	}

	// (c) Enroll in a tx creates exactly one student with details, PIN, created_at == paidAt, Básico,
	// and the accepted start date's month as cohort even after staff move the date;
	// sets links.paid_at, clears links.pin, status becomes Finished; second Enroll returns error
	if err := acceptAgreement(ctx, db, store, issued1.Token, programStart); err != nil {
		t.Fatal(err)
	}
	if err := settings.New(db).SetProgramStartDate(ctx, "2027-01-05", staff); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, link1.TokenHash, paidAt)
	}); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}
	var student students.Student
	if err := db.Where("source_link_hash = ?", link1.TokenHash).Take(&student).Error; err != nil {
		t.Fatalf("find enrolled student: %v", err)
	}
	if student.Details != details1 || student.Pin != pin || !student.CreatedAt.Equal(paidAt) ||
		student.Group != "Básico" || student.Cohort != "2026-11" {
		t.Fatalf("enrolled student mismatch: %+v", student)
	}
	link1After, err := store.Lookup(ctx, issued1.Token)
	if err != nil || link1After == nil {
		t.Fatal(err)
	}
	if link1After.Pin != "" || link1After.PaidAt == nil || !link1After.PaidAt.Equal(paidAt) {
		t.Fatalf("link 1 after enroll: pin=%q, paid_at=%v", link1After.Pin, link1After.PaidAt)
	}
	entry1After := link1After.entry(time.Now())
	if entry1After.Status != Finished {
		t.Fatalf("entry status after enroll = %v; want Finished", entry1After.Status)
	}
	// Second Enroll returns error and still leaves 1 student
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return Enroll(tx, link1.TokenHash, paidAt)
	})
	if err == nil {
		t.Fatal("expected second Enroll to fail")
	}
	db.Model(&students.Student{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 student after second enroll, got %d", count)
	}

	// (c2) Enroll cleared link 1's PIN, but the Student holds it: a fourth link cannot submit it
	issued4, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, issued4.Token, contactDetails("Customer Four"), pin); err != nil || saved != PinTaken {
		t.Fatalf("submit with an enrolled student's pin: saved=%v, err=%v; want PinTaken", saved, err)
	}
	if link4, err := store.Lookup(ctx, issued4.Token); err != nil || link4 == nil || link4.SubmittedAt != nil || link4.Pin != "" {
		t.Fatalf("link 4 should stay unsubmitted: %+v, err=%v", link4, err)
	}

	// (d) a submitted unpaid link past expires_at is Expired and Trashed(), but the same link with PaidAt set is Finished and not trashed; Archived flips 24 h after PaidAt, not after SubmittedAt.
	issued3, err := store.Create(ctx, staff, later)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Submit(ctx, issued3.Token, contactDetails("Customer Three"), "112233"); err != nil || saved != Saved {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	link3, _ := store.Lookup(ctx, issued3.Token)
	link3.ExpiresAt = past
	e3Unpaid := link3.entry(time.Now())
	if e3Unpaid.Status != Expired || !e3Unpaid.Trashed() {
		t.Fatalf("unpaid expired link status=%v, trashed=%v; want Expired, true", e3Unpaid.Status, e3Unpaid.Trashed())
	}
	// Same link with PaidAt set is Finished and not trashed
	now := time.Now()
	link3.PaidAt = &now
	e3Paid := link3.entry(now)
	if e3Paid.Status != Finished || e3Paid.Trashed() {
		t.Fatalf("paid expired link status=%v, trashed=%v; want Finished, false", e3Paid.Status, e3Paid.Trashed())
	}
	// Archived flips 24 h after PaidAt
	link3.PaidAt = &past
	submittedOld := past.Add(-48 * time.Hour)
	link3.SubmittedAt = &submittedOld
	// Paid 1 hour ago: not archived yet
	e3RecentPaid := link3.entry(now)
	if e3RecentPaid.Archived(now) {
		t.Fatalf("paid 1h ago should not be archived yet: %+v", e3RecentPaid)
	}
	// Paid 25 hours ago: archived
	paidLongAgo := now.Add(-25 * time.Hour)
	link3.PaidAt = &paidLongAgo
	e3OldPaid := link3.entry(now)
	if !e3OldPaid.Archived(now) {
		t.Fatalf("paid 25h ago should be archived: %+v", e3OldPaid)
	}
}
