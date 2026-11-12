package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Secondary cost elements (ADR-0064 §4, T60g): accounts that only the
// controlling context's allocations post to, each entry balancing on the
// account, so it nets to zero in the company and never carries a
// balance. The database refuses every other line
// (journal_line_secondary_account) and an account that becomes or stops
// being one (gl_account_secondary_kept); the posting paths and the
// configuration's check refuse both first, naming the line or the
// account.

// secondaryPoster is the source context that posts to secondary cost
// elements.
const secondaryPoster = "controlling"

func init() {
	problem.RegisterConstraint("journal_line_secondary_account", func() *problem.Problem {
		return errSecondaryAccount("lines.account_id", "A line posts to a secondary cost element")
	})
	problem.RegisterConstraint("gl_account_secondary_kept", func() *problem.Problem {
		return problem.Conflict("ledger.secondary_account_not_new", "An account is a secondary cost element from its creation and stays one: "+
			"add a new account with cost_element secondary instead of changing an existing account's cost element.")
	})
}

// errSecondaryAccount is the refusal of a line on a secondary cost
// element at field, what is the line.
func errSecondaryAccount(field, what string) *problem.Problem {
	return problem.Unprocessable("ledger.secondary_account", what+", which only allocations post to: an assessment cycle's run charges "+
		"costs on it. Post to a primary cost account instead.",
		problem.FieldError{Field: field, Code: "secondary_cost_element", Message: "an account that is no secondary cost element"})
}

// refuseSecondary refuses the first line on a secondary cost element in
// an entry of a source context other than controlling, and in one of
// controlling a line on it that names no cost center: an allocation names
// the center it charges or relieves, and the account has no default one.
// accounts are the lines' accounts as checkedAccounts read them.
func refuseSecondary(source string, lines []Line, accounts map[uuid.UUID]account) error {
	for i, l := range lines {
		switch {
		case accounts[l.AccountID].element != config.CostElementSecondary:
		case source != secondaryPoster:
			return errSecondaryAccount(fmt.Sprintf("lines[%d].account_id", i), fmt.Sprintf("Line %d posts to a secondary cost element", i+1))
		case l.CostCenterID == nil:
			return problem.Unprocessable("ledger.cost_center_required", fmt.Sprintf("Line %d posts to a secondary cost element and needs a cost center.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].cost_center_id", i), Code: "required", Message: "a cost center of the legal entity"})
		}
	}
	return nil
}

// checkSecondaryNew reports accounts whose cost element the proposal
// changes to or from secondary (ADR-0064 §4, As built T60g): only an
// account the activation itself creates is a secondary cost element, and
// it stays one. An existing account may hold lines other contexts posted,
// or take one in flight while it is turned; a secondary one turned into
// another kind would pass its assessments off as primary costs. The
// database refuses the same at the activation (gl_account_secondary_kept).
func checkSecondaryNew(ctx context.Context, tx pgx.Tx, p string, co config.Company, out *findings) error {
	var numbers, elements []string
	for _, a := range co.ChartOfAccounts {
		numbers, elements = append(numbers, a.Number), append(elements, string(a.CostElementOrDefault()))
	}
	rows, _ := tx.Query(ctx, `SELECT g.number, g.cost_element = $4 FROM ledger.gl_account g
		JOIN ledger.chart_of_accounts c ON c.tenant_id = g.tenant_id AND c.id = g.chart_id AND c.code = $1
		JOIN unnest($2::text[], $3::text[]) AS a(number, element) ON a.number = g.number
		WHERE (g.cost_element = $4) <> (a.element = $4) ORDER BY 1`, co.Code, numbers, elements, config.CostElementSecondary)
	var number string
	var was bool
	_, err := pgx.ForEachRow(rows, []any{&number, &was}, func() error {
		why := "account %s exists already, so it cannot become a secondary cost element; add a new account with cost_element secondary"
		if was {
			why = "account %s is a secondary cost element and stays one once activated"
		}
		out.add(p+".chart_of_accounts["+number+"].cost_element", why, number)
		return nil
	})
	if err != nil {
		return fmt.Errorf("ledger: check company %s: secondary cost elements: %w", co.Code, err)
	}
	return nil
}
