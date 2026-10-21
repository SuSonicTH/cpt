package app

import (
	"runtime"

	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/state"
)

// ReconcilePending activates binaries staged by a Windows install because the
// old process was still running. It only takes the state lock when work is
// actually due, so normal starts stay cheap.
func (a *App) ReconcilePending() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if !install.HasPending(a.BinDir(), a.State) {
		return nil
	}
	lock, err := state.Acquire(a.Paths.LockFile, reconcileLockTimeout)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := a.loadState(); err != nil {
		return err
	}
	swapped, warnings := install.SwapPending(a.BinDir(), a.State)
	for _, warning := range warnings {
		a.Log.Warn("pending binary update", "warning", warning)
		a.Out.Warnf("%s", warning)
	}
	if len(swapped) == 0 {
		return nil
	}
	for _, bin := range swapped {
		a.Log.Info("activated staged binary", "bin", bin)
	}
	return a.State.Save(a.Paths.StateFile)
}
