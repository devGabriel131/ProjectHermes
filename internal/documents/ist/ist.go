// Package ist describes and grades the IST (Initial Strength Test): a mile run, push-ups, sit-ups,
// a plank hold and body fat estimated from weight and waist. Each event but body fat grades green,
// yellow or red against standards by sex and age band; red fails the event, and any failed event
// fails the test. Students see the standards as reference standards, not current military ones.
// Ported from Masterminds (apps/certificate-worker/internal/ist), which ported the IST-Checker
// prototype and its students.py before it; the tables and formulas are theirs, unchanged.
package ist

import "math"

// Grade is how one event went. Its value doubles as a CSS class suffix.
type Grade string

const (
	Green  Grade = "green"
	Yellow Grade = "yellow"
	Red    Grade = "red"
)

// Pass reports whether g passes its event: anything but red.
func (g Grade) Pass() bool { return g != Red }

// Label is g as students read it, in Spanish.
func (g Grade) Label() string {
	switch g {
	case Green:
		return "Excelente"
	case Yellow:
		return "Aprobado"
	}
	return "No aprobado"
}

// MinAge and MaxAge bound the ages the standards cover.
const MinAge, MaxAge = 17, 51

// Scores is one candidate's IST as measured.
type Scores struct {
	Female       bool
	Age          int // the standards cover MinAge–MaxAge; younger grades as 17–21, older as 47–51
	WeightLbs    int
	WaistIn      int // abdominal circumference at the navel, taken on a relaxed exhale
	RunSeconds   int // the mile
	PushUps      int
	SitUps       int
	PlankSeconds int
}

// Result is a graded IST.
type Result struct {
	AgeBand                     string // "17-21" … "47-51"
	Run, PushUps, SitUps, Plank Grade
	BodyFatPct                  float64 // estimated from weight and waist, rounded to 2 decimals
	MaxBodyFatPct               float64 // the most that passes for the candidate's sex and age
}

// BodyFatPass reports whether body fat is within the limit.
func (r Result) BodyFatPass() bool { return r.BodyFatPct <= r.MaxBodyFatPct }

// Pass reports whether the candidate passed every event.
func (r Result) Pass() bool {
	return r.Run.Pass() && r.PushUps.Pass() && r.SitUps.Pass() && r.Plank.Pass() && r.BodyFatPass()
}

// Evaluate grades s.
func Evaluate(s Scores) Result {
	st := StandardsFor(s.Female, s.Age)
	weight, waist := float64(s.WeightLbs), float64(s.WaistIn)
	fat := -26.97 - 0.12*weight + 1.99*waist
	if s.Female {
		fat = -9.15 - 0.015*weight + 1.27*waist
	}
	return Result{
		AgeBand:       st.AgeBand,
		Run:           st.Run.atMost(s.RunSeconds),
		PushUps:       st.PushUps.atLeast(s.PushUps),
		SitUps:        st.SitUps.atLeast(s.SitUps),
		Plank:         st.Plank.atLeast(s.PlankSeconds),
		BodyFatPct:    round2(fat),
		MaxBodyFatPct: st.MaxBodyFatPct,
	}
}

// Target is one event's two lines: Pass is the least that passes (yellow), Green the least that
// is green. Counted events (reps, plank seconds) reach a line from it up; the mile, in seconds,
// from it down.
type Target struct{ Pass, Green int }

// atLeast grades a count against t.
func (t Target) atLeast(n int) Grade {
	switch {
	case n >= t.Green:
		return Green
	case n >= t.Pass:
		return Yellow
	}
	return Red
}

// atMost grades a time against t.
func (t Target) atMost(seconds int) Grade {
	switch {
	case seconds <= t.Green:
		return Green
	case seconds <= t.Pass:
		return Yellow
	}
	return Red
}

// Standards is what one sex must reach in one age band.
type Standards struct {
	AgeBand         string // "17-21" … "47-51"
	Run             Target // the mile, in seconds; the same at every age
	PushUps, SitUps Target
	Plank           Target // seconds held
	MaxBodyFatPct   float64
}

// StandardsFor returns the standards a candidate of that sex and age is graded against.
func StandardsFor(female bool, age int) Standards { return bandFor(age).standards(female) }

// AllStandards returns one sex's standards for every age band, youngest first.
func AllStandards(female bool) []Standards {
	all := make([]Standards, len(bands))
	for i, b := range bands {
		all[i] = b.standards(female)
	}
	return all
}

// The mile's lines by sex, the same at every age.
var (
	maleRun   = Target{Pass: 8*60 + 30, Green: 7*60 + 45}
	femaleRun = Target{Pass: 10*60 + 30, Green: 9*60 + 45}
)

// standard is a counted event's table entry: min passes, and the green line is the midpoint of
// min and max, rounded down.
type standard struct{ min, max int }

func (st standard) target() Target { return Target{Pass: st.min, Green: (st.min + st.max) / 2} }

// band is one age band's standards. Plank and sit-ups are the same for both sexes.
type band struct {
	label                      string
	maxAge                     int
	plank, sitUps              standard
	malePushUps, femalePushUps standard
	maleMaxFat, femaleMaxFat   float64
}

func (b band) standards(female bool) Standards {
	st := Standards{AgeBand: b.label, Run: maleRun, PushUps: b.malePushUps.target(), SitUps: b.sitUps.target(),
		Plank: b.plank.target(), MaxBodyFatPct: b.maleMaxFat}
	if female {
		st.Run, st.PushUps, st.MaxBodyFatPct = femaleRun, b.femalePushUps.target(), b.femaleMaxFat
	}
	return st
}

var bands = []band{
	// label  age plank (s)          sit-ups           push-ups men      push-ups women    max body fat % men, women
	{"17-21", 21, standard{90, 220}, standard{53, 78}, standard{42, 71}, standard{19, 42}, 20, 30},
	{"22-26", 26, standard{85, 215}, standard{50, 80}, standard{40, 75}, standard{17, 46}, 22, 32},
	{"27-31", 31, standard{80, 210}, standard{45, 82}, standard{39, 77}, standard{17, 50}, 24, 34},
	{"32-36", 36, standard{75, 205}, standard{42, 76}, standard{36, 75}, standard{15, 45}, 24, 34},
	{"37-41", 41, standard{70, 200}, standard{38, 76}, standard{34, 73}, standard{13, 40}, 24, 34},
	{"42-46", 46, standard{70, 200}, standard{32, 72}, standard{30, 66}, standard{12, 37}, 26, 36},
	{"47-51", 51, standard{70, 200}, standard{30, 66}, standard{25, 59}, standard{12, 37}, 26, 36},
}

// bandFor is the first band age fits, or the oldest.
func bandFor(age int) band {
	for _, b := range bands {
		if age <= b.maxAge {
			return b
		}
	}
	return bands[len(bands)-1]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
