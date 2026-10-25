package cerr

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"testing"
)

// explicitStatus is a stand-in for a remote session status.
type explicitStatus struct{ code int }

func (e explicitStatus) Error() string       { return "session failed" }
func (e explicitStatus) CerrExitStatus() int { return e.code }

func TestExitCodePrefersCerrKindOverWrappedSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell exit status is POSIX-only")
	}
	raw := exec.Command("sh", "-c", "exit 1").Run()
	if raw == nil {
		t.Fatal("expected the fake command to fail")
	}
	var ee *exec.ExitError
	if !errors.As(raw, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T", raw)
	}
	// The documented network code must win: a subprocess ExitCode method is not
	// an explicit cpt exit status.
	err := Wrap(Network, fmt.Errorf("copy bundle: %w", raw))
	if got := KindOf(err); got != Network {
		t.Fatalf("KindOf = %v, want Network", got)
	}
	if got := ExitCode(err); got != int(Network) {
		t.Fatalf("ExitCode = %d, want the documented network code %d", got, int(Network))
	}
}

func TestExitCodeHonoursExplicitStatus(t *testing.T) {
	if got := ExitCode(explicitStatus{code: 7}); got != 7 {
		t.Fatalf("ExitCode = %d, want 7", got)
	}
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ExitCode(errors.New("plain")); got != int(Generic) {
		t.Fatalf("ExitCode(plain) = %d, want %d", got, int(Generic))
	}
	// A cerr kind is authoritative even when the wrapped error declares its own
	// status, because the kind is the contract of the command that returned it.
	if got := ExitCode(Wrap(Integrity, explicitStatus{code: 7})); got != int(Integrity) {
		t.Fatalf("wrapped exit status overrode the cerr kind: %d", got)
	}
}
