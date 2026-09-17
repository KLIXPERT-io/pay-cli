package cli

import (
	"bufio"
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

func init() { Register(newAuthCmd) }

func newAuthCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "auth",
		Short:   "Manage profiles and credentials (§5).",
		GroupID: GroupAdmin,
		RunE: Handle(rt, "auth", func(_ context.Context, _ *Runtime, _ []string) (*output.Envelope, error) {
			return nil, apierr.New(apierr.CodeInvalidArgs, "pay auth needs a subcommand").
				WithHint("pay auth login --profile default --base-url https://example.com --api-key-stdin").
				WithDidYouMean("login", "list", "status", "test")
		}),
	}
	cmd.AddCommand(
		newAuthLoginCmd(rt),
		newAuthListCmd(rt),
		newAuthUseCmd(rt),
		newAuthLogoutCmd(rt),
		newAuthTestCmd(rt),
		newAuthRenameCmd(rt),
		newAuthStatusCmd(rt),
		newAuthFixPermsCmd(rt),
	)
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay auth login|list|use|logout|test|rename|status|fix-perms"},
		Long: "Auth is verified by ONE thing only: GET {base}{api_path}/{authCollection}/me returning\n" +
			"a non-null user. Status codes prove nothing here — a wrong API key returns 200 with\n" +
			"{\"user\":null} (verified), and many projects allow unauthenticated reads.",
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig},
		Examples: []Example{
			{Why: "log in with an API key, never putting it in argv", Cmd: "printf %s \"$PAYLOAD_KEY\" | pay auth login --profile default --base-url http://localhost:3000 --api-key-stdin"},
			{Why: "log in with email + password instead", Cmd: "pay auth login --jwt --profile default --base-url http://localhost:3000 --email you@example.com --password-stdin"},
			{Why: "is the stored credential still good?", Cmd: "pay auth test"},
			{Why: "what is configured, without touching the network", Cmd: "pay auth status"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing --api-key on the command line on a shared machine.",
				Right: "argv is world-readable on Linux; pipe the key to --api-key-stdin."},
			{Wrong: "Believing a 200 from `pay find pages` means auth works.",
				Right: "Many projects allow anonymous reads. Run `pay auth test`, which asserts user != null."},
			{Wrong: "Using `pay auth login --api-key` on a project with no useAPIKey collection.",
				Right: "It can never succeed there. `pay doctor` reports which collections support API keys and prints the --jwt command instead."},
		},
		SeeAlso: []string{"pay whoami", "pay doctor", "pay config explain"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// auth collection resolution
// ---------------------------------------------------------------------------

// authCollection resolves the slug that appears in the api-key Authorization
// header and in the /me path. The ladder is §7.0's: configured, then the cached
// Stage -1 answer, then discovery.
func (rt *Runtime) authCollection(ctx context.Context) (string, string, error) {
	if rt.authSlug != "" {
		return rt.authSlug, rt.authSlugSource, nil
	}
	if rt.Cfg.AuthCollection != "" && rt.Cfg.AuthCollection != config.DefaultAuthCollection {
		rt.adoptAuthCollection(rt.Cfg.AuthCollection, rt.Cfg.Sources["auth_collection"])
		return rt.authSlug, rt.authSlugSource, nil
	}
	if sc, err := rt.Scope(); err == nil {
		if res, ok, warn := rt.Cache().LookupAuthResolution(sc); ok && res.AuthCollection != "" {
			rt.adoptAuthCollection(res.AuthCollection, discovery.SourceCached)
			return rt.authSlug, rt.authSlugSource, nil
		} else if warn != nil {
			rt.Warn(*warn)
		}
	}
	m, err := rt.Discovery(ctx)
	if err != nil {
		return "", "", err
	}
	if m.Identity.AuthCollection != nil && *m.Identity.AuthCollection != "" {
		rt.adoptAuthCollection(*m.Identity.AuthCollection, m.Identity.AuthCollectionSource)
		return rt.authSlug, rt.authSlugSource, nil
	}
	return "", discovery.SourceUnknown, apierr.New(apierr.CodeAuthCollectionUnknown,
		"no auth-enabled collection could be resolved on this project").
		WithHint("pass --auth-collection SLUG, or run `pay doctor` to see which collections are auth collections")
}

// ---------------------------------------------------------------------------
// login
// ---------------------------------------------------------------------------

type loginFlags struct {
	jwt           bool
	email         string
	username      string
	passwordStdin bool
	keyring       bool
	noVerify      bool
}

func newAuthLoginCmd(rt *Runtime) *cobra.Command {
	f := &loginFlags{}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a credential for a profile and verify it against /{auth}/me (§5.4).",
		Args:  maxArgs(0, "pay auth login --profile NAME --base-url URL [--api-key-stdin|--jwt …]"),
		RunE: Handle(rt, "auth login", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			if f.jwt {
				return rt.loginJWT(ctx, f)
			}
			return rt.loginAPIKey(ctx, f)
		}),
	}
	cmd.Flags().BoolVar(&f.jwt, "jwt", false, "log in with email/username + password instead of an API key")
	cmd.Flags().StringVar(&f.email, "email", "", "email address (--jwt)")
	cmd.Flags().StringVar(&f.username, "username", "", "username (--jwt), for projects with loginWithUsername")
	cmd.Flags().BoolVar(&f.passwordStdin, "password-stdin", false, "read the password from the first line of stdin (--jwt)")
	cmd.Flags().BoolVar(&f.keyring, "keyring", false, "store the secret in the OS keychain instead of credentials.json")
	cmd.Flags().BoolVar(&f.noVerify, "no-verify", false, "skip the /me verification round trip")

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay auth login --profile NAME --base-url URL [--api-key KEY|--api-key-stdin|--api-key-file F] [--auth-collection SLUG] [--keyring] [--no-verify]",
			"pay auth login --jwt --profile NAME --base-url URL (--email X|--username X) --password-stdin [--keyring] [--no-verify]",
		},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"profile":"default","base_url":"…","auth_mode":"api-key","auth_collection":"users","key_fingerprint":"a1b2…","verified":true,"stored":"file"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		FlagInfo: map[string]FlagInfo{
			"keyring": {Note: "opt-in; PAY_KEYRING=off disables it globally, PAY_KEYRING=force makes it mandatory"},
		},
		Examples: []Example{
			{Why: "API key from an env var, never in argv", Cmd: "printf %s \"$PAYLOAD_KEY\" | pay auth login --profile default --base-url http://localhost:3000 --api-key-stdin"},
			{Why: "API key from a 0600 file", Cmd: "pay auth login --profile prod --base-url https://cms.example.com --api-key-file ~/.secrets/payload.key"},
			{Why: "email + password, token stored in the OS keychain", Cmd: "pay auth login --jwt --profile prod --base-url https://cms.example.com --email you@example.com --password-stdin --keyring"},
			{Why: "the auth collection is not called users here", Cmd: "pay auth login --profile prod --base-url https://cms.example.com --auth-collection admins --api-key-stdin"},
			{Why: "offline setup, verify later", Cmd: "pay auth login --profile prod --base-url https://cms.example.com --api-key-stdin --no-verify"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing --password on the command line.",
				Right: "There is no --password flag by design; use --password-stdin."},
			{Wrong: "Assuming --auth-collection defaults to \"users\".",
				Right: "It defaults to \"auto\" and is discovered. Pin it with --auth-collection when discovery has no permission to look."},
			{Wrong: "Using --no-verify and then debugging a 403 an hour later.",
				Right: "Run `pay auth test` as soon as the network is available; it asserts user != null."},
			{Wrong: "Storing a JWT and expecting it to last forever.",
				Right: "Tokens expire; `pay auth status` prints token_exp and PayCLI refreshes or tells you to log in again."},
		},
		SeeAlso: []string{"pay auth test", "pay auth status", "pay doctor", "pay whoami"},
	})
	return cmd
}

