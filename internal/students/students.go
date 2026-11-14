// Package students owns the Student record, the contact-field registry whose parsers serve submit, autosave, customer edit and staff edit, and the municipality list. See docs/domain/students.md.
package students

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Details struct {
	Name         string `gorm:"not null;default:''"`
	Email        string `gorm:"not null;default:''"`
	Phone        string `gorm:"not null;default:''"`
	Municipality string `gorm:"not null;default:''"`
	DateOfBirth  string `gorm:"type:text;not null;default:''"`
	Gender       string `gorm:"not null;default:''"`
}

type Student struct {
	ID uint64 `gorm:"primaryKey"`
	// EnrollmentID is nil for students staff add (Add), who have no customer enrollment.
	EnrollmentID *uint64 `gorm:"uniqueIndex"`
	Details
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	IsActive  bool      `gorm:"not null;default:true"`
	// Pin is Socrative's credential for the student. A Link holds it from submit until payment;
	// Enroll then moves it here. The partial index keeps rows without a PIN from colliding.
	// The index name is pinIndex.
	Pin string `gorm:"not null;default:'';uniqueIndex:idx_student_pin,where:pin <> ''"`
	// Group is the program stage staff edit through groupField. Enroll leaves it to the column
	// default, the first group; Add uses the group staff chose. The default and check repeat groups.
	Group string `gorm:"not null;default:'Básico';check:student_group_valid,\"group\" IN ('Básico', 'Regular')"`
	// Cohort is the month, YYYY-MM, of the agreement start date the student accepted. Enroll sets it;
	// students added by staff have ''; no edit reaches it.
	Cohort string `gorm:"type:text;not null;check:student_cohort_valid,cohort = '' OR cohort ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'"`
}

// NoCohort is the filter value and ?cohort= URL token for students without a cohort;
// ” is what is stored, and ” in a filter means every cohort.
const NoCohort = "none"

const noCohortLabel = "Existing students"

// CohortLabel names a cohort month, YYYY-MM, such as "November 2026", or "Existing students"
// for students without a cohort.
func CohortLabel(cohort string) string {
	if cohort == "" || cohort == NoCohort {
		return noCohortLabel
	}
	month, err := time.Parse("2006-01", cohort)
	if err != nil {
		return cohort // the check constraint makes this unreachable for stored rows
	}
	return month.Format("January 2006")
}

type Store struct {
	db *gorm.DB
	// changed runs after every committed write, so open management pages refresh.
	changed func()
}

// New returns a store whose committed writes call changed, when it is non-nil.
func New(db *gorm.DB, changed func()) *Store {
	if changed == nil {
		changed = func() {}
	}
	return &Store{db: db, changed: changed}
}

// Filter narrows List. Its zero value keeps every student; a student must match each set field.
type Filter struct {
	Active *bool  // is_active equals it when non-nil
	Cohort string // a YYYY-MM month, or NoCohort
	Group  string // one of GroupOptions
}

// List returns retained students matching filter, newest first, regardless of source access.
// ponytail: unbounded small mock history; paginate before sustained growth.
func (s *Store) List(ctx context.Context, filter Filter) ([]Student, error) {
	query := s.db.WithContext(ctx).Order("created_at DESC, id DESC")
	if filter.Active != nil {
		query = query.Where("is_active = ?", *filter.Active)
	}
	if filter.Cohort != "" {
		if filter.Cohort == NoCohort {
			query = query.Where("cohort = ''")
		} else {
			query = query.Where("cohort = ?", filter.Cohort)
		}
	}
	if filter.Group != "" {
		query = query.Where(`"group" = ?`, filter.Group)
	}
	var records []Student
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// Cohorts returns every cohort that has a student: months newest first, then NoCohort
// when staff-added students exist.
func (s *Store) Cohorts(ctx context.Context) ([]string, error) {
	var cohorts []string
	err := s.db.WithContext(ctx).Model(&Student{}).Distinct().Order("cohort DESC").Pluck("cohort", &cohorts).Error
	if err != nil {
		return nil, err
	}
	if len(cohorts) > 0 && cohorts[len(cohorts)-1] == "" {
		cohorts[len(cohorts)-1] = NoCohort
	}
	return cohorts, nil
}

// Counts is the roster's headcount.
type Counts struct{ Active, Suspended int }

// Total is every retained student, active or not.
func (c Counts) Total() int { return c.Active + c.Suspended }

// Count returns the active and suspended headcounts.
func (s *Store) Count(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.db.WithContext(ctx).Model(&Student{}).Select(
		"count(*) FILTER (WHERE is_active) AS active, count(*) FILTER (WHERE NOT is_active) AS suspended",
	).Scan(&c).Error
	return c, err
}

// Get returns nil for an absent student.
func (s *Store) Get(ctx context.Context, id uint64) (*Student, error) {
	var record Student
	result := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&record)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &record, nil
}

const pinTakenMessage = "That PIN is already in use."

// heldPins reports which of the given pins are held by a Student or a submitted Enrollment still awaiting payment.
// links imports this package, so the table is queried by name without importing links.
func heldPins(db *gorm.DB, pins []string) (map[string]bool, error) {
	var found []string
	err := db.Raw("SELECT pin FROM students WHERE pin IN ? UNION SELECT pin FROM enrollments WHERE pin IN ?", pins, pins).Scan(&found).Error
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(found))
	for _, p := range found {
		held[p] = true
	}
	return held, nil
}

