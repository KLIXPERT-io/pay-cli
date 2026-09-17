package cli

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

func init() { Register(newUpdateSelfCmd) }

// Environment hooks §15 documents and this file is the only consumer of.
const (
	// envUpdateURL points the downloader at a private mirror or an air-gapped
	// artefact store. Distribution is GitHub-only in v0.1.0; this is the
	// documented escape hatch, not a second supported channel.
	envUpdateURL = "PAY_UPDATE_URL"
	// envUpdateStrict refuses an UNVERIFIABLE release. It has no effect on a
	// verification FAILURE, which always aborts (§15.3).
	envUpdateStrict = "PAY_UPDATE_STRICT"
	// envGHToken raises the anonymous 60 req/h GitHub API limit.
	envGHToken = "GH_TOKEN"
	// envNoUpdate disables every network call this file can make.
	envNoUpdate = "PAY_NO_UPDATE"
)

// newUpdateSelfCmd builds `pay update-self` (§15).
//
// The command has exactly two shapes and no third: --check records a pending
// version with a bounded 2 s call, and --apply performs the download, the
// verification and the swap in the foreground under a 120 s total deadline and
// then exits, having done only the update. There is no in-process re-exec: an
// in-process swap would leave the running image executing the OLD command
// surface while reporting the NEW version.
func newUpdateSelfCmd(rt *Runtime) *cobra.Command {
	var (
		check        bool
		apply        bool
		force        bool
		strict       bool
		allowManaged bool
		timeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:     "update-self",
		GroupID: GroupAdmin,
		Short:   "Check for and install a newer pay binary",
		Long: `Check for, and optionally install, a newer pay binary.

With no flags this only CHECKS: it asks the release API for the newest version,
records it and reports. --apply performs the update.

Every download is verified twice: the archive against checksums.txt, and
checksums.txt against its cosign signature or the GitHub attestation. The
result is one of three outcomes, not two:

  verified              proceed
  unverifiable          no evidence either way (cosign absent, attestation API
                        unreachable) - warn loudly and proceed, unless
                        PAY_UPDATE_STRICT=1
  verification_failed   a signature or checksum is present and does NOT match -
                        always abort, exit 1. PAY_UPDATE_STRICT cannot relax it

A binary installed by a package manager (go install, Homebrew, nix, asdf, mise,
scoop, chocolatey, snap, flatpak) is never replaced; the manager's own upgrade
command is reported instead.`,
		Example: `  pay update-self --check
  pay update-self --apply
  PAY_UPDATE_STRICT=1 pay update-self --apply`,
		Args: maxArgs(0, "pay update-self [--check] [--apply] [--force]"),
		RunE: Handle(rt, "update-self", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			return runUpdateSelf(ctx, rt, updateSelfOptions{
				check:        check,
				apply:        apply,
				force:        force,
				strict:       strict,
				allowManaged: allowManaged,
				timeout:      timeout,
			})
		}),
	}

	f := cmd.Flags()
	f.BoolVar(&check, "check", false, "only check; record the pending version and report")
	f.BoolVar(&apply, "apply", false, "download, verify and swap the binary")
	f.BoolVar(&force, "force", false, "apply even when the candidate is not newer")
	f.BoolVar(&strict, "strict", false, "refuse an unverifiable release (same as PAY_UPDATE_STRICT=1)")
	f.BoolVar(&allowManaged, "allow-managed", false, "replace a package-manager-installed binary anyway")
	f.DurationVar(&timeout, "timeout", update.ApplyDeadline, "total deadline for --apply")

	SetHelp(cmd, updateSelfHelp())

	return cmd
}

