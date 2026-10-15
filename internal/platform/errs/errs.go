// Package errs adds context to errors as they leave a package (CLAUDE.md:
// errors that cross a package boundary carry their operation and key).
// Wrapped errors stay inside the service: a client only ever sees a problem
// of 04-api-design §4.6.
package errs

import "fmt"

// Wrapf returns nil for a nil error and otherwise err wrapped with %w after
// the formatted context, for example Wrapf(err, "ledger: scan account %s",
// id). It keeps a function whose only error is the one it returns to a
// single return, so a failure no test can provoke (a row that fails to scan
// after its query succeeded) is not a branch of its own.
func Wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}
