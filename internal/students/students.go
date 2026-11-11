// Package students owns the Student record, the contact-field registry whose parsers serve submit, autosave, customer edit and staff edit, and the municipality list. See docs/domain/students.md.
package students

import (
	"context"
	"errors"
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
	ID             uint64 `gorm:"primaryKey"`
	SourceLinkHash []byte `gorm:"not null;uniqueIndex;check:student_source_hash_size,octet_length(source_link_hash) = 32"`
	Details
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	IsActive  bool      `gorm:"not null;default:true"`
	// Pin is Socrative's credential for the student. A Link holds it from submit until payment;
	// Enroll then moves it here. The partial index keeps rows without a PIN from colliding.
	// The index name is pinIndex.
	Pin string `gorm:"not null;default:'';uniqueIndex:idx_student_pin,where:pin <> ''"`
	// Group is the program stage staff edit through groupField. Enroll leaves it to the column
	// default, so every student starts in the first group; the default and check repeat groups.
	Group string `gorm:"not null;default:'Básico';check:student_group_valid,\"group\" IN ('Básico', 'Regular')"`
	// Cohort is the month, YYYY-MM, of the agreement start date the student accepted. Enroll sets it
	// and no edit reaches it, so it stays the month the student started after any promotion.
	Cohort string `gorm:"type:text;not null;check:student_cohort_month,cohort ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'"`
}

// CohortLabel names a cohort month, YYYY-MM, such as "November 2026".
func CohortLabel(cohort string) string {
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
	Cohort string // a YYYY-MM month
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
		query = query.Where("cohort = ?", filter.Cohort)
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

// Cohorts returns every cohort that has a student, newest first.
func (s *Store) Cohorts(ctx context.Context) ([]string, error) {
	var cohorts []string
	err := s.db.WithContext(ctx).Model(&Student{}).Distinct().Order("cohort DESC").Pluck("cohort", &cohorts).Error
	return cohorts, err
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

// Get returns nil for an absent student, including IDs outside PostgreSQL BIGINT.
func (s *Store) Get(ctx context.Context, id uint64) (*Student, error) {
	if id == 0 || id > 1<<63-1 {
		return nil, nil
	}
	var record Student
	result := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&record)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &record, nil
}

// pinHeld reports whether a Student or a submitted Link still awaiting payment holds pin.
// links imports this package, so the table is queried by name without importing links.
func pinHeld(db *gorm.DB, pin string) (bool, error) {
	var held bool
	err := db.Raw("SELECT EXISTS (SELECT 1 FROM students WHERE pin = ?) OR EXISTS (SELECT 1 FROM links WHERE pin = ?)", pin, pin).Scan(&held).Error
	return held, err
}

// PinTaken reports whether pin is held by an enrolled student or a submitted link awaiting payment.
// The unique indexes are the authority at write time; this lets the customer form warn before final submit.
func (s *Store) PinTaken(ctx context.Context, pin string) (bool, error) {
	return pinHeld(s.db.WithContext(ctx), pin)
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
	if id == 0 || id > 1<<63-1 {
		return nil, nil
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
			held, err := pinHeld(tx, value)
			if err != nil {
				return err
			}
			if held {
				return &ValidationError{Message: "That PIN is already in use."}
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
		return nil, &ValidationError{Message: "That PIN is already in use."}
	}
	if err != nil {
		return nil, err
	}
	if wrote {
		s.changed()
	}
	return persisted, nil
}
