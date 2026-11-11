// Package settings stores staff-configurable options in a single database row.
package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Link lifetimes run from a couple of days up to two weeks.
const (
	MinLinkLifetimeDays     = 2
	MaxLinkLifetimeDays     = 14
	DefaultLinkLifetimeDays = 7
)

// ErrLinkLifetimeRange rejects lifetimes outside the settings slider's range.
var ErrLinkLifetimeRange = fmt.Errorf("settings: link lifetime must be %d-%d days", MinLinkLifetimeDays, MaxLinkLifetimeDays)

// ErrProgramStartDate rejects missing or invalid calendar dates.
var ErrProgramStartDate = errors.New("settings: program start date must be a valid YYYY-MM-DD date")

var errNoStaff = errors.New("settings: staff username required")

// Settings is the only row, ID 1. The single-row and lifetime-range checks repeat the constants, which struct tags can't reference, so neither can be violated even by bypassing the Store. ProgramStartDate is validated only by ParseProgramStartDate.
type Settings struct {
	ID               int       `gorm:"primaryKey;autoIncrement:false;check:settings_single_row,id = 1"`
	LinkLifetimeDays int       `gorm:"not null;check:link_lifetime_days_range,link_lifetime_days BETWEEN 2 AND 14"`
	ProgramStartDate string    `gorm:"type:text;not null;default:''"`
	UpdatedAt        time.Time `gorm:"not null"`
	UpdatedBy        string    `gorm:"not null"`
}

// LinkLifetime is how long a newly created link stays usable.
func (s Settings) LinkLifetime() time.Duration {
	return time.Duration(s.LinkLifetimeDays) * 24 * time.Hour
}

// ParseProgramStartDate validates a nonzero calendar year and returns YYYY-MM-DD.
func ParseProgramStartDate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	date, err := time.Parse(time.DateOnly, raw)
	if err != nil || date.Year() == 0 || date.Format(time.DateOnly) != raw {
		return "", ErrProgramStartDate
	}
	return raw, nil
}

type Store struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Current returns the saved settings, or the defaults before anyone has saved.
// Defaults have no UpdatedBy.
func (s *Store) Current(ctx context.Context) (Settings, error) {
	var current Settings
	result := s.db.WithContext(ctx).Limit(1).Find(&current)
	if result.Error != nil {
		return Settings{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Settings{ID: 1, LinkLifetimeDays: DefaultLinkLifetimeDays}, nil
	}
	return current, nil
}

// SetLinkLifetimeDays saves the lifetime for links created from now on, on
// behalf of staff. Links already issued keep their own expiry.
func (s *Store) SetLinkLifetimeDays(ctx context.Context, days int, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	if days < MinLinkLifetimeDays || days > MaxLinkLifetimeDays {
		return ErrLinkLifetimeRange
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"link_lifetime_days", "updated_at", "updated_by"}),
	}).Create(&Settings{ID: 1, LinkLifetimeDays: days, UpdatedBy: staff}).Error
}

// SetProgramStartDate saves the date shown in new agreements on behalf of staff.
func (s *Store) SetProgramStartDate(ctx context.Context, raw, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	date, err := ParseProgramStartDate(raw)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"program_start_date", "updated_at", "updated_by"}),
	}).Create(&Settings{
		ID:               1,
		LinkLifetimeDays: DefaultLinkLifetimeDays,
		ProgramStartDate: date,
		UpdatedBy:        staff,
	}).Error
}