func (rt *Runtime) loginAPIKey(ctx context.Context, f *loginFlags) (*output.Envelope, error) {
	cred, err := secret.Resolve(ctx, secret.Input{
		Profile:   rt.Cfg.Profile,
		BaseURL:   rt.Cfg.BaseURL,
		Env:       rt.Env,
		Mode:      secret.ModeAPIKey,
		APIKeyEnv: rt.Cfg.APIKeyEnv,
		Store:     rt.SecretStore(),
		Now:       rt.Now(),
		Flags: secret.Flags{
			APIKey:      rt.Flags.apiKey,
			APIKeyStdin: rt.Flags.apiKeyStdin,
			APIKeyFile:  rt.Flags.apiKeyFile,
			Stdin:       rt.App.Stdin,
			IsTTY:       rt.App.StdinIsTTY,
		},
	})
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Value == "" {
		return nil, apierr.New(apierr.CodeAuthMissing,
			"no API key was supplied for profile %q", rt.Cfg.Profile).
			WithHint("printf %%s \"$PAYLOAD_KEY\" | pay auth login --profile %s --base-url %s --api-key-stdin",
				rt.Cfg.Profile, rt.Cfg.BaseURL)
	}
	rt.seedCredential(cred)

	slug, slugSource, verified, identity, err := rt.verifyLogin(ctx, f, cred.Mode)
	if err != nil {
		return nil, err
	}
	saved, err := rt.SecretStore().SaveAPIKey(ctx, rt.Cfg.Profile, cred.Value, f.keyring, rt.Now())
	if err != nil {
		return nil, err
	}
	if saved.Warning != "" {
		rt.Warnf("keyring", "%s", saved.Warning)
	}
	if err := rt.persistProfile(slug, string(secret.ModeAPIKey)); err != nil {
		return nil, err
	}
	rt.warnGitignore()

	data := map[string]any{
		"profile":                rt.Cfg.Profile,
		"base_url":               rt.Cfg.BaseURL,
		"api_path":               rt.Cfg.APIPath,
		"auth_mode":              string(secret.ModeAPIKey),
		"auth_collection":        slug,
		"auth_collection_source": slugSource,
		"key_fingerprint":        saved.Fingerprint,
		"key_source":             cred.Source,
		"verified":               verified,
		"stored":                 saved.Target,
		"credentials_file":       rt.SecretStore().Path(),
	}
	if identity != nil {
		data["user_id"] = identity.UserID
		data["can_access_admin"] = identity.CanAccessAdmin
	}
	return output.New("auth login", output.KindOpResult, data), nil
}

