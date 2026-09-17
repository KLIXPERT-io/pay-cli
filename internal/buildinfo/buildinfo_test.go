package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func bi(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main:     debug.Module{Path: "github.com/KLIXPERT-io/pay-cli", Version: mainVersion},
			Settings: settings,
		}, true
	}
}

func none() (*debug.BuildInfo, bool) { return nil, false }

func TestResolve(t *testing.T) {
	tests := []struct {
		name                  string
		version, commit, date string
		read                  func() (*debug.BuildInfo, bool)
		wantVersion           string
		wantCommit            string
		wantDate              string
	}{
		{
			name:    "ldflags win over everything",
			version: "0.4.2", commit: "deadbeef", date: "2026-09-16T10:00:00Z",
			read:        bi("v0.0.1", debug.BuildSetting{Key: "vcs.revision", Value: "cafebabe"}),
			wantVersion: "0.4.2", wantCommit: "deadbeef", wantDate: "2026-09-16T10:00:00Z",
		},
		{
			name:    "leading v is stripped from the ldflag",
			version: "v1.2.3", read: none,
			wantVersion: "1.2.3",
		},
		{
			name:        "go install module@version",
			read:        bi("v0.3.0"),
			wantVersion: "0.3.0",
		},
		{
			name:        "devel source build uses the vcs revision",
			read:        bi("(devel)", debug.BuildSetting{Key: "vcs.revision", Value: "1a2b3c4d5e6f7a8b9c"}, debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"}),
			wantVersion: "dev+g1a2b3c4d5e6f", wantCommit: "1a2b3c4d5e6f7a8b9c", wantDate: "2026-01-02T03:04:05Z",
		},
		{
			name:        "dirty tree is marked",
			read:        bi("(devel)", debug.BuildSetting{Key: "vcs.revision", Value: "abc123"}, debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			wantVersion: "dev+gabc123.dirty", wantCommit: "abc123",
		},
		{
			name:        "no ldflags and no build info",
			read:        none,
			wantVersion: "dev",
		},
		{
			name:        "build info with no vcs stamps",
			read:        bi("(devel)"),
			wantVersion: "dev",
		},
		{
			name:    "ldflags version dev is upgraded by build info",
			version: "dev", read: bi("v2.0.0"),
			wantVersion: "2.0.0",
		},
		{
			name:    "whitespace is trimmed",
			version: "  0.1.0\n", commit: " abc \t", date: " x ",
			read:        none,
			wantVersion: "0.1.0", wantCommit: "abc", wantDate: "x",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.version, tc.commit, tc.date, tc.read)
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tc.wantCommit)
			}
			if got.Date != tc.wantDate {
				t.Errorf("Date = %q, want %q", got.Date, tc.wantDate)
			}
			if got.Go != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
				t.Errorf("runtime fields not populated: %+v", got)
			}
		})
	}
}

func TestSetAndCurrent(t *testing.T) {
	t.Cleanup(func() {
		mu.Lock()
		resolved, done = Info{}, false
		mu.Unlock()
	})
	Set("9.9.9", "c0ffee", "2026-09-16")
	if got := Version(); got != "9.9.9" {
		t.Errorf("Version() = %q", got)
	}
	if got := Commit(); got != "c0ffee" {
		t.Errorf("Commit() = %q", got)
	}
	if got := Date(); got != "2026-09-16" {
		t.Errorf("Date() = %q", got)
	}
	if got := UserAgent(); got != "pay-cli/9.9.9" {
		t.Errorf("UserAgent() = %q", got)
	}
	if got := Current().InstallPath; got != "" {
		t.Errorf("InstallPath should be caller-supplied, got %q", got)
	}
}

func TestCurrentWithoutSet(t *testing.T) {
	mu.Lock()
	resolved, done = Info{}, false
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		resolved, done = Info{}, false
		mu.Unlock()
	})
	got := Current()
	if got.Version == "" {
		t.Error("Version must never be empty")
	}
	if got.Go == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("runtime fields missing: %+v", got)
	}
}
