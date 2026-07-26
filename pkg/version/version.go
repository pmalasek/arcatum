// Package version carries the build identity of the binaries.
//
// Auto-update needs a runner to know what it is running, so the value is stamped in at
// build time:
//
//	go build -ldflags "-X arcatum/pkg/version.Version=2026.07.26" ./cmd/runner
//
// An unstamped build reports "dev", and a runner reporting "dev" is never updated
// automatically — a development binary must not be replaced by a published one behind the
// developer's back.
package version

// Version is the build identity, set via -ldflags at build time.
var Version = "dev"

// IsDev reports whether this is an unstamped development build.
func IsDev() bool { return Version == "dev" || Version == "" }
