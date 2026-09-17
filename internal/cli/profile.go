package cli

import (
	"context"
	"sort"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// profileRow is one entry of `pay auth list`. It carries no secret: the key
// fingerprint is the only credential-derived value that may leave the process
// (§5.3).
type profileRow struct {
	Name           string `json:"name"`
	Label          string `json:"label,omitempty"`
	Default        bool   `json:"default"`
	Active         bool   `json:"active"`
	BaseURL        string `json:"base_url"`
	APIPath        string `json:"api_path"`
	AuthCollection string `json:"auth_collection"`
	AuthMode       string `json:"auth_mode"`
	Source         string `json:"source"`
	Credential     string `json:"credential"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

// profileRows builds the inventory from the two config files plus the
// credential store. It never resolves a credential — the store is consulted for
// presence and fingerprint only, and the keychain is not unlocked.
func (rt *Runtime) profileRows(ctx context.Context) ([]profileRow, error) {
	names := config.ProfileNames(rt.ProjectFile, rt.UserFile)
	stored, err := rt.SecretStore().File.Profiles()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, n := range stored {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)

	defaultProfile := ""
	for _, f := range []*config.File{rt.ProjectFile, rt.UserFile} {
		if f != nil && f.DefaultProfile != "" {
			defaultProfile = f.DefaultProfile
			break
		}
	}

	rows := make([]profileRow, 0, len(names))
	for _, name := range names {
		row := profileRow{
			Name:       name,
			Default:    name == defaultProfile,
			Active:     rt.Cfg != nil && name == rt.Cfg.Profile,
			Credential: "none",
		}
		for _, f := range []*config.File{rt.ProjectFile, rt.UserFile} {
			if f == nil {
				continue
			}
			if p, ok := f.Profile(name); ok {
				if row.BaseURL == "" && p.BaseURL != "" {
					row.BaseURL = redact.URL(p.BaseURL)
					row.Source = f.Source()
				}
				if row.APIPath == "" {
					row.APIPath = p.APIPath
				}
				if row.AuthCollection == "" {
					row.AuthCollection = p.AuthCollection
				}
				if row.AuthMode == "" {
					row.AuthMode = p.AuthMode
				}
				if row.Label == "" {
					row.Label = p.Label
				}
			}
		}
		if rec, ok, err := rt.SecretStore().File.Get(name); err == nil && ok && rec != nil {
			row.KeyFingerprint = rec.Fingerprint
			switch {
			case rec.Keyring:
				row.Credential = "keyring"
			case rec.Token != "":
				row.Credential = "jwt"
			case rec.APIKey != "":
				row.Credential = "api-key"
			}
		}
		if row.AuthCollection == "" {
			row.AuthCollection = config.DefaultAuthCollection
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func newAuthListCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every configured profile, with its endpoint and credential kind.",
		Args:  maxArgs(0, "pay auth list"),
		RunE: Handle(rt, "auth list", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			rows, err := rt.profileRows(ctx)
			if err != nil {
				return nil, err
			}
			return output.New("auth list", output.KindOpResult, map[string]any{
				"profiles": rows,
				"active":   rt.Cfg.Profile,
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth list"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"profiles":[{"name":"default","base_url":"…","credential":"api-key","key_fingerprint":"a1b2…"}],"active":"default"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitConfig},
		Examples: []Example{
			{Why: "all profiles", Cmd: "pay auth list"},
			{Why: "names only, for a script", Cmd: "pay auth list --path .profiles[].name"},
			{Why: "which profiles actually hold a credential", Cmd: "pay auth list --output table --columns name,base_url,credential"},
			{Why: "confirm which profile is active right now", Cmd: "pay auth list --path .active --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the API key in the output.",
				Right: "Only the 16-hex fingerprint is ever printed; compare fingerprints to tell two keys apart."},
			{Wrong: "Assuming a listed profile can reach its server.",
				Right: "`pay auth test --profile NAME` verifies it against /me; listing only reads local files."},
			{Wrong: "Editing credentials.json to add a profile.",
				Right: "`pay auth login --profile NAME --base-url URL` creates both the profile and its credential."},
		},
		SeeAlso: []string{"pay auth use <profile>", "pay auth status", "pay auth test"},
	})
	return cmd
}

func newAuthUseCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "use <profile>",
		Short:             "Make a profile the default for subsequent commands.",
		Args:              exactArgs(1, "pay auth use <profile>"),
		ValidArgsFunction: CompleteProfiles(rt),
		RunE: Handle(rt, "auth use", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			name := args[0]
			known := config.ProfileNames(rt.ProjectFile, rt.UserFile)
			stored, _ := rt.SecretStore().File.Profiles()
			known = append(known, stored...)
			found := false
			for _, n := range known {
				if n == name {
					found = true
					break
				}
			}
			if !found {
				return nil, apierr.New(apierr.CodeProfileUnknown,
					"no profile named %q", name).
					WithHint("pay auth login --profile %s --base-url https://example.com", name).
					WithDidYouMean(apierr.DidYouMean(name, known)...)
			}
			rt.UserFile.DefaultProfile = name
			rt.UserFile.Path = rt.Paths.ConfigFile
			rt.UserFile.Kind = config.KindUser
			if err := rt.UserFile.Save(); err != nil {
				return nil, err
			}
			return output.New("auth use", output.KindOpResult, map[string]any{
				"default_profile": name,
				"path":            rt.UserFile.Path,
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth use <profile>"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"default_profile":"staging","path":"/home/u/.config/pay/config.toml"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitConfig, apierr.ExitInternal},
		Examples: []Example{
			{Why: "switch the default", Cmd: "pay auth use staging"},
			{Why: "switch back", Cmd: "pay auth use default"},
			{Why: "one-off override without switching", Cmd: "pay find pages --profile staging"},
			{Why: "confirm the switch", Cmd: "pay config get profile"},
		},
		Mistakes: []Mistake{
			{Wrong: "Using this inside a script to scope one command.",
				Right: "It edits the config file for every future command; pass --profile NAME instead."},
			{Wrong: "Expecting it to beat PAY_PROFILE.",
				Right: "The environment variable wins; unset it or pass --profile."},
			{Wrong: "Switching to a profile with no credential and wondering why reads are anonymous.",
				Right: "Run `pay auth test` after switching; anonymous mode is reported in meta.auth_mode."},
		},
		SeeAlso: []string{"pay auth list", "pay auth login", "pay config explain"},
	})
	return cmd
}

func newAuthRenameCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "rename <old> <new>",
		Short:             "Rename a profile in both the config file and the credential store.",
		Args:              exactArgs(2, "pay auth rename <old> <new>"),
		ValidArgsFunction: CompleteProfiles(rt),
		RunE: Handle(rt, "auth rename", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			oldName, newName := args[0], args[1]
			if oldName == newName {
				return nil, apierr.New(apierr.CodeInvalidArgs, "the old and new profile names are identical")
			}
			profile, ok := rt.UserFile.Profile(oldName)
			movedProfile := false
			if ok {
				if _, taken := rt.UserFile.Profile(newName); taken {
					return nil, apierr.New(apierr.CodeInvalidArgs,
						"a profile named %q already exists", newName).
						WithHint("pick another name, or remove it with `pay config unset profiles.%s`", newName)
				}
				rt.UserFile.SetProfile(newName, profile)
				delete(rt.UserFile.Profiles, oldName)
				if rt.UserFile.DefaultProfile == oldName {
					rt.UserFile.DefaultProfile = newName
				}
				rt.UserFile.Path = rt.Paths.ConfigFile
				rt.UserFile.Kind = config.KindUser
				if err := rt.UserFile.Save(); err != nil {
					return nil, err
				}
				movedProfile = true
			}
			movedCredential, err := rt.SecretStore().File.Rename(oldName, newName)
			if err != nil {
				return nil, err
			}
			if !movedProfile && !movedCredential {
				return nil, apierr.New(apierr.CodeProfileUnknown,
					"no profile named %q", oldName).
					WithDidYouMean(apierr.DidYouMean(oldName, config.ProfileNames(rt.ProjectFile, rt.UserFile))...)
			}
			return output.New("auth rename", output.KindOpResult, map[string]any{
				"from": oldName, "to": newName,
				"profile_moved": movedProfile, "credential_moved": movedCredential,
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth rename <old> <new>"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"from":"default","to":"prod","profile_moved":true,"credential_moved":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation, apierr.ExitConfig, apierr.ExitInternal},
		Examples: []Example{
			{Why: "give the starter profile a real name", Cmd: "pay auth rename default prod"},
			{Why: "then make it the default again", Cmd: "pay auth use prod"},
			{Why: "verify both halves moved", Cmd: "pay auth rename a b --path .credential_moved"},
			{Why: "inspect the result", Cmd: "pay auth list"},
		},
		Mistakes: []Mistake{
			{Wrong: "Renaming onto an existing name.",
				Right: "Refused with invalid_args; unset the target first or choose another name."},
			{Wrong: "Expecting a keychain entry to move silently.",
				Right: "It does move, but the OS may prompt for access; credential_moved reports the outcome."},
			{Wrong: "Renaming a profile pinned by ./pay.toml.",
				Right: "Project pins are edited by hand; only the user config file is rewritten here."},
		},
		SeeAlso: []string{"pay auth list", "pay auth use", "pay auth logout"},
	})
	return cmd
}
