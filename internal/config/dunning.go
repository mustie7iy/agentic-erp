package config

import (
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Dunning is how a company duns its customers' overdue items (ADR-0065
// §2): its dunning levels, the tenant's own, none shipped in a country
// pack. A company without levels has no dunning: a dunning run of it is
// refused (collections.no_dunning_levels).
//
// The rest is the company's dunning procedure (T76d, ADR-0065 As built):
// GraceDays, the days an item may be overdue before a run takes it;
// MinimumAmount, the overdue amount below which a customer's notice is
// not proposed (a decimal in the company's currency, empty: none); and
// InterestRatePercent, the annual rate of interest the levels that charge
// interest charge (a decimal of at most three places, empty: none).
type Dunning struct {
	GraceDays           int            `yaml:"grace_days,omitempty" json:"grace_days,omitempty"`
	MinimumAmount       string         `yaml:"minimum_amount,omitempty" json:"minimum_amount,omitempty"`
	InterestRatePercent string         `yaml:"interest_rate_percent,omitempty" json:"interest_rate_percent,omitempty"`
	Levels              []DunningLevel `yaml:"levels,omitempty" json:"levels,omitempty"`
}

// DunningLevel is one level: an item reaches it once it is DaysOverdue
// days past its net due date. Levels are numbered from 1 without a gap
// and each asks for more days than the one before. Charge is what a
// notice at the level charges (a decimal in the company's currency,
// empty: nothing); Interest, whether its items are charged interest at
// the company's rate (T76d).
type DunningLevel struct {
	Level       int    `yaml:"level" json:"level"`
	DaysOverdue int    `yaml:"days_overdue" json:"days_overdue"`
	Charge      string `yaml:"charge,omitempty" json:"charge,omitempty"`
	Interest    bool   `yaml:"interest,omitempty" json:"interest,omitempty"`
}

// MaxDunningLevels is the most levels a company duns by: a bound, so a
// level is one digit wherever it is kept (collections.dunning_notice).
const MaxDunningLevels = 9

// maxDunningDays is the most days overdue a level asks for: ten years.
const maxDunningDays = 3650

// MaxGraceDays is the most grace days a company gives: a year.
const MaxGraceDays = 365

// InterestRateDecimals are the decimal places of a dunning interest rate.
const InterestRateDecimals = 3

// maxInterestRate is the bound a dunning interest rate stays below: 100%
// a year, in thousandths of a percent.
const maxInterestRate = 100_000

// dunning checks a company's dunning: at most MaxDunningLevels levels,
// numbered from 1 in the order given, each at least a day overdue and
// more days than the level before, so an item's level by its days overdue
// is the last level whose days it has reached; and its procedure (T76d):
// grace days 0-MaxGraceDays, the minimum amount and the levels' charges
// amounts of the company's currency not below zero, the interest rate
// above zero and below 100% with at most three places, and set when a
// level charges interest.
func (v *validator) dunning(p string, co *Company) {
	p += ".dunning"
	d := co.Dunning
	exp, known := v.exponents[co.Currency]
	if d.GraceDays < 0 || d.GraceDays > MaxGraceDays {
		v.err(p+".grace_days", "must be 0-%d days", MaxGraceDays)
	}
	if d.MinimumAmount != "" && known {
		v.amount(p+".minimum_amount", d.MinimumAmount, exp)
	}
	if d.InterestRatePercent != "" {
		if r, ok := v.amount(p+".interest_rate_percent", d.InterestRatePercent, InterestRateDecimals); ok && (r == 0 || r >= maxInterestRate) {
			v.err(p+".interest_rate_percent", "a rate above 0%% and below 100%% a year")
		}
	}
	v.dunningLevels(p+".levels", d, exp, known)
}

// dunningLevels checks the levels of a company's dunning (dunning).
func (v *validator) dunningLevels(p string, d Dunning, exp int, known bool) {
	levels := d.Levels
	if len(levels) > MaxDunningLevels {
		v.err(p, "at most %d dunning levels", MaxDunningLevels)
	}
	for i, l := range levels {
		lp := fmt.Sprintf("%s[%d]", p, l.Level)
		if l.Level != i+1 {
			v.err(lp+".level", "levels are numbered from 1 in order: this is level %d", i+1)
		}
		switch {
		case l.DaysOverdue < 1 || l.DaysOverdue > maxDunningDays:
			v.err(lp+".days_overdue", "must be 1-%d days", maxDunningDays)
		case i > 0 && l.DaysOverdue <= levels[i-1].DaysOverdue:
			v.err(lp+".days_overdue", "each level asks for more days overdue than the level before (%d)", levels[i-1].DaysOverdue)
		}
		if l.Charge != "" && known {
			v.amount(lp+".charge", l.Charge, exp)
		}
		if l.Interest && d.InterestRatePercent == "" {
			v.err(lp+".interest", "a level charges interest at the company's rate: set dunning.interest_rate_percent")
		}
	}
}

// Minimum is the company's dunning minimum amount in minor units of its
// currency, whose digits are exp; 0 without one. Validated amounts only:
// one that does not parse is 0.
func (d Dunning) Minimum(exp int) money.Minor {
	m, _ := money.ParseDecimal(d.MinimumAmount, exp) // validated, so only an empty one (none) fails, and gives 0
	return m
}

// ChargeOf is a level's dunning charge in minor units of the company's
// currency, whose digits are exp; 0 without one, as Minimum reads it.
func (l DunningLevel) ChargeOf(exp int) money.Minor {
	m, _ := money.ParseDecimal(l.Charge, exp) // validated, so only an empty one (none) fails, and gives 0
	return m
}

// InterestRate is the company's annual dunning interest rate in
// thousandths of a percent (9.5% is 9500); 0 without one, as Minimum
// reads it.
func (d Dunning) InterestRate() int64 {
	r, _ := money.ParseDecimal(d.InterestRatePercent, InterestRateDecimals) // validated, so only an empty one (none) fails, and gives 0
	return int64(r)
}
