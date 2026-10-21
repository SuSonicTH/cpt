package app

import (
	"github.com/SuSonicTH/cpt/internal/install"
	"github.com/SuSonicTH/cpt/internal/version"
)

// GC prunes reconstructible leftovers. Callers hold the state lock, so no
// other cpt process can be staging files.
func (a *App) GC() install.GCResult {
	return install.GC(install.GCOptions{
		BinDir:         a.BinDir(),
		TmpDir:         a.Paths.Tmp,
		ProbeDir:       a.Paths.ProbeCache,
		SelfDir:        a.Paths.SelfCache,
		State:          a.State,
		BundlesDir:     a.Paths.Bundles,
		CurrentVersion: version.Version,
	})
}

// runGC performs best-effort cleanup after a mutating operation.
func (a *App) runGC() {
	res := a.GC()
	if len(res.Warnings) > 0 {
		for _, warning := range res.Warnings {
			a.Log.Warn("garbage collection", "warning", warning)
		}
	}
	if len(res.Removed) > 0 {
		a.Log.Debug("garbage collected", "files", len(res.Removed), "bytes", res.Bytes)
	}
}
