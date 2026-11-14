// Package version holds the build version, set at link time.
package version

// Version is replaced by
// -ldflags "-X github.com/cairnobs/cairnobs/internal/version.Version=...".
var Version = "dev"
