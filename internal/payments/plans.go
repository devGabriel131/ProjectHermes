// Package payments defines the three tuition plans, their integer-cent installments, Puerto Rico due-date schedules and money formatting. See docs/billing/overview.md.
package payments

import (
	"fmt"
	"time"

	"projecthermes/internal/puertorico"
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

var plans = []Plan{
	{ID: "full", Name: "Pago Único", Amounts: []int64{60000}},
	{ID: "three", Name: "Plan de 3 Pagos Mensuales", Amounts: []int64{20834, 20833, 20833}},
	{ID: "six", Name: "Plan de 6 Pagos Mensuales", Amounts: []int64{10835, 10833, 10833, 10833, 10833, 10833}},
}

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

// InstallmentSpacing, when positive, dates installment i at the anchor plus i spacings instead of on monthly
// midnights, so later installments can be tested in minutes. main sets it from INSTALLMENT_SPACING and refuses a
// live Stripe account; nothing else writes it. Zero is the real monthly schedule.
var InstallmentSpacing time.Duration

// Schedule returns local midnight dates anchored to the original Puerto Rico day, or InstallmentSpacing apart.
func (p Plan) Schedule(now time.Time) []Installment {
	year, month, day := now.In(puertorico.Zone).Date()
	schedule := make([]Installment, len(p.Amounts))
	for i, amount := range p.Amounts {
		if InstallmentSpacing > 0 {
			schedule[i] = Installment{Date: now.Add(time.Duration(i) * InstallmentSpacing).In(puertorico.Zone), AmountCents: amount}
			continue
		}
		targetMonth := month + time.Month(i)
		lastDay := time.Date(year, targetMonth+1, 0, 0, 0, 0, 0, puertorico.Zone).Day()
		date := time.Date(year, targetMonth, min(day, lastDay), 0, 0, 0, 0, puertorico.Zone)
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
