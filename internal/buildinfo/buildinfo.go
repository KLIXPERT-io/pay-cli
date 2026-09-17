// Package buildinfo carries the values stamped into the binary at link time
// (§16.4) and degrades gracefully when they are absent.
//
// cmd/pay/main.go declares the -X targets (main.version / main.commit /
// main.date) and hands them to Set before anything else runs. When the binary
// was produced by "go install .../cmd/pay@latest" instead of goreleaser, the
// ldflags are empty and the module version plus VCS stamps recorded by the Go
// toolchain in debug.ReadBuildInfo are used instead, so `pay version` reports
// something more useful than "dev".
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Fallback reported when neither ldflags nor build info say anything.
const unknownVersion = "dev"

// Info is the payload of `pay version --output json` (§16.4). InstallPath and
// Managed are filled in by the caller (internal/update knows whether the
// binary sits in a package-manager-owned location); every other field is
// resolved here.
type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Date        string `json:"date"`
	Go          string `json:"go"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	InstallPath string `json:"install_path"`
	Managed     bool   `json:"managed"`
}

var (
	mu       sync.RWMutex
	resolved Info
	done     bool
)

// Set records the link-time values. It is called exactly once, from
// cmd/pay/main.go, before any other package runs.
func Set(version, commit, date string) {
	mu.Lock()
	defer mu.Unlock()
	resolved = resolve(version, commit, date, debug.ReadBuildInfo)
	done = true
}

// Current returns the resolved build information, computing it from
// debug.ReadBuildInfo alone when Set was never called (which is the case in
// unit tests and in `go run`).
func Current() Info {
	mu.RLock()
	if done {
		defer mu.RUnlock()
		return resolved
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	if !done {
		resolved = resolve("", "", "", debug.ReadBuildInfo)
		done = true
	}
	return resolved
}

// Version is the shorthand used by meta.cli_version in every envelope (§10.1).
func Version() string { return Current().Version }

// Commit is the VCS revision the binary was built from, or "" when unknown.
func Commit() string { return Current().Commit }

// Date is the build timestamp, or "" when unknown.
func Date() string { return Current().Date }

// UserAgent is the value the transport sends (§6); it is derived here so the
// version string has exactly one source.
func UserAgent() string { return "pay-cli/" + Version() }

// resolve is the pure core, parameterised over the build-info reader so the
// fallback ladder is table-testable without building real binaries.
func resolve(version, commit, date string, read func() (*debug.BuildInfo, bool)) Info {
	info := Info{
		Version: strings.TrimSpace(version),
		Commit:  strings.TrimSpace(commit),
		Date:    strings.TrimSpace(date),
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
	info.Version = strings.TrimPrefix(info.Version, "v")

	bi, ok := read()
	if ok && bi != nil {
		vcsRev, vcsTime, vcsModified := vcsStamps(bi)
		if info.Commit == "" {
			info.Commit = vcsRev
		}
		if info.Date == "" {
			info.Date = vcsTime
		}
		if info.Version == "" || info.Version == unknownVersion {
			info.Version = versionFromBuildInfo(bi, vcsRev, vcsModified)
		}
	}
	if info.Version == "" {
		info.Version = unknownVersion
	}
	return info
}

func vcsStamps(bi *debug.BuildInfo) (rev, when string, modified bool) {
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			when = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return rev, when, modified
}

// versionFromBuildInfo prefers the module version recorded by `go install
// module@version`; a source build records the placeholder "(devel)", in which
// case the short VCS revision is used ("dev+g1a2b3c4", "+dirty" when the tree
// had uncommitted changes).
func versionFromBuildInfo(bi *debug.BuildInfo, rev string, modified bool) string {
	if v := strings.TrimSpace(bi.Main.Version); v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	if rev == "" {
		return unknownVersion
	}
	short := rev
	if len(short) > 12 {
		short = short[:12]
	}
	out := unknownVersion + "+g" + short
	if modified {
		out += ".dirty"
	}
	return out
}
