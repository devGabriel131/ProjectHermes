package ist

import "testing"

// Evaluate decides who passes the IST, so its standards and formulas must hold. Each case sits on
// the lines between grades; the expected values match Masterminds' grading.
func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		s    Scores
		want Result
		pass bool
	}{
		{
			name: "male 21 on every green line",
			s:    Scores{Age: 21, WeightLbs: 160, WaistIn: 32, RunSeconds: 465, PushUps: 56, SitUps: 65, PlankSeconds: 155},
			want: Result{AgeBand: "17-21", Run: Green, PushUps: Green, SitUps: Green, Plank: Green, BodyFatPct: 17.51, MaxBodyFatPct: 20},
			pass: true,
		},
		{
			name: "male 22 on every yellow floor",
			s:    Scores{Age: 22, WeightLbs: 200, WaistIn: 36, RunSeconds: 510, PushUps: 40, SitUps: 50, PlankSeconds: 85},
			want: Result{AgeBand: "22-26", Run: Yellow, PushUps: Yellow, SitUps: Yellow, Plank: Yellow, BodyFatPct: 20.67, MaxBodyFatPct: 22},
			pass: true,
		},
		{
			name: "female 26 on green lines and just under them",
			s:    Scores{Female: true, Age: 26, WeightLbs: 130, WaistIn: 30, RunSeconds: 585, PushUps: 31, SitUps: 64, PlankSeconds: 149},
			want: Result{AgeBand: "22-26", Run: Green, PushUps: Green, SitUps: Yellow, Plank: Yellow, BodyFatPct: 27, MaxBodyFatPct: 32},
			pass: true,
		},
		{
			name: "female 47 one under every floor",
			s:    Scores{Female: true, Age: 47, WeightLbs: 150, WaistIn: 38, RunSeconds: 631, PushUps: 11, SitUps: 29, PlankSeconds: 69},
			want: Result{AgeBand: "47-51", Run: Red, PushUps: Red, SitUps: Red, Plank: Red, BodyFatPct: 36.86, MaxBodyFatPct: 36},
		},
		{
			name: "body fat alone fails the test",
			s:    Scores{Female: true, Age: 17, WeightLbs: 150, WaistIn: 35, RunSeconds: 300, PushUps: 42, SitUps: 78, PlankSeconds: 220},
			want: Result{AgeBand: "17-21", Run: Green, PushUps: Green, SitUps: Green, Plank: Green, BodyFatPct: 33.05, MaxBodyFatPct: 30},
		},
	}
	for _, tt := range tests {
		got := Evaluate(tt.s)
		if got != tt.want || got.Pass() != tt.pass {
			t.Errorf("%s: Evaluate = %+v, pass %v; want %+v, pass %v", tt.name, got, got.Pass(), tt.want, tt.pass)
		}
	}
}
