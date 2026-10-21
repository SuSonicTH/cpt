package version

// These are overridden at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the human readable version.
func String() string { return Version }

// Full returns version, commit and date in a single line.
func Full() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
