package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
)

const (
	// DefaultRepo is the release repository.
	DefaultRepo = "KLIXPERT-io/pay-cli"
	// DefaultAPIBase is the GitHub API root; PAY_UPDATE_URL overrides the
	// download root for private mirrors and air-gapped installs.
	DefaultAPIBase = "https://api.github.com"
	// CheckTimeout bounds `--check`'s HTTP call (§15.1).
	CheckTimeout = 2 * time.Second
	// ApplyDeadline is the total deadline for an explicit `--apply`.
	ApplyDeadline = 120 * time.Second
	// maxArchive caps a downloaded artefact.
	maxArchive = 200 << 20
	// maxMetadata caps checksums.txt, signatures and API responses.
	maxMetadata = 8 << 20
)

// ErrLocked is returned when another `pay` process is already updating.
var ErrLocked = errors.New("update: another update is already in progress")

// Updater performs §15's check, download, verify and swap.
type Updater struct {
	// HTTP is the client for the release API and downloads.
	HTTP *http.Client
	// APIBase is the GitHub API root (a field, not a constant, so tests point
	// it at an httptest.Server).
	APIBase string
	// DownloadBase is the release asset root. Empty derives GitHub's.
	// PAY_UPDATE_URL sets it.
	DownloadBase string
	// Repo is "owner/name".
	Repo string
	// Channel is "stable" (the /releases/latest endpoint) or anything else,
	// which lists /releases and takes the newest, prereleases included.
	Channel string
	// CurrentVersion is the running binary's version.
	CurrentVersion string
	// BinaryPath is the symlink-resolved path that will be replaced.
	BinaryPath string
	// StatePath is update-state.json's location.
	StatePath string
	// LockPath is the apply lock's location. Empty derives it from StatePath.
	LockPath string
	// Token is GH_TOKEN.
	Token string
	// Strict is PAY_UPDATE_STRICT=1: refuse an unverifiable release.
	Strict bool
	// Force applies even when the version is not newer.
	Force bool
	// AllowManaged applies even on a package-managed install.
	AllowManaged bool
	// OS and Arch name the artefact; empty uses runtime's.
	OS, Arch string
	// Now is the injected clock.
	Now func() time.Time
	// Stderr receives the loud unverifiable warning. Nil discards it.
	Stderr io.Writer
	// Signer verifies checksums.txt.
	Signer Signer
	// Timeout is the apply deadline; 0 uses ApplyDeadline.
	Timeout time.Duration
	// CheckTimeout bounds the release lookup; 0 uses CheckTimeout.
	CheckTimeout time.Duration
	// VerifyOnCheck makes --check report the three-state outcome by verifying
	// the signature over checksums.txt (no artefact download).
	VerifyOnCheck bool

	env Env
}

// WithEnv supplies the environment slice managed-install detection needs.
func (u *Updater) WithEnv(env Env) *Updater {
	u.env = env
	return u
}

func (u *Updater) normalise() {
	if u.HTTP == nil {
		u.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if u.APIBase == "" {
		u.APIBase = DefaultAPIBase
	}
	if u.Repo == "" {
		u.Repo = DefaultRepo
	}
	if u.DownloadBase == "" {
		u.DownloadBase = "https://github.com/" + u.Repo + "/releases/download"
	}
	if u.OS == "" {
		u.OS = runtime.GOOS
	}
	if u.Arch == "" {
		u.Arch = runtime.GOARCH
	}
	if u.Now == nil {
		u.Now = func() time.Time { return time.Time{} }
	}
	if u.Timeout <= 0 {
		u.Timeout = ApplyDeadline
	}
	if u.CheckTimeout <= 0 {
		u.CheckTimeout = CheckTimeout
	}
	if u.LockPath == "" && u.StatePath != "" {
		u.LockPath = u.StatePath + ".lock"
	}
	if u.Signer.HTTP == nil {
		u.Signer.HTTP = u.HTTP
	}
	if u.Signer.APIBase == "" {
		u.Signer.APIBase = u.APIBase
	}
	if u.Signer.Repo == "" {
		u.Signer.Repo = u.Repo
	}
	if u.Signer.Token == "" {
		u.Signer.Token = u.Token
	}
}

// archiveAssetName is FROZEN (§15). It duplicates goreleaser's
// name_template "pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}"; changing it after
// v0.1.0 permanently breaks self-update for every already-installed binary,
// because old binaries construct the old name.
func archiveAssetName(version string) string {
	return AssetName(version, runtime.GOOS, runtime.GOARCH)
}

// assetName is the artefact for this updater's target platform. The running
// platform always goes through the frozen archiveAssetName; only a test (or a
// cross-platform query) supplies a different OS/Arch.
func (u *Updater) assetName(tag string) string {
	if u.OS == runtime.GOOS && u.Arch == runtime.GOARCH {
		return archiveAssetName(tag)
	}
	return AssetName(tag, u.OS, u.Arch)
}

// AssetName builds the release archive's file name for a target platform.
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("pay_%s_%s_%s%s", strings.TrimPrefix(version, "v"), goos, goarch, ArchiveExt(goos))
}

