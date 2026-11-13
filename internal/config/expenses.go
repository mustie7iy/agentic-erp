package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ExpenseType is a kind of expense an employee claims on an expense
// report (ADR-0074 §2): travel, meals, lodging. A report's line names one
// by its code, and posts to its expense account. The tenant's own: none
// is shipped in a country pack.
type ExpenseType struct {
	Code    string `yaml:"code" json:"code"`
	Name    string `yaml:"name" json:"name"`
	Account string `yaml:"account" json:"account"`
}

// MaxExpenseTypes bounds a company's expense types.
const MaxExpenseTypes = 100

// expenseTypeRe is an expense type's code, as payables.expense_report_line
// keeps it.
var expenseTypeRe = regexp.MustCompile(`^[A-Z0-9_]{1,20}$`)

// ExpenseType returns the company's expense type of the code, if
// configured.
func (co *Company) ExpenseType(code string) (ExpenseType, bool) {
	for _, t := range co.ExpenseTypes {
		if t.Code == code {
			return t, true
		}
	}
	return ExpenseType{}, false
}

// expenseTypes checks a company's expense types: at most
// MaxExpenseTypes, each with a code of its own, a name, and an expense
// account of the company's chart that reconciles nothing, which is what
// its lines post to.
func (v *validator) expenseTypes(p string, co *Company) {
	p += ".expense_types"
	if len(co.ExpenseTypes) > MaxExpenseTypes {
		v.err(p, "at most %d expense types", MaxExpenseTypes)
	}
	seen := map[string]bool{}
	for i, t := range co.ExpenseTypes {
		tp := fmt.Sprintf("%s[%s]", p, t.Code)
		switch {
		case !expenseTypeRe.MatchString(t.Code):
			tp = fmt.Sprintf("%s[%d]", p, i)
			v.err(tp+".code", "must be 1-20 capital letters, digits or underscores")
		case seen[t.Code]:
			v.err(tp, "duplicate expense type code")
		}
		seen[t.Code] = true
		if n := strings.TrimSpace(t.Name); n == "" || len(t.Name) > 60 {
			v.err(tp+".name", "is required, at most 60 characters")
		}
		v.account(tp+".account", co, t.Account, Expense, false)
	}
}
