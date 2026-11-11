package payments_test

import (
	"reflect"
	"testing"
	"time"

	"projecthermes/internal/payments"
)

func TestPlans(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, id string
		amounts  []int64
		total    int64
	}{
		{"Pago Único", "full", []int64{60000}, 60000},
		{"Plan de 3 Pagos Mensuales", "three", []int64{20834, 20833, 20833}, 62500},
		{"Plan de 6 Pagos Mensuales", "six", []int64{10835, 10833, 10833, 10833, 10833, 10833}, 65000},
	}
	plans := payments.Plans()
	if len(plans) != len(cases) {
		t.Fatalf("Plans() has %d plans, want %d", len(plans), len(cases))
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, ok := payments.Find(tc.id)
			if !ok {
				t.Fatalf("Find(%q) failed", tc.id)
			}
			if plan.ID != tc.id || plan.Name != tc.name || !reflect.DeepEqual(plan.Amounts, tc.amounts) {
				t.Fatalf("Find(%q) = %+v, want name %q and amounts %v", tc.id, plan, tc.name, tc.amounts)
			}
			if !reflect.DeepEqual(plans[i], plan) {
				t.Errorf("Plans()[%d] = %+v, want %+v", i, plans[i], plan)
			}
			if got := plan.TotalCents(); got != tc.total {
				t.Errorf("TotalCents() = %d, want %d", got, tc.total)
			}
		})
	}
	if plan, ok := payments.Find("unknown"); ok || !reflect.DeepEqual(plan, payments.Plan{}) {
		t.Errorf("Find(unknown) = %+v, %t, want zero plan, false", plan, ok)
	}
}

func TestPlanSchedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, id, now string
		dates         []string
	}{
		{"before PR midnight", "full", "2026-10-01T03:59:59Z", []string{"2026-09-30"}},
		{"at PR midnight", "full", "2026-10-01T04:00:00Z", []string{"2026-10-01"}},
		{"31st common year", "three", "2026-01-31T16:00:00Z", []string{"2026-01-31", "2026-02-28", "2026-03-31"}},
		{"31st leap year", "three", "2028-01-31T16:00:00Z", []string{"2028-01-31", "2028-02-29", "2028-03-31"}},
		{"30th restores after clamp", "three", "2026-01-30T16:00:00Z", []string{"2026-01-30", "2026-02-28", "2026-03-30"}},
		{"29th restores after clamp", "three", "2026-01-29T16:00:00Z", []string{"2026-01-29", "2026-02-28", "2026-03-29"}},
		{"six payments cross year", "six", "2026-10-31T16:00:00Z", []string{"2026-10-31", "2026-11-30", "2026-12-31", "2027-01-31", "2027-02-28", "2027-03-31"}},
		{"six payments cross leap year", "six", "2027-10-31T16:00:00Z", []string{"2027-10-31", "2027-11-30", "2027-12-31", "2028-01-31", "2028-02-29", "2028-03-31"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			plan, ok := payments.Find(tc.id)
			if !ok {
				t.Fatalf("Find(%q) failed", tc.id)
			}
			schedule := plan.Schedule(now)
			if len(schedule) != len(tc.dates) {
				t.Fatalf("Schedule() has %d installments, want %d", len(schedule), len(tc.dates))
			}
			total := int64(0)
			for i, installment := range schedule {
				if got := installment.Date.Format("2006-01-02"); got != tc.dates[i] {
					t.Errorf("installment %d date = %s, want %s", i+1, got, tc.dates[i])
				}
				_, offset := installment.Date.Zone()
				hour, minute, second := installment.Date.Clock()
				if installment.Date.Location().String() != "America/Puerto_Rico" || offset != -4*60*60 || hour != 0 || minute != 0 || second != 0 || installment.Date.Nanosecond() != 0 {
					t.Errorf("installment %d is not Puerto Rico midnight: %s", i+1, installment.Date)
				}
				if installment.AmountCents != plan.Amounts[i] {
					t.Errorf("installment %d amount = %d, want %d", i+1, installment.AmountCents, plan.Amounts[i])
				}
				total += installment.AmountCents
			}
			if total != plan.TotalCents() {
				t.Errorf("schedule total = %d, want %d", total, plan.TotalCents())
			}
		})
	}
}

func TestMoney(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cents int64
		want  string
	}{
		{"zero", 0, "$0.00"},
		{"one cent", 1, "$0.01"},
		{"trailing zero", 10, "$0.10"},
		{"full total", 60000, "$600.00"},
		{"three total", 62500, "$625.00"},
		{"six total", 65000, "$650.00"},
		{"rounded first installment", 20834, "$208.34"},
		{"remaining installment", 10833, "$108.33"},
		{"negative cents", -1, "-$0.01"},
		{"negative dollars", -101, "-$1.01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := payments.Money(tc.cents); got != tc.want {
				t.Errorf("Money(%d) = %q, want %q", tc.cents, got, tc.want)
			}
		})
	}
}

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
			if got := payments.SpanishDate(date); got != tc.want {
				t.Errorf("SpanishDate(%s) = %q, want %q", tc.date, got, tc.want)
			}
		})
	}
}
