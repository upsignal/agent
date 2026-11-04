// Package version holds build metadata injected at link time.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set with -ldflags "-X github.com/upsignal/agent/internal/version.Version=..." at release time.
var (
	Version = "0.0.0-dev"
	Commit  = ""
)

// CommitSHA returns the injected commit, falling back to the VCS stamp Go embeds in local builds.
func CommitSHA() string {
	if Commit != "" {
		return Commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// String is the one-line description printed by `upsignal-agent version`.
func String() string {
	return fmt.Sprintf("upsignal-agent %s (commit %s, %s, %s/%s)",
		Version, CommitSHA(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
