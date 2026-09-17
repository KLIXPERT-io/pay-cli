package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

func TestUpdateSelfRefusesWhenPayNoUpdateIsSet(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// opsRun always sets PAY_NO_UPDATE=1, which is exactly the case under test:
	// no unit test may reach the network.
	res := opsRun(t, home, "update-self", "--check")
	if res.Code != 10 {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if code := res.errorCode(t); code != "operation_unsupported" {
		t.Fatalf("error.code = %q, want operation_unsupported\n%s", code, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "PAY_NO_UPDATE") {
		t.Fatalf("the error does not name PAY_NO_UPDATE\n%s", res.Stdout)
	}
}

func TestUpdateSelfRejectsCheckPlusApply(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// The PAY_NO_UPDATE guard runs first, so clear it for this case.
	res := opsRunEnv(t, home, []string{"PAY_NO_UPDATE="}, "update-self", "--check", "--apply")
	if code := res.errorCode(t); code != "invalid_args" {
		t.Fatalf("error.code = %q, want invalid_args\n%s", code, res.Stdout)
	}
	if res.Code != 5 {
		t.Fatalf("exit = %d, want 5", res.Code)
	}
}

func TestNewUpdaterReadsTheDocumentedEnvironment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := config.NewEnv([]string{
		"PAY_HOME=" + home,
		"HOME=" + home,
		envUpdateURL + "=https://mirror.example/releases",
		envGHToken + "=ghp_fixture",
		envUpdateStrict + "=1",
		"GOBIN=" + filepath.Join(home, "gobin"),
	})
	rt := &Runtime{
		App:   App{Now: func() time.Time { return opsClock }},
		Env:   env,
		Paths: config.PathsFor(env, runtime.GOOS, home),
		Cfg:   &config.Resolved{UpdateChannel: "stable"},
	}

	u, err := newUpdater(rt, updateSelfOptions{})
	if err != nil {
		t.Fatalf("newUpdater: %v", err)
	}
	if u.DownloadBase != "https://mirror.example/releases" {
		t.Fatalf("DownloadBase = %q, want the PAY_UPDATE_URL override", u.DownloadBase)
	}
	if u.Token != "ghp_fixture" {
		t.Fatalf("Token = %q, want GH_TOKEN", u.Token)
	}
	if !u.Strict {
		t.Fatalf("Strict = false, want PAY_UPDATE_STRICT=1 to enable it")
	}
	if u.StatePath != rt.Paths.UpdateStateFile() {
		t.Fatalf("StatePath = %q, want %q", u.StatePath, rt.Paths.UpdateStateFile())
	}
	if u.Timeout != update.ApplyDeadline {
		t.Fatalf("Timeout = %s, want the 120 s apply deadline", u.Timeout)
	}
	if !u.VerifyOnCheck {
		t.Fatalf("VerifyOnCheck = false; --check must report the three-state outcome")
	}
	if u.OS != runtime.GOOS || u.Arch != runtime.GOARCH {
		t.Fatalf("OS/Arch = %s/%s, want %s/%s", u.OS, u.Arch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestNewUpdaterStrictFlagAlone(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := config.NewEnv([]string{"PAY_HOME=" + home, "HOME=" + home})
	rt := &Runtime{
		App:   App{Now: func() time.Time { return opsClock }},
		Env:   env,
		Paths: config.PathsFor(env, runtime.GOOS, home),
		Cfg:   &config.Resolved{},
	}
	u, err := newUpdater(rt, updateSelfOptions{strict: true, timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("newUpdater: %v", err)
	}
	if !u.Strict {
		t.Fatalf("--strict did not set Strict")
	}
	if u.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s, want the --timeout override", u.Timeout)
	}
	if u.Channel != "stable" {
		t.Fatalf("Channel = %q, want the stable default", u.Channel)
	}
}

func TestAddVerificationWarning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    *update.Verification
		want bool
	}{
		{"nil", nil, false},
		{"verified", &update.Verification{Outcome: update.OutcomeVerified}, false},
		{"failed", &update.Verification{Outcome: update.OutcomeFailed}, false},
		{"unverifiable", &update.Verification{Outcome: update.OutcomeUnverifiable, Reason: "cosign not on PATH"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := output.New("update-self", output.KindOpResult, nil)
			addVerificationWarning(env, tc.v)
			got := len(env.Warnings) > 0
			if got != tc.want {
				t.Fatalf("warnings = %v, want a warning: %v", env.Warnings, tc.want)
			}
			if tc.want && env.Warnings[0].Code != "update_unverifiable" {
				t.Fatalf("warning code = %q, want update_unverifiable", env.Warnings[0].Code)
			}
		})
	}
}

func TestUpdateHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	statePath := filepath.Join(dir, update.StateFileName)

	if got := UpdateHint(statePath, "v0.1.0"); got != "" {
		t.Fatalf("UpdateHint on a missing state file = %q, want \"\"", got)
	}

	if err := update.SaveState(statePath, update.State{
		Version:        update.StateVersion,
		LastCheck:      opsClock,
		CurrentVersion: "v0.1.0",
		PendingVersion: "v0.2.0",
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got := UpdateHint(statePath, "v0.1.0")
	if !strings.Contains(got, "v0.2.0") || !strings.Contains(got, "update-self") {
		t.Fatalf("UpdateHint = %q, want it to name the pending version and the command", got)
	}

	// Already on the pending version: nothing to say.
	if got := UpdateHint(statePath, "v0.2.0"); got != "" {
		t.Fatalf("UpdateHint when already current = %q, want \"\"", got)
	}
}

// §15's frozen archive-name template is duplicated in goreleaser, install.sh,
// install.ps1 and internal/update. Changing it after v0.1.0 permanently breaks
// self-update for every already-installed binary, so it is pinned here as well
// as in internal/update: this is the copy the release machinery in this
// assignment (.goreleaser.yaml, install.sh, install.ps1) must agree with.
func TestFrozenAssetNameTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		version, goos, goarch, want string
	}{
		{"v1.2.3", "linux", "amd64", "pay_1.2.3_linux_amd64.tar.gz"},
		{"1.2.3", "darwin", "arm64", "pay_1.2.3_darwin_arm64.tar.gz"},
		{"v0.1.0", "windows", "amd64", "pay_0.1.0_windows_amd64.zip"},
		{"v0.1.0", "windows", "arm64", "pay_0.1.0_windows_arm64.zip"},
	}
	for _, tc := range cases {
		got := update.AssetName(tc.version, tc.goos, tc.goarch)
		if got != tc.want {
			t.Fatalf("update.AssetName(%q, %q, %q) = %q, want %q — "+
				"the archive name is FROZEN and is duplicated in .goreleaser.yaml, install.sh and install.ps1",
				tc.version, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// The release machinery this assignment owns must keep agreeing with the
// binary. These are cheap file assertions, not a substitute for `goreleaser
// check`, which runs in CI.
func TestReleaseMachineryAgreesWithTheFrozenTemplate(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	files := map[string]string{
		".goreleaser.yaml": `pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}`,
		"install.sh":       `${BIN}_${ver_noV}_${os}_${arch}`,
		"install.ps1":      `${Bin}_${verNoV}_${os}_${arch}`,
	}
	for name, want := range files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s does not contain the frozen archive template %q", name, want)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the module root")
	return ""
}
