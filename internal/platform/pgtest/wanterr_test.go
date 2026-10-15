package pgtest

import (
	"errors"
	"fmt"
	"testing"
)

// errorTB records an Errorf and lets the test go on, as testing's does.
type errorTB struct {
	testing.TB
	failure string
}

func (e *errorTB) Helper() {}
func (e *errorTB) Errorf(format string, args ...any) {
	e.failure = fmt.Sprintf(format, args...)
}

func TestWantErr(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"no error":        {nil, `got <nil>, want an error containing "disk full"`},
		"another message": {errors.New("timeout"), `got timeout, want an error containing "disk full"`},
		"containing":      {errors.New("write: disk full"), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				WantErr(tb, c.err, "disk full")
			}()
			<-done
			if tb.fatal != c.want {
				t.Errorf("WantErr: got %q, want %q", tb.fatal, c.want)
			}
			etb := &errorTB{TB: t}
			if ok := CheckErr(etb, c.err, "disk full"); ok != (c.want == "") {
				t.Errorf("CheckErr: got %v, want %v", ok, c.want == "")
			}
			if etb.failure != c.want {
				t.Errorf("CheckErr: got %q, want %q", etb.failure, c.want)
			}
		})
	}
}
