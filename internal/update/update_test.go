package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// TestArchiveAssetNameIsFrozen guards §15's frozen template
// "pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}", which is duplicated in
// .goreleaser.yaml, install.sh and install.ps1. Changing it after v0.1.0
// permanently breaks self-update for every already-installed binary.
func TestArchiveAssetNameIsFrozen(t *testing.T) {
	want := fmt.Sprintf("pay_1.2.3_%s_%s%s", runtime.GOOS, runtime.GOARCH, ArchiveExt(runtime.GOOS))
	if got := archiveAssetName("v1.2.3"); got != want {
		t.Fatalf("archiveAssetName(\"v1.2.3\") = %q, want %q", got, want)
	}

	tests := []struct {
		version, goos, goarch, want string
	}{
		{"v1.2.3", "linux", "amd64", "pay_1.2.3_linux_amd64.tar.gz"},
		{"1.2.3", "linux", "arm64", "pay_1.2.3_linux_arm64.tar.gz"},
		{"v0.1.0", "darwin", "arm64", "pay_0.1.0_darwin_arm64.tar.gz"},
		{"v1.2.3", "windows", "amd64", "pay_1.2.3_windows_amd64.zip"},
		{"v1.2.3", "windows", "arm64", "pay_1.2.3_windows_arm64.zip"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := AssetName(tc.version, tc.goos, tc.goarch); got != tc.want {
				t.Fatalf("AssetName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBinaryName(t *testing.T) {
	if BinaryName("windows") != "pay.exe" || BinaryName("linux") != "pay" {
		t.Fatal("binary name is wrong")
	}
}

func TestNewer(t *testing.T) {
	tests := []struct {
		name               string
		current, candidate string
		want               bool
	}{
		{"patch bump", "v1.2.3", "v1.2.4", true},
		{"same", "v1.2.3", "v1.2.3", false},
		{"older", "v1.2.3", "v1.2.2", false},
		{"missing v prefix", "1.2.3", "1.2.4", true},
		{"prerelease is older than release", "v1.2.3-rc.1", "v1.2.3", true},
		{"dev build is always older", "dev", "v0.1.0", true},
		{"unparseable candidate is never newer", "v1.2.3", "not-a-version", false},
		{"empty candidate", "v1.2.3", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Newer(tc.current, tc.candidate); got != tc.want {
				t.Fatalf("Newer(%q, %q) = %v, want %v", tc.current, tc.candidate, got, tc.want)
			}
		})
	}
}

// buildArchive produces a release archive for the running platform containing
// the binary plus the files goreleaser ships alongside it.
func buildArchive(t *testing.T, payload []byte) []byte {
	t.Helper()
	name := BinaryName(runtime.GOOS)
	if ArchiveExt(runtime.GOOS) == ".zip" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, f := range []struct {
			name string
			data []byte
		}{{"README.md", []byte("readme")}, {name, payload}} {
			w, err := zw.Create(f.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(f.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {name, payload}} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type serverOpts struct {
	tag          string
	archive      []byte
	checksum     string // "" = the real digest of archive
	withSig      bool
	attestStatus int
}

// newReleaseServer serves a release exactly as GitHub does.
func newReleaseServer(t *testing.T, opts serverOpts) *httptest.Server {
	t.Helper()
	asset := AssetName(opts.tag, runtime.GOOS, runtime.GOARCH)
	sum := opts.checksum
	if sum == "" {
		sum = Digest(opts.archive)
	}
	checksums := []byte(sum + "  " + asset + "\n")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/KLIXPERT-io/pay-cli/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, opts.tag)
	})
	mux.HandleFunc("/download/"+opts.tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(opts.archive)
	})
	mux.HandleFunc("/download/"+opts.tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(checksums)
	})
	mux.HandleFunc("/download/"+opts.tag+"/checksums.txt.sig", func(w http.ResponseWriter, r *http.Request) {
		if !opts.withSig {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("signature"))
	})
	mux.HandleFunc("/download/"+opts.tag+"/checksums.txt.pem", func(w http.ResponseWriter, r *http.Request) {
		if !opts.withSig {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("certificate"))
	})
	mux.HandleFunc("/repos/KLIXPERT-io/pay-cli/attestations/", func(w http.ResponseWriter, r *http.Request) {
		status := opts.attestStatus
		if status == 0 {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"attestations":[{"bundle":{}}]}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newUpdater(t *testing.T, srv *httptest.Server, binPath string) *Updater {
	t.Helper()
	dir := filepath.Dir(binPath)
	return &Updater{
		HTTP:           srv.Client(),
		APIBase:        srv.URL,
		DownloadBase:   srv.URL + "/download",
		Repo:           "KLIXPERT-io/pay-cli",
		CurrentVersion: "v0.1.0",
		BinaryPath:     binPath,
		StatePath:      filepath.Join(dir, "state", StateFileName),
		Now:            func() time.Time { return at(12) },
		Signer: Signer{
			LookPath: func(string) (string, error) { return "", fmt.Errorf("no cosign") },
		},
	}
}

func writeFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, BinaryName(runtime.GOOS))
	if err := os.WriteFile(path, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckRecordsPendingVersion(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)

	res, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() = %v", err)
	}
	if !res.UpdateAvailable || res.LatestVersion != "v1.2.3" {
		t.Fatalf("result = %+v", res)
	}
	if res.Asset != AssetName("v1.2.3", runtime.GOOS, runtime.GOARCH) {
		t.Fatalf("asset = %q", res.Asset)
	}

	state, err := LoadState(u.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingVersion != "v1.2.3" || !state.LastCheck.Equal(at(12)) {
		t.Fatalf("state = %+v", state)
	}
	if !state.HasPending("v0.1.0") {
		t.Fatal("state should report a pending update")
	}
}

func TestCheckClearsPendingWhenUpToDate(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v0.1.0", archive: archive})
	u := newUpdater(t, srv, writeFakeBinary(t))
	if err := SaveState(u.StatePath, State{PendingVersion: "v9.9.9"}); err != nil {
		t.Fatal(err)
	}

	res, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.UpdateAvailable {
		t.Fatal("update_available should be false")
	}
	state, _ := LoadState(u.StatePath)
	if state.PendingVersion != "" {
		t.Fatalf("pending_version = %q, want cleared", state.PendingVersion)
	}
}

func TestCheckReportsTheThreeStateOutcome(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	tests := []struct {
		name         string
		attestStatus int
		want         Outcome
	}{
		{"attested", http.StatusOK, OutcomeVerified},
		{"no evidence", http.StatusNotFound, OutcomeUnverifiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive, attestStatus: tc.attestStatus})
			u := newUpdater(t, srv, writeFakeBinary(t))
			u.VerifyOnCheck = true
			res, err := u.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Verification.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%+v)", res.Verification.Outcome, tc.want, res.Verification)
			}
		})
	}
}

