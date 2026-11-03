package ledger

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Asset reconciliation accounts (ADR-0057 §3, T57a): their balance is the
// assets' acquisition cost or accumulated depreciation, so only the assets
// context's entries post to them, and a migration run's balances
// (integration). The database refuses every other line
// (journal_line_asset_reconciliation); the posting paths refuse it first,
// naming the line.

func init() {
	problem.RegisterConstraint("journal_line_asset_reconciliation", func() *problem.Problem {
		return errAssetReconciliation("lines.account_id", "A line posts to an asset reconciliation account")
	})
}

// assetPosters are the source contexts that post to asset reconciliation
// accounts.
var assetPosters = map[string]bool{"assets": true, "integration": true}

// errAssetReconciliation is the refusal of a line on an asset
// reconciliation account at field, what is the line.
func errAssetReconciliation(field, what string) *problem.Problem {
	return problem.Unprocessable("ledger.asset_reconciliation_account", what+", which only the assets context posts to: its balance is "+
		"the assets' acquisition cost or accumulated depreciation. Post an acquisition, retirement or transfer of an asset instead.",
		problem.FieldError{Field: field, Code: "asset_reconciliation", Message: "an account that is no asset reconciliation account"})
}

// refuseAssetReconciliation refuses the first line on an asset
// reconciliation account in an entry of a source context other than
// assets and integration. accounts are the lines' accounts as
// checkedAccounts read them.
func refuseAssetReconciliation(source string, lines []Line, accounts map[uuid.UUID]account) error {
	if assetPosters[source] {
		return nil
	}
	for i, l := range lines {
		if accounts[l.AccountID].kind == "asset" {
			return errAssetReconciliation(fmt.Sprintf("lines[%d].account_id", i), fmt.Sprintf("Line %d posts to an asset reconciliation account", i+1))
		}
	}
	return nil
}
