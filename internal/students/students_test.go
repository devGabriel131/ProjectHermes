//go:build integration

package students_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"projecthermes/internal/links"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
	"projecthermes/internal/testdb"
)

func TestListRetainsExpiredSourcesAndOrdersNewestFirst(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	if listed, err := roster.List(ctx, students.Filter{}); err != nil || len(listed) != 0 {
		t.Fatalf("empty roster = %+v, %v", listed, err)
	}

	_, oldest := enroll(t, db, sources, ctx, "Same name")
	_, firstTie := enroll(t, db, sources, ctx, "Same name")
	_, lastTie := enroll(t, db, sources, ctx, "Same name")
	oldTime := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	newTime := oldTime.Add(time.Hour)
	for _, fixture := range []struct {
		student students.Student
		at      time.Time
	}{{oldest, oldTime}, {firstTie, newTime}, {lastTie, newTime}} {
		if !fixture.student.IsActive {
			t.Fatalf("new student inactive: %+v", fixture.student)
		}
		if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", fixture.student.ID).
			Update("created_at", fixture.at).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.WithContext(ctx).Model(&links.Link{}).Where("token_hash = ?", fixture.student.SourceLinkHash).
			Updates(map[string]any{"submitted_at": fixture.at, "expires_at": fixture.at.Add(168 * time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", oldest.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", oldest.ID).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.WithContext(ctx).AutoMigrate(&links.Link{}, &students.Student{}); err != nil {
			t.Fatal(err)
		}
	}
	var sourceAfter links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", oldest.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Fatalf("inactive student changed retained source: before=%+v, after=%+v", sourceBefore, sourceAfter)
	}

	listed, err := roster.List(ctx, students.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []uint64{lastTie.ID, firstTie.ID, oldest.ID}
	if len(listed) != len(wantIDs) {
		t.Fatalf("retained %d students, want %d", len(listed), len(wantIDs))
	}
	for i, id := range wantIDs {
		if listed[i].ID != id || listed[i].Name != "Same name" || listed[i].IsActive != (id != oldest.ID) {
			t.Fatalf("row %d = %+v, want student %d", i, listed[i], id)
		}
		got, err := roster.Get(ctx, id)
		if err != nil || got == nil || !reflect.DeepEqual(*got, listed[i]) {
			t.Fatalf("Get(%d) = %+v, %v, want listed student %+v", id, got, err, listed[i])
		}
	}
	if _, err := roster.UpdateField(ctx, firstTie.ID, "group", "Regular"); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", oldest.ID).Update("cohort", "2026-10").Error; err != nil {
		t.Fatal(err)
	}
	if cohorts, err := roster.Cohorts(ctx); err != nil || !reflect.DeepEqual(cohorts, []string{"2026-11", "2026-10"}) {
		t.Fatalf("Cohorts() = %v, %v, want newest first without repeats", cohorts, err)
	}
	active, suspended := true, false
	for _, filter := range []struct {
		filter students.Filter
		want   []uint64
	}{
		{students.Filter{Active: &active}, wantIDs[:2]},
		{students.Filter{Active: &suspended}, wantIDs[2:]},
		{students.Filter{Cohort: "2026-11"}, wantIDs[:2]},
		{students.Filter{Group: "Básico"}, []uint64{lastTie.ID, oldest.ID}},
		{students.Filter{Active: &active, Cohort: "2026-11", Group: "Básico"}, []uint64{lastTie.ID}},
		{students.Filter{Active: &suspended, Group: "Regular"}, nil},
	} {
		filtered, err := roster.List(ctx, filter.filter)
		if err != nil || len(filtered) != len(filter.want) {
			t.Fatalf("List(%+v) = %+v, %v, want students %v", filter.filter, filtered, err, filter.want)
		}
		for i, id := range filter.want {
			if filtered[i].ID != id {
				t.Fatalf("List(%+v)[%d] = student %d, want %d", filter.filter, i, filtered[i].ID, id)
			}
		}
	}
	entries, err := sources.List(ctx)
	if err != nil || len(entries) != 3 {
		t.Fatalf("source history = %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Status != links.Finished || entry.ExpiresAt.After(time.Now()) {
			t.Fatalf("source %+v is not an expired completion", entry)
		}
	}
	for _, id := range []uint64{0, lastTie.ID + 1, 1 << 63, ^uint64(0)} {
		if got, err := roster.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("missing Get(%d) = %+v, %v", id, got, err)
		}
	}
}