func TestApplySwapsTheBinary(t *testing.T) {
	const payload = "NEW BINARY CONTENT"
	archive := buildArchive(t, []byte(payload))
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	var stderr bytes.Buffer
	u.Stderr = &stderr

	res, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if !res.Updated || res.ToVersion != "v1.2.3" {
		t.Fatalf("result = %+v", res)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("binary = %q, want %q", got, payload)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("mode = %v, want executable", info.Mode().Perm())
		}
	}
	// Unverifiable, because the release carried no signature and the
	// attestation API returned 404: proceed, but say so loudly.
	if res.Verification.Outcome != OutcomeUnverifiable {
		t.Fatalf("verification = %+v", res.Verification)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("stderr = %q, want a loud warning", stderr.String())
	}
	state, _ := LoadState(u.StatePath)
	if state.PendingVersion != "" || !state.LastApply.Equal(at(12)) {
		t.Fatalf("state = %+v", state)
	}
	// No temporary files were left behind next to the binary.
	entries, err := os.ReadDir(filepath.Dir(bin))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pay-update-") {
			t.Fatalf("temp file survived: %s", e.Name())
		}
	}
}

// TestUpdateSelfAbortsOnBadChecksum is §15.3's named regression test: a wrong
// checksum means NO swap and a non-zero exit, with PAY_UPDATE_STRICT unset.
func TestUpdateSelfAbortsOnBadChecksum(t *testing.T) {
	archive := buildArchive(t, []byte("MALICIOUS BINARY"))
	srv := newReleaseServer(t, serverOpts{
		tag:      "v1.2.3",
		archive:  archive,
		checksum: strings.Repeat("0", 64),
	})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	u.Strict = false // explicitly NOT strict

	res, err := u.Apply(context.Background())
	if err == nil {
		t.Fatal("Apply() = nil, want update_verification_failed")
	}
	if apierr.CodeOf(err) != apierr.CodeUpdateVerificationFailed {
		t.Fatalf("code = %s, want update_verification_failed", apierr.CodeOf(err))
	}
	if apierr.ExitCode(err) == 0 {
		t.Fatal("exit code must be non-zero")
	}
	if res.Updated {
		t.Fatal("result claims an update happened")
	}
	if got, _ := os.ReadFile(bin); string(got) != "OLD BINARY" {
		t.Fatalf("the binary was swapped despite a bad checksum: %q", got)
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "expected") || !strings.Contains(e.Hint, "computed") {
		t.Errorf("hint must print expected vs computed, got %q", e.Hint)
	}
	state, _ := LoadState(u.StatePath)
	if state.Verification != OutcomeFailed {
		t.Fatalf("state.verification = %q, want verification_failed", state.Verification)
	}
	if state.HasPending("v0.1.0") {
		t.Fatal("a failed verification must never be applied implicitly")
	}
}

