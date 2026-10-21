package bankstatement

import (
	"regexp"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/iban"
)

// mtField is one field of an MT940 message: its tag and its value, the
// lines of a multi-line value joined by "\n".
type mtField struct {
	tag, value string
	lines      []string // the value's lines, joined once into value
	line       int      // the file's line the field starts on, from 1
}

var (
	mtTag     = regexp.MustCompile(`^:([0-9]{2}[A-Z]?):(.*)$`)
	mtBalance = regexp.MustCompile(`^([CD])([0-9]{6})([A-Z]{3})([0-9]{1,15},[0-9]{0,15})$`)
	// :61: value date, entry date, mark, funds code, amount, type, and the
	// references (mtReferences).
	mtLine = regexp.MustCompile(`^([0-9]{6})([0-9]{4})?(R?[CD])([A-Z])?([0-9]{1,15},[0-9]{0,15})([NSF][A-Z0-9]{3})(.*)$`)
)

// ParseMT940 reads the statements of an MT940 file, with or without the
// SWIFT envelope ({1:…}{2:…}{4: … -}), in the currencies of exps. Text in
// another encoding than UTF-8 is read with its invalid bytes replaced.
func ParseMT940(b []byte, exps currency.Exponents) ([]Statement, error) {
	fields, err := mtFields(strings.ToValidUTF8(string(b), "�"))
	if err != nil {
		return nil, err
	}
	var out []Statement
	for len(fields) > 0 {
		if fields[0].tag != "20" {
			return nil, atLine(malformed("mt940: a statement starts with :20:, not :%s:", fields[0].tag), fields[0].line)
		}
		end := 1
		for end < len(fields) && fields[end].tag != "20" {
			end++
		}
		s, err := mtStatement(fields[:end], exps)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		fields = fields[end:]
	}
	if len(out) == 0 {
		return nil, malformed("mt940: no statement")
	}
	return out, nil
}

// mtFields splits the text into fields, leaving out the SWIFT envelope's
// blocks and trailers and the "-" that ends a message.
func mtFields(text string) ([]mtField, error) {
	var out []mtField
	n := 0
	for line := range strings.Lines(strings.ReplaceAll(text, "\r\n", "\n")) {
		n++
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.TrimSpace(line) == "", strings.HasPrefix(line, "{"), strings.HasPrefix(line, "-"):
			continue
		}
		if m := mtTag.FindStringSubmatch(line); m != nil {
			out = append(out, mtField{tag: m[1], lines: []string{m[2]}, line: n})
			continue
		}
		if len(out) == 0 {
			return nil, atLine(malformed("mt940: text before the first field"), n)
		}
		// Lines are collected and joined once: appending to the value would
		// copy it for every line, quadratic in an uploaded file.
		out[len(out)-1].lines = append(out[len(out)-1].lines, line)
	}
	for i := range out {
		out[i].value, out[i].lines = strings.Join(out[i].lines, "\n"), nil
	}
	return out, nil
}

// mtStatement reads the fields of one statement, from :20: to the next.
func mtStatement(fields []mtField, exps currency.Exponents) (Statement, error) {
	s := Statement{Format: MT940}
	var closing bool
	for i, f := range fields {
		var err error
		switch f.tag {
		case "20":
			s.ID = strings.TrimSpace(f.value)
		case "25":
			s.Account = mtAccount(f.value)
		case "28C":
			s.Sequence = strings.TrimSpace(f.value)
		case "60F", "60M", "62F", "62M":
			err = mtBalanceField(&s, f, exps, &closing)
		case "61":
			var e Entry
			if e, err = mtEntry(f.value, s.Currency, exps); err == nil {
				if i+1 < len(fields) && fields[i+1].tag == "86" {
					mtInformation(&e, fields[i+1].value)
				}
				s.Entries = append(s.Entries, e)
			}
		}
		if err != nil {
			return Statement{}, atLine(err, f.line)
		}
	}
	if s.Currency == "" || !closing || s.Sequence == "" {
		return Statement{}, atLine(malformed("mt940 statement %q: :28C:, :60F: or :62F: missing, or :61: before :60F:", s.ID), fields[0].line)
	}
	return s, atLine(s.check(), fields[0].line)
}

// mtBalanceField reads the statement's one opening balance (:60a:), which
// sets its currency, or its closing balance (:62a:) after it.
func mtBalanceField(s *Statement, f mtField, exps currency.Exponents, closing *bool) error {
	opening := s.Currency != ""
	var err error
	switch {
	case f.tag[:2] == "60" && opening:
		return malformed("mt940 statement %q: a second opening balance", s.ID)
	case f.tag[:2] == "60":
		s.Opening, s.Currency, err = mtBalanceOf(f.value, "", exps)
	case !opening:
		return malformed("mt940 statement %q: closing balance before the opening", s.ID)
	default:
		s.Closing, _, err = mtBalanceOf(f.value, s.Currency, exps)
		*closing = true
	}
	return err
}

// mtAccount reads :25:, the account: an IBAN, or the bank's account
// number, which some banks give after their BIC or transit number and "/".
func mtAccount(v string) Account {
	v = strings.TrimSpace(v)
	if iban.Valid(v) {
		return Account{IBAN: strings.ReplaceAll(v, " ", "")}
	}
	prefix, number, found := strings.Cut(v, "/")
	if found && iban.Valid(number) {
		return Account{IBAN: number, BIC: prefix}
	}
	return Account{Number: v}
}