func TestUpdateFieldChangesOnlySelectedCanonicalColumn(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Original submission")
	_, other := enroll(t, db, sources, ctx, "Other student")
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	roster = students.New(db, func() { writes++ })
	for _, tc := range []struct {
		key, raw, want string
	}{
		{"name", " \tMaría\u2003José Pérez Rivera\n", "María José Pérez Rivera"},
		{"email", " Canonical.Name+Student@EXAMPLE.ORG ", "Canonical.Name+Student@example.org"},
		{"phone", "(939) 234-5678", "+19392345678"},
		{"municipality", " \tAñasco \u2003", "Añasco"},
		{"date_of_birth", " 2004-05-14 ", "2004-05-14"},
		{"gender", " Male ", "Male"},
		{"pin", " 000123 ", "000123"},
		{"group", " Regular ", "Regular"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			updated, err := roster.UpdateField(ctx, original.ID, tc.key, tc.raw)
			want := original
			switch tc.key {
			case "name":
				want.Name = tc.want
			case "email":
				want.Email = tc.want
			case "phone":
				want.Phone = tc.want
			case "municipality":
				want.Municipality = tc.want
			case "date_of_birth":
				want.DateOfBirth = tc.want
			case "gender":
				want.Gender = tc.want
			case "pin":
				want.Pin = tc.want
			case "group":
				want.Group = tc.want
			}
			if err != nil || updated == nil || !reflect.DeepEqual(*updated, want) {
				t.Fatalf("UpdateField = %+v, %v, want %+v", updated, err, want)
			}
			persisted, err := roster.Get(ctx, original.ID)
			if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) {
				t.Fatalf("persisted student = %+v, %v, want %+v", persisted, err, want)
			}
			unrelated, err := roster.Get(ctx, other.ID)
			if err != nil || unrelated == nil || !reflect.DeepEqual(*unrelated, other) {
				t.Fatalf("other student changed: %+v, %v", unrelated, err)
			}
			var sourceAfter links.Link
			if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sourceBefore, sourceAfter) {
				t.Fatalf("immutable source changed: before %+v, after %+v", sourceBefore, sourceAfter)
			}
			xmin, writesBefore := studentXmin(t, db, ctx, original.ID), writes
			replayed, err := roster.UpdateField(ctx, original.ID, tc.key, tc.raw)
			if err != nil || replayed == nil || !reflect.DeepEqual(*replayed, want) {
				t.Fatalf("normalized replay = %+v, %v, want %+v", replayed, err, want)
			}
			if after := studentXmin(t, db, ctx, original.ID); after != xmin {
				t.Errorf("normalized replay rewrote row: xmin %q -> %q", xmin, after)
			}
			if writes != writesBefore {
				t.Errorf("normalized replay notified %d writes, want zero", writes-writesBefore)
			}
			persisted, err = roster.Get(ctx, original.ID)
			if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) {
				t.Fatalf("replay changed persisted student: %+v, %v, want %+v", persisted, err, want)
			}
			original = want
		})
	}
	if writes != len(students.StaffFields()) {
		t.Fatalf("committed-write notifications = %d, want %d", writes, len(students.StaffFields()))
	}
}

