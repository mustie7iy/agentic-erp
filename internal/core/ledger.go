package core

import (
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// ValidateEntry is the deterministic check every journal entry must pass,
// whether it came from a migration, a human, or an agent.
func (s *Store) ValidateEntry(je *JournalEntry) error {
	if _, ok := s.Companies[je.CompanyCode]; !ok {
		return fmt.Errorf("entry %s: unknown company code %s", je.ID, je.CompanyCode)
	}
	if _, dup := s.Journal[je.ID]; dup {
		return fmt.Errorf("entry %s: duplicate id", je.ID)
	}
	if len(je.Lines) < 2 {
		return fmt.Errorf("entry %s: needs at least two lines", je.ID)
	}
	var total money.Minor
	for i, l := range je.Lines {
		acct := s.Account(je.CompanyCode, l.Account)
		if acct == nil {
			return fmt.Errorf("entry %s line %d: unknown account %s", je.ID, i+1, l.Account)
		}
		if acct.VendorRecon && l.VendorID == "" {
			return fmt.Errorf("entry %s line %d: account %s requires a vendor", je.ID, i+1, l.Account)
		}
		if l.VendorID != "" {
			if _, ok := s.Vendors[l.VendorID]; !ok {
				return fmt.Errorf("entry %s line %d: unknown vendor %s", je.ID, i+1, l.VendorID)
			}
		}
		total += l.Amount
	}
	if total != 0 {
		return fmt.Errorf("entry %s: unbalanced by %s", je.ID, total)
	}
	return nil
}

func (s *Store) Post(je *JournalEntry) error {
	if err := s.ValidateEntry(je); err != nil {
		return err
	}
	s.Journal[je.ID] = je
	return nil
}

// TrialBalance returns the balance per account (debit positive). asOf is an
// ISO date; empty means all postings.
func (s *Store) TrialBalance(companyCode, asOf string) map[string]money.Minor {
	tb := map[string]money.Minor{}
	for _, je := range s.Journal {
		if je.CompanyCode != companyCode || (asOf != "" && je.PostingDate > asOf) {
			continue
		}
		for _, l := range je.Lines {
			tb[l.Account] += l.Amount
		}
	}
	return tb
}

type OpenItem struct {
	JournalEntryID string      `json:"journalEntryId"`
	Reference      string      `json:"reference,omitempty"`
	PostingDate    string      `json:"postingDate"`
	Amount         money.Minor `json:"amount"`
}

// VendorOpenItems returns uncleared items on vendor reconciliation accounts.
// A negative amount (credit) is money owed to the vendor.
func (s *Store) VendorOpenItems(companyCode string) map[string][]OpenItem {
	out := map[string][]OpenItem{}
	for _, id := range SortedKeys(s.Journal) {
		je := s.Journal[id]
		if je.CompanyCode != companyCode {
			continue
		}
		for _, l := range je.Lines {
			if l.VendorID == "" || l.ClearedBy != "" {
				continue
			}
			if a := s.Account(companyCode, l.Account); a == nil || !a.VendorRecon {
				continue
			}
			out[l.VendorID] = append(out[l.VendorID], OpenItem{
				JournalEntryID: je.ID, Reference: je.Reference, PostingDate: je.PostingDate, Amount: l.Amount,
			})
		}
	}
	return out
}
