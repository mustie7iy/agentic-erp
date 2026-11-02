package integration

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/cabank"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// A vendor's bank account (LFBK) becomes the supplier's payments account
// by its bank country: a Canadian one (BANKS CA), which has no IBAN, as
// scheme ca with the institution and transit numbers its bank key holds
// in the Canadian Payments Association's electronic routing form, 0 +
// institution + transit (cabank.SplitMemberID; ADR-0038, T72d); any other
// as an IBAN when it is one, or else a domestic account.

// canadian reports whether the bank account is in Canada.
func canadian(b sap.LFBK) bool { return b.BANKS == "CA" }

// firstBanks is each vendor's first bank account, the one loaded.
func firstBanks(e *sap.Extract) map[string]sap.LFBK {
	banks := map[string]sap.LFBK{}
	for _, b := range e.LFBK {
		if _, ok := banks[b.LIFNR]; !ok {
			banks[b.LIFNR] = b
		}
	}
	return banks
}

// supplierBank is the payments account a vendor's bank account becomes.
// A Canadian bank key that is not a routing number leaves the routing
// empty, which partner refuses; the quality gate refuses it first
// (vendorBanks).
//
// It is valid from the day before today, UTC's day (database.Today): on a
// cutover evening in Toronto UTC is already tomorrow, and a payment run
// dated on the company's day pays only to an account valid on it. Partner
// accepts that one day for the suppliers partner.Migration creates, and
// for nothing else (product owner 2026-11-02; ADR-0051 As built).
func supplierBank(v sap.LFA1, b sap.LFBK, today time.Time) partner.BankInput {
	in := partner.BankInput{Purpose: "payments", AccountNumber: b.BANKN, AccountScheme: "domestic", BankCountryCode: b.BANKS,
		HolderName: v.NAME1, ValidFrom: today.AddDate(0, 0, -1)}
	switch {
	case canadian(b):
		in.AccountScheme = "ca"
		in.InstitutionNumber, in.TransitNumber, _ = cabank.SplitMemberID(b.BANKL)
	case bankfile.ValidIBAN(b.BANKN):
		in.AccountScheme = "iban"
	}
	return in
}

// vendorBanks checks the Canadian bank account each vendor loads with: a
// bank key that is a routing number and an account number of 7 to 12
// digits, as partner requires (T45d). The account number is not repeated
// in the issue: it is sealed on the platform and shown by its last four.
func (q *qualityCheck) vendorBanks() {
	banks := firstBanks(q.e)
	for _, lifnr := range sortedKeys(banks) {
		b := banks[lifnr]
		if !canadian(b) {
			continue
		}
		if _, _, ok := cabank.SplitMemberID(b.BANKL); !ok {
			q.add(SevError, "LFBK", lifnr, "bank key %q of the Canadian bank account is not a routing number as 0 + institution (3 digits) + transit (5 digits)", b.BANKL)
		}
		if !cabank.ValidNumber(cabank.Digits(b.BANKN)) {
			q.add(SevError, "LFBK", lifnr, "the Canadian bank account number is not 7-12 digits")
		}
	}
}

// bankAccount is a payments account as the reconciliation compares it:
// scheme, bank country, Canadian routing and last four characters, or ""
// for none.
func bankAccount(a partner.PaymentAccount) string {
	s := a.Scheme + " " + a.BankCountryCode
	if a.InstitutionNumber != "" || a.TransitNumber != "" {
		s += " " + a.InstitutionNumber + "-" + a.TransitNumber
	}
	return s + " ending " + a.Last4
}

// sapBankAccount is the account a vendor's bank account loads as, in
// bankAccount's form, its number normalized as partner stores it.
func sapBankAccount(v sap.LFA1, b sap.LFBK) string {
	in := supplierBank(v, b, time.Time{})
	number := fieldcrypt.Normalize(in.AccountNumber)
	if in.AccountScheme == "ca" {
		number = cabank.Digits(in.AccountNumber)
	}
	return bankAccount(partner.PaymentAccount{Scheme: in.AccountScheme, BankCountryCode: in.BankCountryCode,
		InstitutionNumber: in.InstitutionNumber, TransitNumber: in.TransitNumber, Last4: fieldcrypt.Last4(number)})
}

// bankAccounts compares the payments bank account of each vendor set up in
// the company: SAP's first one (LFBK), as it loads, with the supplier's
// valid today. A vendor without one in SAP must have none on the platform.
func (rc *reconciler) bankAccounts(ctx context.Context, s companyScope) error {
	vendors := map[string]sap.LFA1{}
	for _, v := range rc.ext.LFA1 {
		vendors[v.LIFNR] = v
	}
	banks := firstBanks(rc.ext)
	sapSide, partnerOf := map[string]string{}, map[string]uuid.UUID{}
	var ids []uuid.UUID
	for _, c := range rc.ext.LFB1 {
		if c.BUKRS != s.cc {
			continue
		}
		if b, ok := banks[c.LIFNR]; ok {
			sapSide[c.LIFNR] = sapBankAccount(vendors[c.LIFNR], b)
		}
		if id, ok := rc.r["LFA1/"+c.LIFNR]; ok {
			partnerOf[c.LIFNR] = id
			ids = append(ids, id)
		}
	}
	accounts, err := rc.e.Suppliers.PaymentAccounts(ctx, ids)
	if err != nil {
		return err
	}
	differ := 0
	for _, c := range rc.ext.LFB1 {
		if c.BUKRS != s.cc {
			continue
		}
		platform := ""
		if a, ok := accounts[partnerOf[c.LIFNR]]; ok {
			platform = bankAccount(a)
		}
		if platform != sapSide[c.LIFNR] {
			differ++
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: s.cc, Key: "bank account of vendor " + sap.Alpha(c.LIFNR),
				SAP: sapSide[c.LIFNR], Platform: platform})
		}
	}
	rc.add(s.cc, "Vendor bank accounts: vendors with differences", "0", strconv.Itoa(differ), differ == 0)
	return nil
}