func TestUpdateFieldRejectsUnknownFieldsAndInvalidDetailsWithoutWrites(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, original := enroll(t, db, sources, ctx, "Unchanged student")
	_, other := enroll(t, db, sources, ctx, "Other student")
	var sourceBefore links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceBefore).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	roster = students.New(db, func() { writes++ })
	for _, key := range []string{"", "Name", "id", "email_confirm", "source_link_hash", "created_at", "is_active", "cohort", "name = 'changed'; DROP TABLE students; --"} {
		t.Run("unknown field "+key, func(t *testing.T) {
			if got, err := roster.UpdateField(ctx, original.ID, key, "Changed student"); got != nil || !errors.Is(err, students.ErrUnknownField) {
				t.Fatalf("UpdateField(%q) = %+v, %v, want ErrUnknownField", key, got, err)
			}
		})
	}
	for _, tc := range []struct {
		name, key, raw string
	}{
		{"one word name", "name", "Changed"},
		{"malformed email", "email", "PRIVATE_EMAIL"},
		{"Canadian phone", "phone", "+15062345678"},
		{"forged municipality", "municipality", "PRIVATE_FORGED_MUNICIPALITY"},
		{"future date of birth", "date_of_birth", "2099-01-01"},
		{"forged gender", "gender", "PRIVATE_FORGED_GENDER"},
		{"short pin", "pin", "123"},
		{"forged group", "group", "PRIVATE_FORGED_GROUP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roster.UpdateField(ctx, original.ID, tc.key, tc.raw)
			var invalid *students.ValidationError
			if got != nil || !errors.As(err, &invalid) {
				t.Fatalf("UpdateField = %+v, %v, want ValidationError", got, err)
			}
		})
	}
	for _, field := range students.StaffFields() {
		t.Run("missing student "+field.Key, func(t *testing.T) {
			for _, id := range []uint64{0, other.ID + 1, 1 << 63, ^uint64(0)} {
				if got, err := roster.UpdateField(ctx, id, field.Key, field.Value(original)); got != nil || err != nil {
					t.Fatalf("missing UpdateField(%d, %q) = %+v, %v", id, field.Key, got, err)
				}
			}
		})
	}
	for _, want := range []students.Student{original, other} {
		persisted, err := roster.Get(ctx, want.ID)
		if err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) {
			t.Fatalf("rejected edits changed student: %+v, %v, want %+v", persisted, err, want)
		}
	}
	var sourceAfter links.Link
	if err := db.WithContext(ctx).Where("token_hash = ?", original.SourceLinkHash).Take(&sourceAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceBefore, sourceAfter) {
		t.Fatalf("rejected edits changed source: before %+v, after %+v", sourceBefore, sourceAfter)
	}
	if listed, err := roster.List(ctx, students.Filter{}); err != nil || len(listed) != 2 {
		t.Fatalf("rejected edits changed roster: %+v, %v", listed, err)
	}
	if writes != 0 {
		t.Fatalf("rejected edits notified %d writes", writes)
	}
}

