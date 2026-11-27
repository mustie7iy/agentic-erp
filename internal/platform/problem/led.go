package problem

import "errors"

// Led is the refusal in err as an action of another context answers it,
// when that action made the refused call itself (collections' dunning
// run posting a customer debit memo through receivables, T76h): a copy
// whose detail leads with lead, which names what the caller acted on,
// and without field errors, which name the refused call's fields that the
// caller's own request does not have. The code and status stay the
// refusing context's, which a consumer branches on. The problem is
// copied, never changed: its maker may keep it (the package doc). An
// error that holds no problem is returned as it is.
func Led(err error, lead string) error {
	var p *Problem
	if !errors.As(err, &p) {
		return err
	}
	led := *p
	led.Detail, led.Errors = lead+p.Detail, nil
	return &led
}

// Renamed is the refusal in err as the caller's request names its fields,
// when the caller passed another context what its request gave under
// other names or indexes (shipping's goods issue items, which inventory
// numbers as its own, S2f2): a copy whose field errors' fields are
// rename's of theirs, without those rename does not keep (ok false: a
// field the caller's request does not have, as Led leaves them out). The
// code, status and detail stay the refusing context's. The problem is
// copied, never changed (Led). An error that holds no problem is returned
// as it is.
func Renamed(err error, rename func(field string) (string, bool)) error {
	var p *Problem
	if !errors.As(err, &p) {
		return err
	}
	renamed := *p
	renamed.Errors = nil
	for _, fe := range p.Errors {
		if f, ok := rename(fe.Field); ok {
			fe.Field = f
			renamed.Errors = append(renamed.Errors, fe)
		}
	}
	return &renamed
}