func TestApplyStrictRefusesUnverifiable(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	u.Strict = true

	if _, err := u.Apply(context.Background()); err == nil {
		t.Fatal("Apply() = nil, want a refusal under PAY_UPDATE_STRICT")
	}
	if got, _ := os.ReadFile(bin); string(got) != "OLD BINARY" {
		t.Fatal("the binary was swapped under strict verification")
	}
}

func TestApplyIsANoOpWhenUpToDate(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v0.1.0", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)

	res, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if res.Updated || res.Skipped != "already up to date" {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(bin); string(got) != "OLD BINARY" {
		t.Fatal("the binary was replaced")
	}
}

func TestApplyRefusesManagedInstall(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	u.WithEnv(Env{GOBIN: filepath.Dir(bin)})

	res, err := u.Apply(context.Background())
	if err == nil {
		t.Fatal("Apply() = nil, want a refusal on a managed install")
	}
	if !res.Managed.Managed || res.Managed.Manager != "go install" {
		t.Fatalf("managed = %+v", res.Managed)
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "go install") {
		t.Errorf("hint = %q, want the go install command", e.Hint)
	}
	if got, _ := os.ReadFile(bin); string(got) != "OLD BINARY" {
		t.Fatal("a managed binary was replaced")
	}
}

func TestApplySkipsWhenTheLockIsHeld(t *testing.T) {
	archive := buildArchive(t, []byte("NEW BINARY"))
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: archive})
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	u.LockPath = filepath.Join(t.TempDir(), "update.lock")

	held, err := acquireLock(u.LockPath)
	if err != nil {
		t.Fatalf("acquireLock() = %v", err)
	}
	t.Cleanup(func() { _ = held.release() })

	res, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply() = %v, want a skipped no-op", err)
	}
	if res.Updated || res.Skipped != "another update is in progress" {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(bin); string(got) != "OLD BINARY" {
		t.Fatal("two concurrent updates swapped the binary")
	}
}

func TestLockIsReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	first, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release() = %v", err)
	}
	second, err := acquireLock(path)
	if err != nil {
		t.Fatalf("re-acquire = %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyReportsAMissingAsset(t *testing.T) {
	srv := newReleaseServer(t, serverOpts{tag: "v1.2.3", archive: nil})
	// The archive handler is registered, but an unknown platform's asset is not.
	bin := writeFakeBinary(t)
	u := newUpdater(t, srv, bin)
	u.OS, u.Arch = "plan9", "riscv64"

	if _, err := u.Apply(context.Background()); err == nil {
		t.Fatal("Apply() = nil, want route_not_found")
	} else if apierr.CodeOf(err) != apierr.CodeRouteNotFound {
		t.Fatalf("code = %s, want route_not_found", apierr.CodeOf(err))
	}
}

func TestExtract(t *testing.T) {
	payload := []byte("BINARY")
	archive := buildArchive(t, payload)
	got, err := extract(archive, runtime.GOOS)
	if err != nil {
		t.Fatalf("extract() = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("extract() = %q, want %q", got, payload)
	}

	if _, err := extract([]byte("not an archive"), runtime.GOOS); err == nil {
		t.Fatal("extract of junk = nil error")
	}
}

func TestSpawnDetachedRejectsAMissingBinary(t *testing.T) {
	if err := SpawnDetached("", ApplyArgs, nil); err == nil {
		t.Fatal("SpawnDetached(\"\") = nil, want an error")
	}
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	if err := SpawnDetached(missing, ApplyArgs, nil); err == nil {
		t.Fatal("SpawnDetached(missing) = nil, want an error")
	}
}

func TestApplyArgsAreTheImplicitPath(t *testing.T) {
	// The detached child must run the EXPLICIT apply, so it goes through the
	// same verification and the same 120 s deadline as a user-typed one.
	want := []string{"update-self", "--apply", "--quiet"}
	if len(ApplyArgs) != len(want) {
		t.Fatalf("ApplyArgs = %v, want %v", ApplyArgs, want)
	}
	for i := range want {
		if ApplyArgs[i] != want[i] {
			t.Fatalf("ApplyArgs = %v, want %v", ApplyArgs, want)
		}
	}
	if NoRecurseEnv != "PAY_NO_UPDATE=1" {
		t.Fatalf("NoRecurseEnv = %q", NoRecurseEnv)
	}
}

func TestCleanupOldBinaryIsSafe(t *testing.T) {
	// It must tolerate both an absent file and an empty path.
	CleanupOldBinary("")
	CleanupOldBinary(filepath.Join(t.TempDir(), "pay"))
}