func TestUpdateFieldObservesConcurrentEdits(t *testing.T) {
	for _, tc := range []struct {
		name, raw          string
		deleted, wantWrite bool
	}{
		{name: "concurrent_equal", raw: " Concurrent\u2003student "},
		{name: "concurrent_noop_becomes_change", raw: " Original\u2003student ", wantWrite: true},
		{name: "concurrent_delete", raw: "Changed student", deleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, sources, ctx := studentStores(t)
			_, original := enroll(t, db, sources, ctx, "Original student")
			writes := 0
			roster := students.New(db, func() { writes++ })
			blocker := db.WithContext(ctx).Begin()
			if blocker.Error != nil {
				t.Fatal(blocker.Error)
			}
			saveCtx, cancelSave := context.WithCancel(ctx)
			type result struct {
				record *students.Student
				err    error
			}
			var results chan result
			defer func() {
				blocker.Rollback()
				cancelSave()
				if results != nil {
					for range results {
					}
				}
			}()
			row := blocker.Model(&students.Student{}).Where("id = ?", original.ID)
			var held *gorm.DB
			if tc.deleted {
				held = row.Delete(&students.Student{})
			} else {
				held = row.Updates(map[string]any{"name": "Concurrent student", "email": "Concurrent.Student@example.org"})
			}
			if held.Error != nil || held.RowsAffected != 1 {
				t.Fatalf("hold concurrent edit: rows=%d, err=%v", held.RowsAffected, held.Error)
			}
			var xmin string
			if !tc.deleted {
				xmin = studentXmin(t, blocker, ctx, original.ID)
			}
			results = make(chan result, 1)
			go func() {
				defer close(results)
				record, err := roster.UpdateField(saveCtx, original.ID, "name", tc.raw)
				results <- result{record: record, err: err}
			}()
			testdb.AwaitBlocked(t, ctx, db, blocker, results)
			if err := blocker.Commit().Error; err != nil {
				t.Fatal(err)
			}
			got := <-results
			if got.err != nil {
				t.Fatalf("save after concurrent edit: %v", got.err)
			}
			persisted, err := roster.Get(ctx, original.ID)
			if tc.deleted {
				if got.record != nil || err != nil || persisted != nil || writes != 0 {
					t.Fatalf("save ignored deletion: result=%+v, persisted=%+v, err=%v, writes=%d", got.record, persisted, err, writes)
				}
				return
			}
			want := original
			want.Email = "Concurrent.Student@example.org"
			wantWrites := 0
			if tc.wantWrite {
				wantWrites = 1
			} else {
				want.Name = "Concurrent student"
			}
			if got.record == nil || !reflect.DeepEqual(*got.record, want) ||
				err != nil || persisted == nil || !reflect.DeepEqual(*persisted, want) || writes != wantWrites {
				t.Fatalf("save after edit: result=%+v, persisted=%+v, err=%v, writes=%d; want %+v, writes=%d",
					got.record, persisted, err, writes, want, wantWrites)
			}
			if after := studentXmin(t, db, ctx, original.ID); (after != xmin) != tc.wantWrite {
				t.Fatalf("xmin %q -> %q, want UPDATE=%t", xmin, after, tc.wantWrite)
			}
		})
	}
}

func studentXmin(t *testing.T, db *gorm.DB, ctx context.Context, id uint64) string {
	t.Helper()
	var xmin string
	if err := db.WithContext(ctx).Raw("SELECT xmin::text FROM students WHERE id = ?", id).Scan(&xmin).Error; err != nil {
		t.Fatal(err)
	}
	if xmin == "" {
		t.Fatal("student row missing xmin")
	}
	return xmin
}

