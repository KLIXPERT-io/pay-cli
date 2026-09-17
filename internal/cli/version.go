package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

func init() { Register(newVersionCmd) }

// versionPayload is `pay version`'s data block (§16.4).
type versionPayload struct {
	buildinfo.Info
	Update *updateStatus `json:"update,omitempty"`
}

type updateStatus struct {
	Channel         string `json:"channel"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Verification    string `json:"verification,omitempty"`
	Managed         bool   `json:"managed"`
	Manager         string `json:"manager,omitempty"`
	Hint            string `json:"hint,omitempty"`
	Error           string `json:"error,omitempty"`
}

func newVersionCmd(rt *Runtime) *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:     "version",
		Short:   "Print the PayCLI build stamp (and, with --check, whether a newer release exists).",
		GroupID: GroupAdmin,
		Args:    maxArgs(0, "pay version [--check]"),
		RunE: Handle(rt, "version", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			info := buildinfo.Current()
			managed := update.DetectManaged(info.InstallPath, rt.updateEnv())
			if info.InstallPath == "" {
				if exe, err := update.ResolveBinary(); err == nil {
					info.InstallPath = exe
					managed = update.DetectManaged(exe, rt.updateEnv())
				}
			}
			info.Managed = managed.Managed

			data := versionPayload{Info: info}
			if check {
				st := &updateStatus{
					Channel:        rt.updateChannel(),
					CurrentVersion: info.Version,
					Managed:        managed.Managed,
					Manager:        managed.Manager,
					Hint:           managed.Hint,
				}
				u := rt.updater()
				res, err := u.Check(ctx)
				switch {
				case err != nil:
					// A failed check is information, not a failure: `pay
					// version --check` on an offline machine must still print
					// the build stamp with exit 0.
					st.Error = err.Error()
					rt.Warnf("update_check_failed", "could not reach the release feed: %s", err)
				default:
					st.LatestVersion = res.LatestVersion
					st.UpdateAvailable = res.UpdateAvailable
					st.Verification = string(res.Verification.Outcome)
				}
				data.Update = st
			}
			env := output.New("version", output.KindOpResult, data)
			if data.Update != nil && data.Update.UpdateAvailable && !managed.Managed {
				env.WithNext(&output.Next{
					Reason: "update_available",
					Cmd:    "pay update-self --apply",
					Args:   map[string]any{},
				})
			}
			return env, nil
		}),
	}
	cmd.Flags().BoolVar(&check, "check", false, "also ask the release feed whether a newer version exists")

	SetHelp(cmd, &Help{
		Synopsis: []string{"pay version [--check]"},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"version":"0.1.0","commit":"…","date":"…","go":"go1.27.1","os":"linux","arch":"amd64","install_path":"/usr/local/bin/pay","managed":false}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitNetwork},
		Examples: []Example{
			{Why: "the build stamp, offline and instant", Cmd: "pay version"},
			{Why: "just the semver, for a script", Cmd: "pay version --path .version --output id"},
			{Why: "is a newer release available?", Cmd: "pay version --check"},
			{Why: "was this binary installed by a package manager?", Cmd: "pay version --path .managed"},
		},
		Mistakes: []Mistake{
			{Wrong: "Parsing `pay --version` output as JSON.",
				Right: "`pay --version` prints the bare semver for humans; use `pay version` for the envelope."},
			{Wrong: "Running `pay version --check` in a tight loop.",
				Right: "The check is bounded at 2s and cached for 24h in the state file; call it once per session."},
			{Wrong: "Calling `pay update-self` on a Homebrew/Scoop install.",
				Right: "Check `.managed` first and use the package manager's own upgrade command, which .update.hint names."},
		},
		SeeAlso: []string{"pay update-self --check", "pay doctor"},
	})
	return cmd
}

func (rt *Runtime) updateChannel() string {
	if rt.Cfg != nil && rt.Cfg.UpdateChannel != "" {
		return rt.Cfg.UpdateChannel
	}
	return "stable"
}

func (rt *Runtime) updateEnv() update.Env {
	return update.Env{
		GOPATH: rt.Env.Get("GOPATH"),
		GOBIN:  rt.Env.Get("GOBIN"),
		Home:   rt.Env.Get("HOME"),
		GOOS:   buildinfo.Current().OS,
	}
}

// updater builds the §15 self-updater over the injected clock and paths.
func (rt *Runtime) updater() *update.Updater {
	u := &update.Updater{
		CurrentVersion: buildinfo.Version(),
		Channel:        rt.updateChannel(),
		StatePath:      rt.Paths.UpdateStateFile(),
		Now:            rt.App.Now,
		Stderr:         rt.App.Stderr,
		OS:             buildinfo.Current().OS,
		Arch:           buildinfo.Current().Arch,
		Token:          rt.Env.Get("GITHUB_TOKEN"),
	}
	if exe, err := update.ResolveBinary(); err == nil {
		u.BinaryPath = exe
		u.LockPath = exe + ".lock"
	}
	return u.WithEnv(rt.updateEnv())
}