// updateSelfHelp is §10.5's block for `pay update-self`.
//
// It was the last command in the tree still rendering "(none recorded for this
// command)" under EXAMPLES and COMMON MISTAKES, with the renderer's {0, 1}
// exit-code fallback underneath — which is a lie for a command whose ONLY
// failure modes are remote. Every code below was measured by running the
// command; every example below was run before it was written down.
func updateSelfHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay update-self [--check]",
			"pay update-self --apply [--force] [--strict] [--allow-managed] [--timeout DURATION]",
		},
		Args: []ArgSpec{},
		FlagInfo: map[string]FlagInfo{
			"timeout": {
				Note: "shadows the root --timeout for this command; it bounds the whole --apply, not one request",
			},
		},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"mode":"check"|"apply","current_version":"…","latest_version":"…",` +
				`"update_available":bool,"applied":bool,"asset":"…","path":"…","skipped":"…",` +
				`"verification":{"outcome":"verified"|"unverifiable"|"verification_failed",…},` +
				`"managed":{"managed":bool,"manager":"…","hint":"…"},"checked_at":"…","state_path":"…"}`},
		// Measured against this build. 4 is the ordinary answer on a repo with
		// no published release yet, which is exactly the case an agent hits
		// first; 10 is what PAY_NO_UPDATE produces; 5 is --check with --apply.
		ExitCodes: []int{
			apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
			apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork,
			apierr.ExitCapability,
		},
		Examples: []Example{
			{Why: "is a newer pay published? records the pending version and reports",
				Cmd: "pay update-self --check"},
			{Why: "the same check — with no flags update-self never installs anything",
				Cmd: "pay update-self"},
			{Why: "download, verify twice and swap the binary in the foreground",
				Cmd: "pay update-self --apply"},
			{Why: "reinstall the current version, or move back onto the published one",
				Cmd: "pay update-self --apply --force"},
			{Why: "refuse a release whose signature cannot be checked at all",
				Cmd: "PAY_UPDATE_STRICT=1 pay update-self --apply   # same as --apply --strict"},
			{Why: "raise GitHub's 60 req/h anonymous limit on a shared runner",
				Cmd: "GH_TOKEN=$GH_TOKEN pay update-self --check"},
			{Why: "prove the machine makes no release call at all (exits 10)",
				Cmd: "PAY_NO_UPDATE=1 pay update-self"},
			{Why: "confirm which binary is now running, and where it lives",
				Cmd: "pay version"},
		},
		Mistakes: []Mistake{
			{Wrong: "`pay update-self --check --apply` — expecting check-then-install in one call.",
				Right: "They are mutually exclusive and the pair exits 5 (invalid_args). Run `pay update-self --check` first, then `pay update-self --apply`."},
			{Wrong: "Adding --dry-run to preview the swap.",
				Right: "--dry-run is a NO-OP here: update-self does not model a Payload write, so it still contacts the release API and still replaces the binary. Use `pay update-self --check`, which is the read-only form."},
			{Wrong: "Reading exit 4 as \"pay is broken\".",
				Right: "route_not_found means the release API had no release for the configured channel (a fresh repo, or a channel with nothing published). The installed binary is untouched; check `pay config get update_channel` and the project's releases page."},
			{Wrong: "Setting PAY_UPDATE_STRICT=1 and expecting it to relax a bad signature.",
				Right: "It does the opposite, and only to the middle outcome: `unverifiable` (no evidence either way) becomes a refusal. `verification_failed` (a checksum or signature that is present and does NOT match) always aborts with exit 1, and no flag or variable relaxes it."},
			{Wrong: "Running --apply on a Homebrew / go install / nix / scoop binary.",
				Right: "A package-managed binary is never replaced; the envelope reports .managed with the manager's own upgrade command in .managed.hint. Run that, or pass --allow-managed to overwrite it and accept that the manager will fight you."},
			{Wrong: "Expecting the running process to become the new version.",
				Right: "There is no in-process re-exec — the swap lands on disk and this process exits having done only the update. Run `pay version` (or just the next command) to see the new one."},
		},
		Notes: []string{
			"--dry-run and --no-cache have no effect on this command: it talks to the release API, not to Payload.",
			"PAY_UPDATE_URL points the downloader at a mirror; PAY_NO_UPDATE disables every network call this command can make.",
		},
		SeeAlso: []string{
			"pay version                      # what is installed right now, and whether a manager owns it",
			"pay config get update_channel    # which channel --check follows",
			"pay explain --section exit_codes # the full table behind the codes above",
		},
	}
}

type updateSelfOptions struct {
	check        bool
	apply        bool
	force        bool
	strict       bool
	allowManaged bool
	timeout      time.Duration
}

type updateSelfData struct {
	Mode           string               `json:"mode"`
	CurrentVersion string               `json:"current_version"`
	LatestVersion  string               `json:"latest_version,omitempty"`
	Available      bool                 `json:"update_available"`
	Applied        bool                 `json:"applied"`
	Asset          string               `json:"asset,omitempty"`
	Path           string               `json:"path,omitempty"`
	Skipped        string               `json:"skipped,omitempty"`
	Verification   *update.Verification `json:"verification,omitempty"`
	Managed        update.Managed       `json:"managed"`
	CheckedAt      *time.Time           `json:"checked_at,omitempty"`
	StatePath      string               `json:"state_path"`
}

func runUpdateSelf(ctx context.Context, rt *Runtime, opt updateSelfOptions) (*output.Envelope, error) {
	// Argument validation comes first: a contradictory command line is the
	// caller's mistake and is worth naming even on a machine where updating is
	// switched off entirely.
	if opt.check && opt.apply {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--check and --apply are mutually exclusive.").
			WithHint("run `pay update-self --check` first, then `pay update-self --apply`.")
	}

	// §4.5's rule: a variable that is set but empty counts as unset. Env.Has is
	// presence-only, so the value is what is tested here.
	if rt.Env.Get(envNoUpdate) != "" {
		return nil, apierr.New(apierr.CodeOperationUnsupported,
			"%s is set, so PayCLI will not contact the release API.", envNoUpdate).
			WithHint("unset %s to check for updates.", envNoUpdate)
	}

	u, err := newUpdater(rt, opt)
	if err != nil {
		return nil, err
	}

	data := updateSelfData{
		Mode:           "check",
		CurrentVersion: u.CurrentVersion,
		StatePath:      u.StatePath,
		Managed:        update.DetectManaged(u.BinaryPath, updaterEnv(rt)),
	}

	if opt.apply {
		data.Mode = "apply"
		res, err := u.Apply(ctx)
		if res != nil {
			data.Applied = res.Updated
			data.LatestVersion = res.ToVersion
			data.Available = res.ToVersion != "" && res.ToVersion != res.FromVersion
			data.Asset = res.Asset
			data.Path = res.Path
			data.Skipped = res.Skipped
			data.Managed = res.Managed
			v := res.Verification
			data.Verification = &v
		}
		if err != nil {
			return nil, err
		}
		env := output.New("update-self", output.KindOpResult, jsonValue(data))
		addVerificationWarning(env, data.Verification)
		if data.Applied {
			env.WithNext(&output.Next{
				Reason: output.ReasonVerifyWrite,
				Cmd:    "pay version",
			})
		}
		return env, nil
	}

	res, err := u.Check(ctx)
	if err != nil {
		return nil, err
	}
	data.LatestVersion = res.LatestVersion
	data.Available = res.UpdateAvailable
	data.Asset = res.Asset
	data.Managed = res.Managed
	checkedAt := res.CheckedAt
	data.CheckedAt = &checkedAt
	if res.Verification.Outcome != "" {
		v := res.Verification
		data.Verification = &v
	}

	env := output.New("update-self", output.KindOpResult, jsonValue(data))
	addVerificationWarning(env, data.Verification)
	switch {
	case res.Managed.Managed && res.UpdateAvailable:
		env.AddWarning(output.Warning{
			Code:    "update_managed",
			Message: fmt.Sprintf("pay %s is available, but this binary is managed by %s.", res.LatestVersion, res.Managed.Manager),
			Hint:    res.Managed.Hint,
		})
	case res.UpdateAvailable:
		env.WithNext(&output.Next{
			Reason: "update_available",
			Cmd:    "pay update-self --apply",
			Args:   map[string]any{"from": res.CurrentVersion, "to": res.LatestVersion},
		})
	}
	return env, nil
}

// addVerificationWarning surfaces §15.3's middle outcome in the envelope as
// well as on stderr, because an agent never reads stderr.
func addVerificationWarning(env *output.Envelope, v *update.Verification) {
	if v == nil || v.Outcome != update.OutcomeUnverifiable {
		return
	}
	env.AddWarning(output.Warning{
		Code:    "update_unverifiable",
		Message: v.WarnLine(),
		Hint:    "set PAY_UPDATE_STRICT=1 to refuse unverifiable releases, or install cosign so the signature can be checked.",
	})
}

// newUpdater assembles the §15 updater from the resolved configuration and the
// captured environment. Every field that a test needs to redirect — the API
// base, the download base, the clock, the binary path — is a field rather than
// a constant, which is what makes the "wrong checksum performs no swap" test
// possible without a network.
func newUpdater(rt *Runtime, opt updateSelfOptions) (*update.Updater, error) {
	bin, err := update.ResolveBinary()
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInternal,
			"cannot resolve the running binary: %v", err)
	}
	timeout := opt.timeout
	if timeout <= 0 {
		timeout = update.ApplyDeadline
	}
	channel := "stable"
	if rt.Cfg != nil && rt.Cfg.UpdateChannel != "" {
		channel = rt.Cfg.UpdateChannel
	}

	u := &update.Updater{
		DownloadBase:   rt.Env.Get(envUpdateURL),
		Channel:        channel,
		CurrentVersion: buildinfo.Version(),
		BinaryPath:     bin,
		StatePath:      rt.Paths.UpdateStateFile(),
		Token:          rt.Env.Get(envGHToken),
		Strict:         opt.strict || rt.Env.Get(envUpdateStrict) == "1",
		Force:          opt.force,
		AllowManaged:   opt.allowManaged,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		Now:            rt.App.Now,
		Stderr:         rt.Stderr(),
		Timeout:        timeout,
		// --check verifies the signature over checksums.txt only when it is
		// about to report an outcome, so the default check stays one request.
		VerifyOnCheck: !opt.apply,
	}
	return u.WithEnv(updaterEnv(rt)), nil
}

func updaterEnv(rt *Runtime) update.Env {
	return update.Env{
		GOPATH: rt.Env.Get("GOPATH"),
		GOBIN:  rt.Env.Get("GOBIN"),
		Home:   homeDirOf(rt),
		GOOS:   runtime.GOOS,
	}
}

// UpdateHint is the one-line notice `pay version` and `pay doctor` print when a
// newer release was recorded by an earlier --check. It never performs I/O
// beyond reading the state file, so it is safe on every command path.
func UpdateHint(statePath, currentVersion string) string {
	state, err := update.LoadState(statePath)
	if err != nil || !state.HasPending(currentVersion) {
		return ""
	}
	return fmt.Sprintf("pay %s is available (you have %s): run `pay update-self --apply`.",
		strings.TrimSpace(state.PendingVersion), strings.TrimSpace(currentVersion))
}
