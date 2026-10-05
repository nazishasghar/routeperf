// Package version is stamped at build time via -ldflags.
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func String() string { return Version + " (" + Commit + ", " + Date + ")" }