// PinTaken reports whether pin is held by an enrolled student or a submitted link awaiting payment.
// The unique indexes are the authority at write time; this lets the customer form warn before final submit.
func (s *Store) PinTaken(ctx context.Context, pin string) (bool, error) {
	held, err := heldPins(s.db.WithContext(ctx), []string{pin})
	return held[pin], err
}

// pinIndex names Student.Pin's unique index.
const pinIndex = "idx_student_pin"

// IsUniqueViolation reports whether err is a unique violation on the named index. The driver error
// exposes its SQLSTATE through a method but the constraint name only in its message, which
// carries no row values.
func IsUniqueViolation(err error, index string) bool {
	var database interface{ SQLState() string }
	return errors.As(err, &database) && database.SQLState() == "23505" && strings.Contains(err.Error(), index)
}

var ErrUnknownField = errors.New("students: unknown field")

// UpdateField changes only the registered column and returns its persisted row.
// Normalized no-ops return the current row without writing or notifying.
func (s *Store) UpdateField(ctx context.Context, id uint64, key, raw string) (*Student, error) {
	field, ok := FindStaffField(key)
	if !ok {
		return nil, ErrUnknownField
	}
	value, err := field.Parse(raw, true)
	if err != nil {
		return nil, err
	}
	var persisted *Student
	wrote := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record Student
		// Lock the current row so comparison and write share one version.
		result := tx.Where("id = ?", id).
			Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Limit(1).Find(&record)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if field.Value(record) == value {
			persisted = &record
			return nil
		}
		if field.Key == pinField.Key {
			held, err := heldPins(tx, []string{value})
			if err != nil {
				return err
			}
			if held[value] {
				return &ValidationError{Message: pinTakenMessage}
			}
		}
		result = tx.Model(&record).
			Clauses(clause.Returning{}).Update(field.Key, value)
		// Zero rows after the locked read means the database suppressed the write (a trigger): not a save.
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		persisted, wrote = &record, true
		return nil
	})
	if IsUniqueViolation(err, pinIndex) {
		return nil, &ValidationError{Message: pinTakenMessage}
	}
	if err != nil {
		return nil, err
	}
	if wrote {
		s.changed()
	}
	return persisted, nil
}

// NewStudent is what staff enter to add a student without a customer link.
type NewStudent struct{ Name, Email, Pin string }

// Rejection is an entry Add refused. Each error holds that field's validation message, "" when it passed.
type Rejection struct {
	Index                           int // position in the entries passed to Add
	NameError, EmailError, PinError string
}

// Problems lists the rejection's messages in field order: name, email, PIN.
func (r Rejection) Problems() []string {
	var problems []string
	if r.NameError != "" {
		problems = append(problems, r.NameError)
	}
	if r.EmailError != "" {
		problems = append(problems, r.EmailError)
	}
	if r.PinError != "" {
		problems = append(problems, r.PinError)
	}
	return problems
}

// Add inserts each valid entry as an active student in group, with no source link and no cohort, in one
// transaction, and returns the added students in entry order plus every rejection in entry order.
// group must come from ParseGroup; the student_group_valid check refuses anything else.
func (s *Store) Add(ctx context.Context, group string, entries []NewStudent) ([]Student, []Rejection, error) {
	type candidate struct {
		index   int
		student Student
	}
	var candidates []candidate
	var rejections []Rejection
	var pins []string
	for i, e := range entries {
		rejection := Rejection{Index: i}
		name, err := ParseName(e.Name, true)
		if err != nil {
			rejection.NameError = err.Error()
		}
		email, err := ParseEmail(e.Email, true)
		if err != nil {
			rejection.EmailError = err.Error()
		}
		pin, err := ParsePin(e.Pin, true)
		if err != nil {
			rejection.PinError = err.Error()
		}
		if len(rejection.Problems()) > 0 {
			rejections = append(rejections, rejection)
			continue
		}
		pins = append(pins, pin)
		candidates = append(candidates, candidate{index: i, student: Student{
			Details: Details{Name: name, Email: email}, Pin: pin, IsActive: true, Group: group,
		}})
	}
	if len(candidates) == 0 {
		return nil, rejections, nil
	}

	var inserted []Student
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// held starts as the PINs already taken and gains each one this batch takes, so a repeat within the batch is refused too.
		held, err := heldPins(tx, pins)
		if err != nil {
			return err
		}
		for _, c := range candidates {
			if held[c.student.Pin] {
				rejections = append(rejections, Rejection{Index: c.index, PinError: pinTakenMessage})
				continue
			}
			held[c.student.Pin] = true
			inserted = append(inserted, c.student)
		}
		if len(inserted) == 0 {
			return nil
		}
		return tx.CreateInBatches(&inserted, 500).Error
	})

	if IsUniqueViolation(err, pinIndex) {
		return nil, nil, &ValidationError{Message: "A PIN was just taken by someone else. Please try again."}
	}
	if err != nil {
		return nil, nil, err
	}

	slices.SortFunc(rejections, func(a, b Rejection) int {
		return cmp.Compare(a.Index, b.Index)
	})

	if len(inserted) > 0 {
		s.changed()
	}

	return inserted, rejections, nil
}