// ArchiveExt is .zip on Windows and .tar.gz everywhere else, matching
// goreleaser's format_overrides.
func ArchiveExt(goos string) string {
	if goos == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// BinaryName is the executable's name inside the archive.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "pay.exe"
	}
	return "pay"
}

// Newer reports whether candidate is a strictly newer release than current.
// A non-semver current version (a dev build, "unknown") counts as older, so a
// locally built binary can still be updated deliberately.
func Newer(current, candidate string) bool {
	c := canonicalVersion(candidate)
	if c == "" {
		return false
	}
	cur := canonicalVersion(current)
	if cur == "" {
		return true
	}
	return semver.Compare(c, cur) > 0
}

func canonicalVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return semver.Canonical(v)
}

// ResolveBinary returns the running binary's real path, following symlinks so
// the swap replaces the file rather than the link.
func ResolveBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// CheckResult is what `pay update-self --check` reports.
type CheckResult struct {
	CurrentVersion  string       `json:"current_version"`
	LatestVersion   string       `json:"latest_version,omitempty"`
	UpdateAvailable bool         `json:"update_available"`
	Asset           string       `json:"asset,omitempty"`
	Verification    Verification `json:"verification,omitempty"`
	Managed         Managed      `json:"managed"`
	CheckedAt       time.Time    `json:"checked_at"`
}

// Check asks the release API for the newest version, records it in the state
// file and returns the comparison. It is bounded by CheckTimeout so it can be
// called from a fast command without an unbounded stall.
func (u *Updater) Check(ctx context.Context) (*CheckResult, error) {
	u.normalise()
	ctx, cancel := context.WithTimeout(ctx, u.CheckTimeout)
	defer cancel()

	tag, err := u.latestTag(ctx)
	if err != nil {
		u.recordError(err)
		return nil, err
	}

	res := &CheckResult{
		CurrentVersion:  u.CurrentVersion,
		LatestVersion:   tag,
		UpdateAvailable: Newer(u.CurrentVersion, tag),
		Asset:           u.assetName(tag),
		Managed:         DetectManaged(u.BinaryPath, u.env),
		CheckedAt:       u.Now(),
	}

	if res.UpdateAvailable && u.VerifyOnCheck {
		res.Verification = u.verifySignatureOnly(ctx, tag)
	}

	if u.StatePath != "" {
		state, _ := LoadState(u.StatePath)
		state.LastCheck = u.Now()
		state.CurrentVersion = u.CurrentVersion
		state.LastError = ""
		if res.UpdateAvailable {
			if state.PendingVersion != tag {
				state.Notified = false
			}
			state.PendingVersion = tag
			state.Verification = res.Verification.Outcome
		} else {
			state.PendingVersion = ""
			state.Verification = ""
			state.Notified = false
		}
		if err := SaveState(u.StatePath, state); err != nil {
			return res, apierr.Wrap(err, apierr.CodeInternal, "cannot write %s: %v", u.StatePath, err)
		}
	}
	return res, nil
}

// verifySignatureOnly checks the signature over checksums.txt without
// downloading the archive, so `--check` can print the three-state outcome.
func (u *Updater) verifySignatureOnly(ctx context.Context, tag string) Verification {
	checksums, err := u.download(ctx, u.assetURL(tag, "checksums.txt"), maxMetadata)
	if err != nil {
		return Verification{Outcome: OutcomeUnverifiable, Method: "checksum", Reason: "checksums.txt could not be fetched"}
	}
	sig, _ := u.download(ctx, u.assetURL(tag, "checksums.txt.sig"), maxMetadata)
	pem, _ := u.download(ctx, u.assetURL(tag, "checksums.txt.pem"), maxMetadata)
	return u.Signer.VerifySignature(ctx, checksums, sig, pem)
}

// ApplyResult is what `pay update-self --apply` reports.
type ApplyResult struct {
	Updated      bool         `json:"updated"`
	FromVersion  string       `json:"from_version"`
	ToVersion    string       `json:"to_version,omitempty"`
	Asset        string       `json:"asset,omitempty"`
	Path         string       `json:"path,omitempty"`
	Verification Verification `json:"verification"`
	Managed      Managed      `json:"managed"`
	// Skipped explains a no-op ("already up to date", "another update is in
	// progress", "managed install").
	Skipped string `json:"skipped,omitempty"`
}