func studentStores(t *testing.T) (*gorm.DB, *students.Store, *links.Store, context.Context) {
	t.Helper()
	db := testdb.Open(t, &links.Link{}, &students.Student{}, &settings.Settings{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return db, students.New(db, nil), links.New(db, nil), ctx
}

// pinSeq gives each enrolled test student its own PIN: the roster refuses duplicates.
var pinSeq atomic.Int32

func enroll(t *testing.T, db *gorm.DB, sources *links.Store, ctx context.Context, name string) (links.Entry, students.Student) {
	t.Helper()
	entry, err := sources.Create(ctx, "student-test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	details := students.Details{
		Name: name, Email: "Original.Student@example.com", Phone: "+17872345678", Municipality: "San Juan", DateOfBirth: "2005-06-15", Gender: "Female",
	}
	if saved, err := sources.Submit(ctx, entry.Token, details, fmt.Sprintf("%06d", 100000+pinSeq.Add(1))); err != nil || saved != links.Saved {
		t.Fatalf("enrollment saved=%v, err=%v", saved, err)
	}
	digest, err := hex.DecodeString(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Checkout requires the accepted agreement whose start date Enroll turns into the cohort.
	const programStart = "2026-11-10"
	if err := settings.New(db).SetProgramStartDate(ctx, programStart, "student-test"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := sources.AcceptTerms(ctx, entry.Token, programStart); err != nil || accepted == nil {
		t.Fatalf("accept agreement: link=%+v, err=%v", accepted, err)
	}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return links.Enroll(tx, digest, time.Now().UTC())
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	var student students.Student
	if err := db.WithContext(ctx).Where("source_link_hash = ?", digest).Take(&student).Error; err != nil {
		t.Fatal(err)
	}
	return entry, student
}

func TestPinHeldBySubmittedLinkBlocksUpdateField(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)

	// 1. Submit link with a PIN (no Student exists yet)
	entry, err := sources.Create(ctx, "student-test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const linkPin = "654321"
	details := students.Details{
		Name: "Pending Customer", Email: "pending@example.com", Phone: "+17875550000", Municipality: "Ponce",
	}
	if saved, err := sources.Submit(ctx, entry.Token, details, linkPin); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}

	// PinTaken is true for a PIN held only by a submitted link
	taken, err := roster.PinTaken(ctx, linkPin)
	if err != nil || !taken {
		t.Fatalf("PinTaken = %v, %v; want true, nil", taken, err)
	}

	// 2. An enrolled student cannot be edited to take the link-held PIN
	_, enrolledStudent := enroll(t, db, sources, ctx, "Enrolled Student")
	updated, err := roster.UpdateField(ctx, enrolledStudent.ID, "pin", linkPin)
	if updated != nil {
		t.Fatalf("expected nil updated student, got %+v", updated)
	}
	var valErr *students.ValidationError
	if !errors.As(err, &valErr) || valErr.Message != "That PIN is already in use." {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
}

func TestRosterCount(t *testing.T) {
	db, roster, sources, ctx := studentStores(t)
	_, s1 := enroll(t, db, sources, ctx, "Student 1")
	_, s2 := enroll(t, db, sources, ctx, "Student 2")
	if err := db.WithContext(ctx).Model(&students.Student{}).Where("id = ?", s2.ID).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}

	counts, err := roster.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Active != 1 || counts.Suspended != 1 || counts.Total() != 2 {
		t.Fatalf("counts = %+v; want Active=1, Suspended=1, Total=2", counts)
	}
	_ = s1
}

func TestAddInsertsValidStudentsAndRejectsTheRest(t *testing.T) {
	db, _, sources, ctx := studentStores(t)
	var calls int
	roster := students.New(db, func() { calls++ })

	// Setup: enroll one student
	_, enrolledStudent := enroll(t, db, sources, ctx, "Enrolled Student")

	// Submit a link holding PIN 654321
	linkEntry, err := sources.Create(ctx, "student-test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const linkPin = "654321"
	linkDetails := students.Details{
		Name: "Pending Customer", Email: "pending@example.com", Phone: "+17875550000", Municipality: "Ponce",
	}
	if saved, err := sources.Submit(ctx, linkEntry.Token, linkDetails, linkPin); err != nil || saved != links.Saved {
		t.Fatalf("submit: saved=%v, err=%v", saved, err)
	}

	entries := []students.NewStudent{
		{Name: "Ana Torres", Email: "ana@example.com", Pin: "0388"},
		{Name: "Amarilys Zapata", Email: "", Pin: "2556"},
		{Name: "Enrolled Duplicate", Email: "enrolled@example.com", Pin: enrolledStudent.Pin},
		{Name: "Link Duplicate", Email: "link@example.com", Pin: linkPin},
		{Name: "Ana Duplicate", Email: "ana2@example.com", Pin: "0388"},
		{Name: "Mirayda 123 Silvestrini", Email: "m@example.com", Pin: "7247"},
		{Name: "Luis Rivera", Email: "luis@example.com", Pin: "1234"},
	}

	added, rejected, err := roster.Add(ctx, "Regular", entries)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if len(added) != 2 {
		t.Fatalf("len(added) = %d, want 2", len(added))
	}
	if added[0].Name != "Ana Torres" || added[0].Pin != "0388" {
		t.Fatalf("added[0] = %+v, want Ana Torres (0388)", added[0])
	}
	if added[1].Name != "Luis Rivera" || added[1].Pin != "1234" {
		t.Fatalf("added[1] = %+v, want Luis Rivera (1234)", added[1])
	}

	wantRejectedIndices := []int{1, 2, 3, 4, 5}
	if len(rejected) != len(wantRejectedIndices) {
		t.Fatalf("len(rejected) = %d, want %d: %+v", len(rejected), len(wantRejectedIndices), rejected)
	}
	for i, r := range rejected {
		if r.Index != wantRejectedIndices[i] {
			t.Fatalf("rejected[%d].Index = %d, want %d", i, r.Index, wantRejectedIndices[i])
		}
	}
	if rejected[0].EmailError != "Enter a valid email address." {
		t.Fatalf("rejected[0].EmailError = %q, want 'Enter a valid email address.'", rejected[0].EmailError)
	}
	for i := 1; i <= 3; i++ {
		if rejected[i].PinError != "That PIN is already in use." {
			t.Fatalf("rejected[%d].PinError = %q, want 'That PIN is already in use.'", i, rejected[i].PinError)
		}
	}
	if rejected[4].NameError != "Enter a full name using letters and spaces." {
		t.Fatalf("rejected[4].NameError = %q, want 'Enter a full name using letters and spaces.'", rejected[4].NameError)
	}

	// Re-read the added rows with Get
	for _, a := range added {
		got, err := roster.Get(ctx, a.ID)
		if err != nil || got == nil {
			t.Fatalf("Get(%d) = %+v, %v", a.ID, got, err)
		}
		if got.SourceLinkHash != nil {
			t.Fatalf("SourceLinkHash = %v, want nil", got.SourceLinkHash)
		}
		if got.Cohort != "" {
			t.Fatalf("Cohort = %q, want ''", got.Cohort)
		}
		if !got.IsActive {
			t.Fatalf("IsActive = false, want true")
		}
		if got.Group != "Regular" {
			t.Fatalf("Group = %q, want 'Regular'", got.Group)
		}
	}
	ana, err := roster.Get(ctx, added[0].ID)
	if err != nil || ana.Pin != "0388" {
		t.Fatalf("ana.Pin = %q, want '0388'", ana.Pin)
	}

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	cohorts, err := roster.Cohorts(ctx)
	if err != nil {
		t.Fatalf("Cohorts() err: %v", err)
	}
	if len(cohorts) == 0 || cohorts[len(cohorts)-1] != students.NoCohort {
		t.Fatalf("Cohorts() = %v, want ending with %q", cohorts, students.NoCohort)
	}

	list, err := roster.List(ctx, students.Filter{Cohort: students.NoCohort})
	if err != nil {
		t.Fatalf("List(Cohort: NoCohort): %v", err)
	}
	if len(list) != 2 || list[0].Name != "Luis Rivera" || list[1].Name != "Ana Torres" {
		t.Fatalf("List = %+v, want [Luis Rivera, Ana Torres]", list)
	}

	// A second Add where every entry is invalid writes nothing and leaves calls at 1
	invalidEntries := []students.NewStudent{
		{Name: "Bad Name 123", Email: "bad", Pin: "99"},
	}
	added2, rejected2, err := roster.Add(ctx, "Básico", invalidEntries)
	if err != nil {
		t.Fatalf("Add(invalid): %v", err)
	}
	if len(added2) != 0 || len(rejected2) != 1 {
		t.Fatalf("Add(invalid) added=%d, rejected=%d", len(added2), len(rejected2))
	}
	if calls != 1 {
		t.Fatalf("calls after invalid Add = %d, want 1", calls)
	}
}
