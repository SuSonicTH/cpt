// Package cerr defines the error kinds that map onto cpt's process exit codes.
package cerr

import (
	"errors"
	"fmt"
)

// Kind maps to a process exit code.
type Kind int

const (
	Generic   Kind = 1
	Usage     Kind = 2
	Integrity Kind = 3
	Conflict  Kind = 4
	Network   Kind = 5
)

// E is an error with an associated Kind.
type E struct {
	Kind Kind
	Err  error
}

func (e *E) Error() string { return e.Err.Error() }
func (e *E) Unwrap() error { return e.Err }

// New builds a typed error.
func New(kind Kind, format string, args ...any) error {
	return &E{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// Wrap attaches a kind to an existing error unless it already has one.
func Wrap(kind Kind, err error) error {
	if err == nil {
		return nil
	}
	var e *E
	if errors.As(err, &e) {
		return err
	}
	return &E{Kind: kind, Err: err}
}

// KindOf reports the kind of err, defaulting to Generic.
func KindOf(err error) Kind {
	var e *E
	if errors.As(err, &e) {
		return e.Kind
	}
	return Generic
}

// ExitCode returns the process exit code for err. Errors may carry an explicit
// exit status (interactive SSH sessions do) before the cerr kind is considered.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		if code := ec.ExitCode(); code > 0 && code < 256 {
			return code
		}
	}
	return int(KindOf(err))
}