func (rt *Runtime) loginJWT(ctx context.Context, f *loginFlags) (*output.Envelope, error) {
	if f.email == "" && f.username == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--jwt needs an identifier").
			WithHint("pay auth login --jwt --email you@example.com --password-stdin")
	}
	if f.email != "" && f.username != "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--email and --username are mutually exclusive")
	}
	if !f.passwordStdin {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--jwt requires --password-stdin").
			WithHint("there is no --password flag by design; argv is world-readable")
	}
	password, err := readLine(rt.App.Stdin)
	if err != nil || password == "" {
		return nil, apierr.New(apierr.CodeAuthMissing,
			"no password was read from stdin").
			WithHint("printf %%s \"$PASSWORD\" | pay auth login --jwt --email you@example.com --password-stdin")
	}

	// The login request itself is unauthenticated, so the client is built
	// anonymously and re-credentialed once the token exists.
	rt.seedCredential(&secret.Credential{Mode: secret.ModeAnonymous, Source: secret.SourceNone, Fingerprint: secret.Anon})
	slug, slugSource, err := rt.authCollection(ctx)
	if err != nil {
		return nil, err
	}
	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}
	res, err := client.Login(ctx, payload.LoginInput{
		Collection: slug,
		Email:      f.email,
		Username:   f.username,
		Password:   password,
	})
	if err != nil {
		return nil, err
	}
	token := secret.NewJWT(res.Token, res.Exp)
	identifier, field := f.email, "email"
	if f.username != "" {
		identifier, field = f.username, "username"
	}

	verified := false
	var identity *payload.Identity
	if !f.noVerify {
		authed := client.WithCredential(payload.AuthModeJWT, slug, res.Token)
		identity, err = authed.Me(ctx, slug)
		if err != nil {
			return nil, err
		}
		if err := payload.AssertIdentity(identity); err != nil {
			return nil, err
		}
		verified = true
	}

	saved, err := rt.SecretStore().SaveJWT(ctx, rt.Cfg.Profile, token, identifier, field, f.keyring, rt.Now())
	if err != nil {
		return nil, err
	}
	if saved.Warning != "" {
		rt.Warnf("keyring", "%s", saved.Warning)
	}
	if err := rt.persistProfile(slug, string(secret.ModeJWT)); err != nil {
		return nil, err
	}
	rt.warnGitignore()
	rt.cred = &secret.Credential{
		Mode: secret.ModeJWT, Value: res.Token, Source: secret.SourceStoredJWT,
		Fingerprint: saved.Fingerprint, Exp: token.Exp,
		LoginIdentifier: identifier, LoginField: field,
	}

	data := map[string]any{
		"profile":                rt.Cfg.Profile,
		"base_url":               rt.Cfg.BaseURL,
		"api_path":               rt.Cfg.APIPath,
		"auth_mode":              string(secret.ModeJWT),
		"auth_collection":        slug,
		"auth_collection_source": slugSource,
		"login_field":            field,
		"login_identifier":       identifier,
		"key_fingerprint":        saved.Fingerprint,
		"verified":               verified,
		"stored":                 saved.Target,
		"credentials_file":       rt.SecretStore().Path(),
	}
	if !token.Exp.IsZero() {
		data["token_exp"] = token.Exp.UTC().Format(time.RFC3339)
	}
	if identity != nil {
		data["user_id"] = identity.UserID
		data["can_access_admin"] = identity.CanAccessAdmin
	}
	return output.New("auth login", output.KindOpResult, data), nil
}