// mtBalanceOf reads an opening or closing balance (:60F:, :62F:); a
// closing balance must be in the opening balance's currency ccy.
func mtBalanceOf(v, ccy string, exps currency.Exponents) (Balance, string, error) {
	m := mtBalance.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return Balance{}, "", malformed("mt940: balance %q", v)
	}
	if ccy != "" && m[3] != ccy {
		return Balance{}, "", malformed("mt940: closing balance in %s, opening in %s", m[3], ccy)
	}
	d, err := mtDate(m[2])
	if err != nil {
		return Balance{}, "", err
	}
	amt, err := amount(m[4], m[3], exps, true, m[1] == "D")
	if err != nil {
		return Balance{}, "", err
	}
	return Balance{Amount: amt, Date: d}, m[3], nil
}

// mtEntry reads a statement line (:61:): its first line, and a second of
// supplementary details, which is kept as its text.
func mtEntry(v, ccy string, exps currency.Exponents) (Entry, error) {
	first, details, _ := strings.Cut(v, "\n")
	m := mtLine.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil || ccy == "" {
		return Entry{}, malformed("mt940: statement line %q", first)
	}
	value, err := mtDate(m[1])
	if err != nil {
		return Entry{}, err
	}
	booked := value
	if m[2] != "" {
		if booked, err = mtEntryDate(value, m[2]); err != nil {
			return Entry{}, err
		}
	}
	// RD reverses a debit, so credits the account; RC debits it.
	amt, err := amount(m[5], ccy, exps, true, m[3] == "D" || m[3] == "RC")
	if err != nil {
		return Entry{}, err
	}
	owner, bank, err := mtReferences(m[7])
	if err != nil {
		return Entry{}, err
	}
	return Entry{Amount: amt, Reversal: m[3][0] == 'R', BookingDate: booked, ValueDate: value, TransactionCode: m[6],
		OwnerReference: owner, BankReference: bank, Text: strings.TrimSpace(details)}, nil
}

// mtReferences reads the end of a :61: line: the reference for the account
// owner (16x, which may hold "/" but not "//"), and after "//" the bank's
// (16x). NONREF is no reference.
func mtReferences(rest string) (owner, bank string, err error) {
	owner, bank, _ = strings.Cut(rest, "//")
	owner, bank = strings.TrimSpace(owner), strings.TrimSpace(bank)
	if len(owner) > 16 || len(bank) > 16 {
		return "", "", malformed("mt940: references %q longer than 16 characters", rest)
	}
	if owner == "NONREF" {
		owner = ""
	}
	return owner, bank, nil
}

// mtDate reads a YYMMDD date; SWIFT years are of this century.
func mtDate(s string) (time.Time, error) {
	d, err := time.Parse("060102", s)
	if err != nil {
		return time.Time{}, malformed("mt940: date %q", s)
	}
	return d.AddDate(2000-d.Year()/100*100, 0, 0), nil
}

// mtEntryDate reads the MMDD entry date of a line valued on value: in the
// value date's year, or the year next to it when the two straddle a new
// year (booked 31 December, valued 2 January).
func mtEntryDate(value time.Time, mmdd string) (time.Time, error) {
	d, err := time.Parse("20060102", value.Format("2006")+mmdd)
	if err != nil {
		return time.Time{}, malformed("mt940: entry date %q", mmdd)
	}
	switch months := int(d.Month()) - int(value.Month()); {
	case months > 6:
		d = d.AddDate(-1, 0, 0)
	case months < -6:
		d = d.AddDate(1, 0, 0)
	}
	return d, nil
}

// mtSubfields are the structured :86: subfields read (SWIFT's /CODE/
// convention for information to the account owner).
var mtSubfields = map[string]bool{"EREF": true, "PREF": true, "IREF": true, "NAME": true, "IBAN": true, "BIC": true,
	"REMI": true, "ORDP": true, "BENM": true, "MREF": true, "PURP": true, "ID": true, "ADDR": true, "CSID": true, "TRCD": true, "USTD": true}

// mtInformation reads :86:, the information to the account owner, into
// the entry: its text, and when it is structured (/EREF/…/NAME/…) a detail
// with the references and counterparty it names.
func mtInformation(e *Entry, v string) {
	text := strings.ReplaceAll(v, "\n", "")
	// The text keeps the bank's lines: SWIFT wraps at 65 characters, inside
	// words or between them, so no one join is right for free text.
	e.Text = strings.TrimSpace(e.Text + "\n" + v)
	if !strings.HasPrefix(text, "/") {
		return
	}
	parts := map[string][]string{}
	tag := ""
	for _, part := range strings.Split(text[1:], "/") {
		switch {
		case mtSubfields[part]:
			tag = part
		case tag != "":
			parts[tag] = append(parts[tag], part)
		}
	}
	values := map[string]string{}
	for t, p := range parts {
		values[t] = strings.TrimSpace(strings.Trim(strings.Join(p, "/"), "/"))
	}
	d := Detail{Amount: e.Amount, EndToEndID: values["EREF"], PaymentInfoID: values["PREF"], InstructionID: values["IREF"],
		TransactionCode: e.TransactionCode, Remittance: firstOf(values["REMI"], values["USTD"]),
		Counterparty: Party{Name: values["NAME"], IBAN: values["IBAN"], BIC: values["BIC"]}}
	if d.EndToEndID == "NOTPROVIDED" {
		d.EndToEndID = ""
	}
	e.Details = []Detail{d}
}
