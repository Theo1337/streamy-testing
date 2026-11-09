// Package version holds the build metadata printed by `streamy -version` and
// shown in the tray tooltip.
package version

// These can be overridden at build time without touching the source:
//
//	go build -ldflags "-X streamy/internal/version.Version=0.2.0 \
//	                   -X streamy/internal/version.Commit=$(git rev-parse --short HEAD)"
//
// That is why they are variables and not constants: the linker can only
// rewrite variables.
var (
	Version = "0.99.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a single line, human readable version string.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
