// Package version reports build metadata for the running binary.
//
// Release builds set the variables below with linker flags, for example:
//
//	go build -ldflags "-X switchyard/internal/version.Version=v0.1.0 \
//	  -X switchyard/internal/version.Commit=3f2a9c1 \
//	  -X switchyard/internal/version.Date=2026-11-09T00:00:00Z" ./cmd/switchyard
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at link time. The defaults identify a local development build.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the build metadata of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build metadata. When the linker flags were not set it falls
// back to the VCS information the Go toolchain embeds in the binary.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version()}

	var revision, vcsTime string
	var modified bool
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if info.Commit == "" && revision != "" {
		info.Commit = revision
		if modified {
			info.Commit += "-dirty"
		}
	}
	if info.Date == "" {
		info.Date = vcsTime
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

// String formats the metadata on one line, as printed by `switchyard version`.
func (i Info) String() string {
	return fmt.Sprintf("switchyard %s (commit %s, built %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion)
}
