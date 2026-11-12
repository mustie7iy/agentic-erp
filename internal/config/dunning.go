package config

import "fmt"

// Dunning is how a company duns its customers' overdue items (ADR-0065
// §2): its dunning levels, the tenant's own, none shipped in a country
// pack. A company without levels has no dunning: a dunning run of it is
// refused (collections.no_dunning_levels).
type Dunning struct {
	Levels []DunningLevel `yaml:"levels,omitempty" json:"levels,omitempty"`
}

// DunningLevel is one level: an item reaches it once it is DaysOverdue
// days past its net due date. Levels are numbered from 1 without a gap
// and each asks for more days than the one before.
type DunningLevel struct {
	Level       int `yaml:"level" json:"level"`
	DaysOverdue int `yaml:"days_overdue" json:"days_overdue"`
}

// MaxDunningLevels is the most levels a company duns by: a bound, so a
// level is one digit wherever it is kept (collections.dunning_notice).
const MaxDunningLevels = 9

// maxDunningDays is the most days overdue a level asks for: ten years.
const maxDunningDays = 3650

// dunning checks a company's dunning levels: at most MaxDunningLevels,
// numbered from 1 in the order given, each at least a day overdue and
// more days than the level before, so an item's level by its days overdue
// is the last level whose days it has reached.
func (v *validator) dunning(p string, co *Company) {
	p += ".dunning.levels"
	levels := co.Dunning.Levels
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
	}
}
