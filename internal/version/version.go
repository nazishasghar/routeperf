// Package version is stamped at build time via -ldflags; `go install` builds
// fall back to the module version and VCS info embedded by the Go toolchain.
package version

import "runtime/debug"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func String() string {
	if Version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if v := bi.Main.Version; v != "" && v != "(devel)" {
				Version = v
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) >= 7 {
						Commit = s.Value[:7]
					}
				case "vcs.time":
					Date = s.Value
				}
			}
		}
	}
	return Version + " (" + Commit + ", " + Date + ")"
}
