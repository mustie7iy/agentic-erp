package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The account kinds of a valuation class (config.InventoryAccounts) a
// movement type posts its value against (ADR-0082 §8; SAP's GBB
// modifiers ZOB, BSA, VBR and VNG, from memory); a transfer posts against
// none but stock and price differences.
var counterKinds = map[string]string{"501": config.InventoryReceipt, "502": config.InventoryReceipt, "561": config.InventoryInitialEntry,
	"562": config.InventoryInitialEntry, "201": config.InventoryConsumption, "202": config.InventoryConsumption, "551": config.InventoryScrapping,
	"552": config.InventoryScrapping}

// entryRef is the journal entry a document or price change posted; all
// nil when it posted none.
type entryRef struct {
	id     *uuid.UUID
	year   *int16
	number *string
}

// entryOf is the entry the ledger posted.
func entryOf(e ledger.Entry) entryRef {
	return entryRef{id: &e.ID, year: &e.FiscalYear, number: &e.DocumentNumber}
}

// accounts finds the company's accounts of valuation classes by kind,
// once each, refusing a posting a class lacks one for (422
// inventory.no_inventory_account) before anything is posted.
type accounts struct {
	e       *Endpoints
	company uuid.UUID
	code    string
	found   map[string]uuid.UUID
}

func (e *Endpoints) accounts(company uuid.UUID, code string) *accounts {
	return &accounts{e: e, company: company, code: code, found: map[string]uuid.UUID{}}
}

func (a *accounts) of(ctx context.Context, kind, class string) (uuid.UUID, error) {
	key := config.InventoryKey(kind, class)
	if id, ok := a.found[key]; ok {
		return id, nil
	}
	id, ok, err := a.e.Accounts.Determination(ctx, a.company, key)
	if err != nil {
		return uuid.Nil, fmt.Errorf("inventory: %s account of valuation class %s in company %s: %w", kind, class, a.code, err)
	}
	if !ok {
		return uuid.Nil, problem.Unprocessable("inventory.no_inventory_account", "Company "+a.code+"'s valuation class "+class+" has no "+kind+
			" account; the movement posts once the configuration names it (companies[].account_determination.inventory).")
	}
	a.found[key] = id
	return id, nil
}

// entryLines are the journal lines of a document's valued lines: each
// line's value on its class's stock account with its quantity, its
// price difference on the class's price difference account, and the
// rest on the account its movement type posts against, with the line's
// cost center. A line of no value posts nothing.
func (e *Endpoints) entryLines(ctx context.Context, in posting, vals map[valuationKey]*Valuation, r refs) ([]ledger.PostingLine, error) {
	a := e.accounts(in.company.ID, in.company.Code)
	mt := movementTypes[in.movementType]
	var out []ledger.PostingLine
	add := func(kind, class string, amount money.Minor, line ledger.PostingLine) error {
		if amount == 0 {
			return nil
		}
		id, err := a.of(ctx, kind, class)
		if err != nil {
			return err
		}
		line.AccountID, line.Amount = id, amount
		out = append(out, line)
		return nil
	}
	for _, l := range in.lines {
		class := vals[valuationKey{l.MaterialID, l.PlantID}].ValuationClass
		qty, unit := l.Quantity, r.materials[l.MaterialID].BaseUnit
		if err := add(config.InventoryStock, class, l.Amount, ledger.PostingLine{Quantity: &qty, Unit: &unit}); err != nil {
			return nil, err
		}
		if err := add(config.InventoryPriceDifference, class, l.Difference, ledger.PostingLine{}); err != nil {
			return nil, err
		}
		if mt.transfer == noTransfer {
			if err := add(counterKinds[in.movementType], class, -(l.Amount + l.Difference), ledger.PostingLine{CostCenterID: l.CostCenterID}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// post posts the document's value through the ledger in its company's
// currency (ADR-0082 §8): one entry, document type WE for a receipt or
// initial entry and its reversal, WA for the rest, dated the document's
// day. A transfer between storage locations of a plant moves no value
// out of the valuation area and posts none, as SAP's 311 posts no
// accounting document (from memory).
// The valuations are of the company's currency, which they were created in.
func (e *Endpoints) post(ctx context.Context, in posting, id uuid.UUID, number string, vals map[valuationKey]*Valuation, r refs) (entryRef, error) {
	if movementTypes[in.movementType].transfer == betweenStorageLocations {
		return entryRef{}, nil
	}
	lines, err := e.entryLines(ctx, in, vals, r)
	if err != nil || len(lines) == 0 {
		return entryRef{}, err
	}
	docType := "WA"
	if k := counterKinds[in.movementType]; k == config.InventoryReceipt || k == config.InventoryInitialEntry {
		docType = "WE"
	}
	text := "Material document " + number + " (" + in.movementType + " " + movementTypes[in.movementType].name + ")"
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: in.company.ID, DocumentType: docType, DocumentDate: in.day, PostingDate: in.day,
		Currency: in.company.LocalCurrency, Reference: &number, HeaderText: &text, SourceContext: "inventory", SourceDocumentID: id, Lines: lines})
	if err != nil {
		return entryRef{}, fmt.Errorf("inventory: post material document %s: %w", number, err)
	}
	return entryOf(entry), nil
}
