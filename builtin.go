// Package cpt exposes assets embedded in the cpt binary.
//
// The master package list lives at the repository root as cpt.json so that the
// same file can be served over HTTP and embedded at build time.
package cpt

import _ "embed"

//go:embed cpt.json
var builtinList []byte

// BuiltinList returns the master package list shipped with this binary.
// The returned slice must not be modified.
func BuiltinList() []byte { return builtinList }

// BuiltinListOrigin is where the master list is published.
const BuiltinListOrigin = "https://raw.githubusercontent.com/SuSonicTH/cpt/master/cpt.json"
