//go:build integration

package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"projecthermes/internal/testdb"
)

func TestLinkLifetime(t *testing.T) {
	db := testdb.Open(t, &Settings{})
	store := New(db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	requireLifetime := func(t *testing.T, days int, by string) {
		t.Helper()
		current, err := store.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current.LinkLifetimeDays != days || current.UpdatedBy != by {
			t.Fatalf("current = %d days by %q, want %d by %q", current.LinkLifetimeDays, current.UpdatedBy, days, by)
		}
		if current.ProgramStartDate != "" {
			t.Fatalf("unconfigured program date = %q, want empty", current.ProgramStartDate)
		}
	}

	requireLifetime(t, DefaultLinkLifetimeDays, "") // Nobody has saved yet.
	for _, days := range []int{MinLinkLifetimeDays, MaxLinkLifetimeDays} {
		if err := store.SetLinkLifetimeDays(ctx, days, "alice"); err != nil {
			t.Fatalf("set %d days: %v", days, err)
		}
		requireLifetime(t, days, "alice")
	}

	for _, tc := range []struct {
		name  string
		days  int
		staff string
		want  error
	}{
		{"below range", MinLinkLifetimeDays - 1, "bob", ErrLinkLifetimeRange},
		{"above range", MaxLinkLifetimeDays + 1, "bob", ErrLinkLifetimeRange},
		{"negative", -3, "bob", ErrLinkLifetimeRange},
		{"anonymous", DefaultLinkLifetimeDays, "", errNoStaff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetLinkLifetimeDays(ctx, tc.days, tc.staff); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			requireLifetime(t, MaxLinkLifetimeDays, "alice")
		})
	}

	t.Run("database rejects bypasses", func(t *testing.T) {
		if err := db.WithContext(ctx).Model(&Settings{}).Where("id = 1").Update("link_lifetime_days", 30).Error; err == nil {
			t.Error("stored a 30-day lifetime")
		}
		if err := db.WithContext(ctx).Create(&Settings{ID: 2, LinkLifetimeDays: 7, UpdatedBy: "bob"}).Error; err == nil {
			t.Error("stored a second settings row")
		}
		requireLifetime(t, MaxLinkLifetimeDays, "alice")
	})
}

func TestProgramStartDate(t *testing.T) {
	db := testdb.Open(t, &Settings{})
	store := New(db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	requireSettings := func(t *testing.T, date string, days int, staff string) Settings {
		t.Helper()
		current, err := store.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current.ID != 1 || current.ProgramStartDate != date || current.LinkLifetimeDays != days || current.UpdatedBy != staff {
			t.Fatalf("current = %+v, want date %q, %d days by %q", current, date, days, staff)
		}
		if staff != "" && current.UpdatedAt.IsZero() {
			t.Fatal("saved settings have no update timestamp")
		}
		return current
	}

	requireSettings(t, "", DefaultLinkLifetimeDays, "")
	if err := store.SetProgramStartDate(ctx, " \t2027-01-31\n", "alice"); err != nil {
		t.Fatal(err)
	}
	requireSettings(t, "2027-01-31", DefaultLinkLifetimeDays, "alice")
	if err := store.SetLinkLifetimeDays(ctx, MaxLinkLifetimeDays, "bob"); err != nil {
		t.Fatal(err)
	}
	requireSettings(t, "2027-01-31", MaxLinkLifetimeDays, "bob")
	if err := store.SetProgramStartDate(ctx, "2027-03-15", "carol"); err != nil {
		t.Fatal(err)
	}
	saved := requireSettings(t, "2027-03-15", MaxLinkLifetimeDays, "carol")

	for _, tc := range []struct {
		name  string
		raw   string
		staff string
		want  error
	}{
		{"empty", "", "dave", ErrProgramStartDate},
		{"blank", " \t", "dave", ErrProgramStartDate},
		{"zero year", "0000-01-01", "dave", ErrProgramStartDate},
		{"malformed", "2027-1-31", "dave", ErrProgramStartDate},
		{"impossible", "2027-02-29", "dave", ErrProgramStartDate},
		{"anonymous", "2028-02-29", "", errNoStaff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetProgramStartDate(ctx, tc.raw, tc.staff); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if current := requireSettings(t, saved.ProgramStartDate, saved.LinkLifetimeDays, saved.UpdatedBy); current != saved {
				t.Fatalf("rejected write changed settings: before %+v, after %+v", saved, current)
			}
		})
	}

	t.Run("concurrent independent writes", func(t *testing.T) {
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- store.SetProgramStartDate(ctx, "2028-02-29", "date-editor")
		}()
		go func() {
			<-start
			results <- store.SetLinkLifetimeDays(ctx, MinLinkLifetimeDays, "lifetime-editor")
		}()
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		current, err := store.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current.UpdatedBy != "date-editor" && current.UpdatedBy != "lifetime-editor" {
			t.Fatalf("concurrent write attribution = %q", current.UpdatedBy)
		}
		requireSettings(t, "2028-02-29", MinLinkLifetimeDays, current.UpdatedBy)
	})
}