// Apply downloads, verifies and swaps the binary under a total deadline.
//
// On success the caller exits 0 having done only the update: it never re-execs
// the new binary, so the running image's command surface always matches the
// version it reports (§15.1).
func (u *Updater) Apply(ctx context.Context) (*ApplyResult, error) {
	u.normalise()
	ctx, cancel := context.WithTimeout(ctx, u.Timeout)
	defer cancel()

	managed := DetectManaged(u.BinaryPath, u.env)
	res := &ApplyResult{FromVersion: u.CurrentVersion, Managed: managed, Path: u.BinaryPath}
	if managed.Managed && !u.AllowManaged {
		res.Skipped = "managed install"
		return res, apierr.New(apierr.CodeOperationUnsupported,
			"this binary was installed by %s, so PayCLI will not replace it.", managed.Manager).
			WithHint("%s", managed.Hint)
	}

	if u.LockPath != "" {
		lock, err := acquireLock(u.LockPath)
		if err != nil {
			if errors.Is(err, ErrLocked) {
				res.Skipped = "another update is in progress"
				return res, nil
			}
			return res, apierr.Wrap(err, apierr.CodeInternal, "cannot take the update lock %s: %v", u.LockPath, err)
		}
		// release only reports a failure to unlink a lock file we are done with;
		// there is nothing the caller could do about it.
		defer func() { _ = lock.release() }()
	}

	tag, err := u.targetVersion(ctx)
	if err != nil {
		u.recordError(err)
		return res, err
	}
	res.ToVersion = tag
	if !u.Force && !Newer(u.CurrentVersion, tag) {
		res.Skipped = "already up to date"
		return res, nil
	}

	asset := u.assetName(tag)
	res.Asset = asset

	archive, err := u.download(ctx, u.assetURL(tag, asset), maxArchive)
	if err != nil {
		u.recordError(err)
		return res, err
	}
	checksums, err := u.download(ctx, u.assetURL(tag, "checksums.txt"), maxMetadata)
	if err != nil {
		u.recordError(err)
		return res, err
	}
	sig, _ := u.download(ctx, u.assetURL(tag, "checksums.txt.sig"), maxMetadata)
	pem, _ := u.download(ctx, u.assetURL(tag, "checksums.txt.pem"), maxMetadata)

	verification := Combine(
		VerifyChecksum(asset, archive, checksums),
		u.Signer.VerifySignature(ctx, checksums, sig, pem),
	)
	res.Verification = verification
	if err := verification.Enforce(u.Strict); err != nil {
		// §15.3 "delete the downloaded artefact": nothing has been written to
		// disk at this point — the archive exists only in this function's
		// memory and is discarded with the frame. The swap below is the first
		// thing that touches the filesystem, and it is never reached.
		u.recordVerification(tag, verification)
		return res, err
	}
	if line := verification.WarnLine(); line != "" && u.Stderr != nil {
		fmt.Fprintln(u.Stderr, line)
	}

	binary, err := extract(archive, u.OS)
	if err != nil {
		u.recordError(err)
		return res, err
	}
	if u.BinaryPath == "" {
		return res, apierr.New(apierr.CodeInternal, "the running binary's path could not be resolved, so it cannot be replaced.").
			WithHint("reinstall with install.sh, or pass an explicit path")
	}
	if err := u.swap(binary); err != nil {
		u.recordError(err)
		return res, err
	}

	res.Updated = true
	if u.StatePath != "" {
		state, _ := LoadState(u.StatePath)
		state.PendingVersion = ""
		state.Verification = verification.Outcome
		state.Notified = false
		state.CurrentVersion = tag
		state.LastApply = u.Now()
		state.LastError = ""
		if err := SaveState(u.StatePath, state); err != nil {
			return res, apierr.Wrap(err, apierr.CodeInternal, "cannot write %s: %v", u.StatePath, err)
		}
	}
	return res, nil
}

// swap writes the new binary next to the old one and renames it into place.
func (u *Updater) swap(binary []byte) error {
	dir := filepath.Dir(u.BinaryPath)
	tmp, err := os.CreateTemp(dir, ".pay-update-*")
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInternal,
			"cannot write next to %s: %v", u.BinaryPath, err).
			WithHint("self-update needs write access to the install directory; reinstall into ~/.local/bin, or update with your package manager")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return apierr.Wrap(err, apierr.CodeInternal, "cannot write %s: %v", tmpName, err)
	}
	if err := tmp.Sync(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return apierr.Wrap(err, apierr.CodeInternal, "cannot flush %s: %v", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot close %s: %v", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot chmod %s: %v", tmpName, err)
	}
	if err := swapBinary(tmpName, u.BinaryPath); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot replace %s: %v", u.BinaryPath, err)
	}
	return nil
}

