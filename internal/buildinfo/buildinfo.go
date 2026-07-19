package buildinfo

import "fmt"

// These values are set at build time via ldflags.
// Example: go build -ldflags="-X github.com/makewand/makewand/internal/buildinfo.Version=v0.2.0 -X github.com/makewand/makewand/internal/buildinfo.Commit=abc123def"
var (
	Version = "dev"
	Commit  = ""
	Dirty   = ""
)

// VersionString returns a formatted version string.
// For released versions: "v0.2.0"
// For development: "dev-abc123def-dirty" or "dev-abc123def"
func VersionString() string {
	if Version != "dev" {
		return Version
	}
	result := "dev"
	if Commit != "" {
		result += "-" + Commit
	}
	if Dirty != "" {
		result += "-" + Dirty
	}
	return result
}

// FormatVersion returns version suitable for binary --version output.
func FormatVersion() string {
	s := VersionString()
	if Commit != "" {
		s += fmt.Sprintf(" (%s", Commit)
		if Dirty != "" {
			s += fmt.Sprintf(", %s", Dirty)
		}
		s += ")"
	}
	return s
}
