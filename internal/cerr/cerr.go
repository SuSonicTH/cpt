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

// ExitStatus is implemented by errors that must report a specific process exit
// status instead of their cerr kind. Interactive SSH sessions use it to return
// the remote shell's status unchanged.
//
// Only types that declare this method are honoured. A plain *exec.ExitError is
// deliberately not: it has an ExitCode method of its own, and honouring that
// would let a wrapped subprocess failure override the documented cerr code of
// the error that wraps it.
type ExitStatus interface {
	error
	CerrExitStatus() int
}

// ExitCode returns the process exit code for err. A cerr kind always wins,
// because it is the documented contract of the command that returned it; only
// errors with an explicit ExitStatus may override a plain Generic error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *E
	if errors.As(err, &e) {
		return int(e.Kind)
	}
	var st ExitStatus
	if errors.As(err, &st) {
		if code := st.CerrExitStatus(); code > 0 && code < 256 {
			return code
		}
	}
	return int(Generic)
}
