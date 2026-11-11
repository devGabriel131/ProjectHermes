// Package payments defines the three tuition plans, their integer-cent installments, Puerto Rico due-date schedules and money formatting. See docs/billing/overview.md.
package payments

import (
	"errors"
	"fmt"
	"time"
)

// Plan describes an available payment option. Amounts is read-only.
type Plan struct {
	ID, Name string
	Amounts  []int64
}

type Installment struct {
	Date        time.Time
	AmountCents int64
}

var ErrUnknownPlan = errors.New("unknown payment plan")

var plans = []Plan{
	{ID: "full", Name: "Pago Único", Amounts: []int64{60000}},
	{ID: "three", Name: "Plan de 3 Pagos Mensuales", Amounts: []int64{20834, 20833, 20833}},
	{ID: "six", Name: "Plan de 6 Pagos Mensuales", Amounts: []int64{10835, 10833, 10833, 10833, 10833, 10833}},
}

// PuertoRico uses UTC-4 without modern daylight saving time.
var PuertoRico = time.FixedZone("America/Puerto_Rico", -4*60*60)

// Plans returns the ordered registry. Callers must not modify the slice or Amounts.
func Plans() []Plan {
	return plans
}

// Find returns a plan whose Amounts callers must not modify.
func Find(id string) (Plan, bool) {
	for _, plan := range plans {
		if plan.ID == id {
			return plan, true
		}
	}
	return Plan{}, false
}

func (p Plan) TotalCents() int64 {
	var total int64
	for _, amount := range p.Amounts {
		total += amount
	}
	return total
}

// Schedule returns local midnight dates anchored to the original Puerto Rico day.
func (p Plan) Schedule(now time.Time) []Installment {
	year, month, day := now.In(PuertoRico).Date()
	schedule := make([]Installment, len(p.Amounts))
	for i, amount := range p.Amounts {
		targetMonth := month + time.Month(i)
		lastDay := time.Date(year, targetMonth+1, 0, 0, 0, 0, 0, PuertoRico).Day()
		date := time.Date(year, targetMonth, min(day, lastDay), 0, 0, 0, 0, PuertoRico)
		schedule[i] = Installment{Date: date, AmountCents: amount}
	}
	return schedule
}

func Money(cents int64) string {
	if cents < 0 {
		return fmt.Sprintf("-$%d.%02d", -(cents / 100), -(cents % 100))
	}
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

var spanishMonths = [...]string{
	"enero", "febrero", "marzo", "abril", "mayo", "junio",
	"julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre",
}

// SpanishDate formats the calendar date in its existing location.
func SpanishDate(date time.Time) string {
	year, month, day := date.Date()
	return fmt.Sprintf("%d de %s de %d", day, spanishMonths[month-1], year)
}