// verifyLogin resolves the auth collection and, unless --no-verify, asserts
// §5.4's single truth: body.user != null.
func (rt *Runtime) verifyLogin(ctx context.Context, f *loginFlags, mode secret.Mode) (string, string, bool, *payload.Identity, error) {
	slug, source, err := rt.authCollection(ctx)
	if err != nil {
		if f.noVerify {
			// Without verification an unresolved slug is survivable: the
			// credential is stored and `pay auth test` resolves it later.
			return config.DefaultAuthCollection, discovery.SourceUnknown, false, nil, nil
		}
		return "", "", false, nil, err
	}
	if f.noVerify || mode == secret.ModeAnonymous {
		return slug, source, false, nil, nil
	}
	client, err := rt.Client(ctx)
	if err != nil {
		return "", "", false, nil, err
	}
	identity, err := client.Me(ctx, slug)
	if err != nil {
		return "", "", false, nil, err
	}
	if err := payload.AssertIdentity(identity); err != nil {
		return "", "", false, nil, err
	}
	return slug, source, true, identity, nil
}

// seedCredential installs an already-resolved credential so the lazy chain does
// not run a second time and the client is built with exactly this secret.
func (rt *Runtime) seedCredential(c *secret.Credential) {
	rt.credOnce.Do(func() { rt.cred = c })
	rt.rebuildLogger()
}

// persistProfile writes the connection half of the login into the user config
// file. The secret never goes here (§4.2).
func (rt *Runtime) persistProfile(authCollection, mode string) error {
	profile, _ := rt.UserFile.Profile(rt.Cfg.Profile)
	profile.BaseURL = rt.Cfg.BaseURL
	if rt.Flags.changed("api-path") {
		profile.APIPath = rt.Cfg.APIPath
	}
	if rt.Flags.changed("auth-collection") && authCollection != "" {
		profile.AuthCollection = authCollection
	}
	if profile.AuthMode == "" || rt.Flags.changed("auth-mode") {
		profile.AuthMode = mode
	}
	rt.UserFile.SetProfile(rt.Cfg.Profile, profile)
	if rt.UserFile.DefaultProfile == "" {
		rt.UserFile.DefaultProfile = rt.Cfg.Profile
	}
	rt.UserFile.Path = rt.Paths.ConfigFile
	rt.UserFile.Kind = config.KindUser
	return rt.UserFile.Save()
}

// warnGitignore is §5.3's last bullet: a project whose .gitignore does not
// cover the agent skill directory is one `git add -A` away from publishing it.
func (rt *Runtime) warnGitignore() {
	if !rt.Project.Found() {
		return
	}
	if covered, known := skills.GitignoreCovers(rt.Project.Dir, ".claude/skills/pay"); known && !covered {
		rt.Warnf(skills.WarnNotIgnored,
			"%s/.gitignore does not cover the agent skill directory (.claude/skills/pay)", rt.Project.Dir)
	}
}