// targetVersion prefers the version already recorded by --check, so an implicit
// apply does not need a second network round trip before it starts.
func (u *Updater) targetVersion(ctx context.Context) (string, error) {
	if u.StatePath != "" {
		if state, err := LoadState(u.StatePath); err == nil && state.PendingVersion != "" {
			if u.Force || Newer(u.CurrentVersion, state.PendingVersion) {
				return state.PendingVersion, nil
			}
		}
	}
	return u.latestTag(ctx)
}

// release is the slice of GitHub's release object PayCLI reads.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// latestTag resolves the newest release tag for the configured channel.
func (u *Updater) latestTag(ctx context.Context) (string, error) {
	base := strings.TrimSuffix(u.APIBase, "/")
	if u.Channel == "" || u.Channel == "stable" {
		body, err := u.download(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", base, u.Repo), maxMetadata)
		if err != nil {
			return "", err
		}
		var r release
		if err := json.Unmarshal(body, &r); err != nil || r.TagName == "" {
			return "", apierr.New(apierr.CodeNonJSONResponse,
				"the release API did not return a usable release for %s.", u.Repo).
				WithHint("check PAY_UPDATE_URL / network access, or install manually from the releases page")
		}
		return r.TagName, nil
	}

	body, err := u.download(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=20", base, u.Repo), maxMetadata)
	if err != nil {
		return "", err
	}
	var list []release
	if err := json.Unmarshal(body, &list); err != nil {
		return "", apierr.New(apierr.CodeNonJSONResponse, "the release list for %s could not be parsed.", u.Repo)
	}
	best := ""
	for _, r := range list {
		if r.Draft || r.TagName == "" {
			continue
		}
		if best == "" || Newer(best, r.TagName) {
			best = r.TagName
		}
	}
	if best == "" {
		return "", apierr.New(apierr.CodeRouteNotFound, "no release was found for %s on channel %q.", u.Repo, u.Channel)
	}
	return best, nil
}

// assetURL builds a release asset URL.
func (u *Updater) assetURL(tag, name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(u.DownloadBase, "/"), tag, name)
}

// download fetches a URL with the release token attached, classifying network
// failures the same way the rest of PayCLI does.
//
// §3.1 note: these requests target the GitHub release API, not the Payload API,
// so they deliberately do not go through internal/payload's transport (which
// injects Payload credentials and Accept-Language).
func (u *Updater) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInvalidArgs, "bad update URL: %v", err)
	}
	req.Header.Set("Accept", "application/octet-stream, application/vnd.github+json, */*")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}

	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, apierr.Network(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		code := apierr.CodeServerUnavailable
		switch resp.StatusCode {
		case http.StatusNotFound:
			code = apierr.CodeRouteNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			code = apierr.CodeAuthInvalid
		case http.StatusTooManyRequests:
			code = apierr.CodeRateLimited
		}
		return nil, apierr.New(code, "GET %s returned HTTP %d.", path.Base(url), resp.StatusCode).
			WithHint("set GH_TOKEN to raise GitHub's anonymous rate limit, or install manually from the releases page")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, apierr.Network(err)
	}
	return body, nil
}

// recordError stores a failure in the state file for `pay doctor`. It is
// best-effort: a failing state write must not mask the real error.
func (u *Updater) recordError(err error) {
	if u.StatePath == "" || err == nil {
		return
	}
	state, loadErr := LoadState(u.StatePath)
	if loadErr != nil {
		return
	}
	state.LastError = apierr.From(err).Message
	state.LastCheck = u.Now()
	_ = SaveState(u.StatePath, state)
}

// recordVerification remembers a failed verification so the implicit path never
// retries it in the background.
func (u *Updater) recordVerification(tag string, v Verification) {
	if u.StatePath == "" {
		return
	}
	state, err := LoadState(u.StatePath)
	if err != nil {
		return
	}
	state.PendingVersion = tag
	state.Verification = v.Outcome
	state.LastError = v.Reason
	_ = SaveState(u.StatePath, state)
}

// extract pulls the `pay` binary out of the release archive.
func extract(archive []byte, goos string) ([]byte, error) {
	name := BinaryName(goos)
	var (
		out []byte
		err error
	)
	if ArchiveExt(goos) == ".zip" {
		out, err = extractZip(archive, name)
	} else {
		out, err = extractTarGz(archive, name)
	}
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInternal, "the release archive could not be read: %v", err).
			WithHint("install manually from the releases page and report this")
	}
	return out, nil
}

// extractTarGz returns the named entry from a .tar.gz.
func extractTarGz(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is not in the archive", name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != name {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxArchive))
	}
}

// extractZip returns the named entry from a .zip.
func extractZip(data []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if path.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxArchive))
	}
	return nil, fmt.Errorf("%s is not in the archive", name)
}
