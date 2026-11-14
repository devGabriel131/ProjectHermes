package customer

import (
	"testing"
	"time"
)

func TestSpanishDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, date, want string
	}{
		{"program date", "2026-09-30T00:00:00Z", "30 de septiembre de 2026"},
		{"UTC calendar date stays intact", "2026-01-01T00:00:00Z", "1 de enero de 2026"},
		{"local calendar date stays intact", "2026-12-31T23:59:59-04:00", "31 de diciembre de 2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			date, err := time.Parse(time.RFC3339, tc.date)
			if err != nil {
				t.Fatal(err)
			}
			if got := spanishDate(date); got != tc.want {
				t.Errorf("spanishDate(%s) = %q, want %q", tc.date, got, tc.want)
			}
		})
	}
}