func readLine(r interface{ Read([]byte) (int, error) }) (string, error) {
	if r == nil {
		return "", apierr.New(apierr.CodeAuthMissing, "no standard input is attached")
	}
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// ---------------------------------------------------------------------------
// logout / test / status / fix-perms
// ---------------------------------------------------------------------------

func newAuthLogoutCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "logout [profile]",
		Short:             "Delete a profile's stored credential (the profile itself survives).",
		Args:              maxArgs(1, "pay auth logout [profile]"),
		ValidArgsFunction: CompleteProfiles(rt),
		RunE: Handle(rt, "auth logout", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			name := rt.Cfg.Profile
			if len(args) == 1 {
				name = args[0]
			}
			removed, err := rt.SecretStore().Logout(ctx, name)
			if err != nil {
				return nil, err
			}
			return output.New("auth logout", output.KindOpResult, map[string]any{
				"profile": name, "removed": removed, "credentials_file": rt.SecretStore().Path(),
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth logout [profile]"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"profile":"prod","removed":true,"credentials_file":"…"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitInternal},
		Examples: []Example{
			{Why: "forget the active profile's credential", Cmd: "pay auth logout"},
			{Why: "forget one specific profile", Cmd: "pay auth logout staging"},
			{Why: "confirm nothing is left", Cmd: "pay auth list --path .profiles[].credential"},
			{Why: "rotate: log out then back in", Cmd: "pay auth logout && printf %s \"$NEW_KEY\" | pay auth login --api-key-stdin"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting logout to delete the profile.",
				Right: "It removes only the secret. Remove the profile with `pay config unset profiles.NAME`."},
			{Wrong: "Expecting it to invalidate the key server-side.",
				Right: "Payload API keys are revoked in the admin UI; this is a local delete."},
			{Wrong: "Assuming a keychain entry survives.",
				Right: "Logout removes the keychain entry too; `removed` reports what happened."},
		},
		SeeAlso: []string{"pay auth login", "pay auth list"},
	})
	return cmd
}

func newAuthTestCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Verify the active credential against /{auth}/me (exit 0 verified, 2 not).",
		Args:  maxArgs(0, "pay auth test [--profile P]"),
		RunE: Handle(rt, "auth test", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			cred, err := rt.Credential(ctx)
			if err != nil {
				return nil, err
			}
			if cred.Anonymous() {
				return nil, apierr.New(apierr.CodeAuthMissing,
					"profile %q has no credential; PayCLI would run anonymously", rt.Cfg.Profile).
					WithHint("pay auth login --profile %s --base-url %s --api-key-stdin", rt.Cfg.Profile, rt.Cfg.BaseURL)
			}
			slug, slugSource, err := rt.authCollection(ctx)
			if err != nil {
				return nil, err
			}
			client, err := rt.Client(ctx)
			if err != nil {
				return nil, err
			}
			identity, err := client.Me(ctx, slug)
			if err != nil {
				return nil, err
			}
			if err := payload.AssertIdentity(identity); err != nil {
				return nil, err
			}
			return output.New("auth test", output.KindOpResult, map[string]any{
				"profile":                rt.Cfg.Profile,
				"base_url":               rt.Cfg.BaseURL,
				"auth_mode":              string(cred.Mode),
				"auth_collection":        slug,
				"auth_collection_source": slugSource,
				"key_fingerprint":        cred.Fingerprint,
				"key_source":             cred.Source,
				"user_id":                identity.UserID,
				"can_access_admin":       identity.CanAccessAdmin,
				"strategy":               identity.Strategy,
				"verified":               true,
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth test [--profile P]"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"profile":"default","auth_mode":"api-key","auth_collection":"users","user_id":66,"verified":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "is the active profile good?", Cmd: "pay auth test"},
			{Why: "check another profile without switching", Cmd: "pay auth test --profile staging"},
			{Why: "use it as a guard in a script", Cmd: "pay auth test >/dev/null || exit 2"},
			{Why: "check a key that is only in the environment", Cmd: "PAY_API_KEY=… pay auth test"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating exit 0 from any other command as proof of auth.",
				Right: "Anonymous reads succeed on many projects; only this command asserts user != null."},
			{Wrong: "Reading the HTTP status instead of .verified.",
				Right: "A wrong key returns HTTP 200 with user:null. PayCLI turns that into auth_invalid, exit 2."},
			{Wrong: "Running it against a project whose auth collection is not discoverable.",
				Right: "Pass --auth-collection SLUG; `pay doctor` lists the candidates."},
		},
		SeeAlso: []string{"pay whoami", "pay auth status", "pay doctor"},
	})
	return cmd
}

func newAuthStatusCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the active profile's auth configuration without touching the network (§5.3).",
		Args:  maxArgs(0, "pay auth status"),
		RunE: Handle(rt, "auth status", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			data := map[string]any{
				"profile":                rt.Cfg.Profile,
				"base_url":               rt.Cfg.BaseURL,
				"auth_mode":              string(rt.Cfg.AuthMode),
				"auth_collection":        rt.Cfg.AuthCollection,
				"auth_collection_source": rt.Cfg.Sources["auth_collection"],
				"key_fingerprint":        nil,
				"key_source":             secret.SourceNone,
				"token_exp":              nil,
				"authenticated":          false,
				"credentials_file":       rt.SecretStore().Path(),
				"keyring_mode":           string(rt.Cfg.KeyringMode),
			}
			if sc, err := rt.Scope(); err == nil {
				if res, ok, _ := rt.Cache().LookupAuthResolution(sc); ok && res.AuthCollection != "" {
					data["auth_collection"] = res.AuthCollection
					data["auth_collection_source"] = discovery.SourceCached
				}
			}
			if rec, ok, err := rt.SecretStore().File.Get(rt.Cfg.Profile); err == nil && ok && rec != nil {
				data["key_fingerprint"] = rec.Fingerprint
				data["authenticated"] = true
				switch {
				case rec.Keyring:
					data["key_source"] = secret.SourceKeyringName
				case rec.Token != "":
					data["key_source"] = secret.SourceStoredJWT
				default:
					data["key_source"] = secret.SourceCredFile
				}
				if rec.TokenExp != nil {
					data["token_exp"] = rec.TokenExp.UTC().Format(time.RFC3339)
					data["token_expired"] = rec.JWT().Expired(rt.Now())
				}
				if rec.AuthMode != "" {
					data["auth_mode"] = string(rec.AuthMode)
				}
			}
			for _, name := range secret.APIKeyEnvNames(rt.Cfg.Profile) {
				if rt.Env.Has(name) {
					data["key_source"] = "env:" + name
					data["authenticated"] = true
					break
				}
			}
			return output.New("auth status", output.KindOpResult, data), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth status"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"profile":"default","auth_mode":"api-key","auth_collection":"users","key_fingerprint":"a1b2…","key_source":"file:credentials.json","authenticated":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitConfig},
		Examples: []Example{
			{Why: "what is configured, offline", Cmd: "pay auth status"},
			{Why: "which fingerprint is in play?", Cmd: "pay auth status --path .key_fingerprint --output id"},
			{Why: "has the stored token expired?", Cmd: "pay auth status --path .token_expired"},
			{Why: "compare two profiles", Cmd: "pay auth status --profile prod --path .key_fingerprint --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading `authenticated: true` as \"the server accepts this credential\".",
				Right: "It means a credential exists locally. `pay auth test` is the network check."},
			{Wrong: "Looking here for the key itself.",
				Right: "Only the fingerprint is ever printed (§5.3). Two identical fingerprints mean the same key."},
			{Wrong: "Expecting auth_collection to be resolved on a cold cache.",
				Right: "It reads auth-resolution.json only; run `pay discover` to resolve \"auto\"."},
		},
		SeeAlso: []string{"pay auth test", "pay auth list", "pay doctor"},
	})
	return cmd
}

func newAuthFixPermsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fix-perms",
		Short: "Restore mode 0600 on credentials.json and 0700 on the config directory.",
		Args:  maxArgs(0, "pay auth fix-perms"),
		RunE: Handle(rt, "auth fix-perms", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			path := rt.SecretStore().Path()
			if err := rt.SecretStore().File.FixPerms(); err != nil {
				return nil, err
			}
			return output.New("auth fix-perms", output.KindOpResult, map[string]any{
				"credentials_file": path, "mode": "0600", "config_dir": rt.Paths.ConfigDir, "fixed": true,
			}), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay auth fix-perms"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"credentials_file":"…","mode":"0600","fixed":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitInternal},
		Examples: []Example{
			{Why: "after a restore from backup widened the mode", Cmd: "pay auth fix-perms"},
			{Why: "the exact remedy auth_insecure_permissions names", Cmd: "pay auth fix-perms && pay auth test"},
			{Why: "confirm the path it touched", Cmd: "pay auth fix-perms --path .credentials_file --output id"},
			{Why: "inside a container image build", Cmd: "pay auth fix-perms --quiet"},
		},
		Mistakes: []Mistake{
			{Wrong: "chmod 644 credentials.json to \"make it readable\".",
				Right: "PayCLI refuses to read a world-readable credential file; run this instead."},
			{Wrong: "Running it to fix a wrong key.",
				Right: "Permissions are not the credential; use `pay auth login` to replace the key."},
			{Wrong: "Expecting it to work on Windows ACLs.",
				Right: "Windows has no POSIX mode; the command is a no-op there and reports so."},
		},
		SeeAlso: []string{"pay auth status", "pay config paths"},
	})
	return cmd
}
