package settings

import (
	"errors"
	"testing"
	"time"
)

func TestParseDate(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"canonical", "2027-01-31", "2027-01-31"},
		{"outer whitespace", " \t2028-02-29\n", "2028-02-29"},
		{"first calendar year", "0001-01-01", "0001-01-01"},
		{"empty", "", ""},
		{"blank", " \t\n", ""},
		{"zero year", "0000-01-01", ""},
		{"short year", "027-01-31", ""},
		{"short month", "2027-1-31", ""},
		{"short day", "2027-01-1", ""},
		{"wrong separator", "2027/01/31", ""},
		{"timestamp", "2027-01-31T00:00:00Z", ""},
		{"internal whitespace", "2027- 01-31", ""},
		{"nonleap February", "2027-02-29", ""},
		{"nonleap century", "2100-02-29", ""},
		{"day overflow", "2027-04-31", ""},
		{"zero day", "2027-01-00", ""},
		{"month overflow", "2027-13-01", ""},
		{"zero month", "2027-00-01", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDate(tc.raw)
			if tc.want == "" {
				if !got.IsZero() || !errors.Is(err, ErrDate) {
					t.Fatalf("ParseDate(%q) = %v, %v, want zero and %v", tc.raw, got, err, ErrDate)
				}
				return
			}
			if err != nil || got.Format(time.DateOnly) != tc.want {
				t.Fatalf("ParseDate(%q) = %v, %v, want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}
