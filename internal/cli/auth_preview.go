package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
)

// ---------------------------------------------------------------------------
// pay auth preview-secret (F9)
// ---------------------------------------------------------------------------

type previewSecretFlags struct {
	stdin bool
	clear bool
}

// newAuthPreviewSecretCmd reports, stores or clears the profile's draft-preview
// secret. The value is never printed: the report carries its source and its
// §4.4 fingerprint, which is enough to tell two secrets apart.
//
// The data keys deliberately avoid the word "secret": §5.3's structural rule
// masks any key matching (?i)token|secret, which would hide the very source
// and fingerprint this command exists to show.
func newAuthPreviewSecretCmd(rt *Runtime) *cobra.Command {
	f := &previewSecretFlags{}
	cmd := &cobra.Command{
		Use:   "preview-secret",
		Short: "Show, store (--stdin) or clear (--clear) the site's draft-preview secret for `pay url --preview`.",
		Args:  maxArgs(0, "pay auth preview-secret [--stdin | --clear]"),
		RunE: Handle(rt, "auth preview-secret", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			return rt.runPreviewSecret(f)
		}),
	}
	cmd.Flags().BoolVar(&f.stdin, "stdin", false, "read the secret from one line of stdin and store it in credentials.json (0600)")
	cmd.Flags().BoolVar(&f.clear, "clear", false, "remove the stored secret (environment variables are not touched)")
	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay auth preview-secret                      # where it resolves from, and its fingerprint",
			"printf %s \"$PREVIEW_SECRET\" | pay auth preview-secret --stdin",
			"pay auth preview-secret --clear",
		},
		Long: "`pay url --preview` needs the site's preview secret (the Payload website template's\n" +
			"PREVIEW_SECRET) to perform the draft-preview handshake. It resolves, first hit wins:\n" +
			"  1. $PAY_PREVIEW_SECRET_<PROFILE_UPPER_SNAKE>\n" +
			"  2. $PAY_PREVIEW_SECRET\n" +
			"  3. the variable named by the profile's preview_secret_env (e.g. PREVIEW_SECRET,\n" +
			"     so the app's own .env can be reused without copying the value)\n" +
			"  4. credentials.json, written by this command with --stdin\n" +
			"It is a credential: a config file that sets preview_secret is rejected, and the value\n" +
			"never appears in any output, log line or audit record.",
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"profile":"stage","configured":true,"source":"env:PAY_PREVIEW_SECRET","fingerprint":"a1b2c3d4e5f60718"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitValidation},
		Examples: []Example{
			{Why: "where does the preview secret come from right now?", Cmd: "pay auth preview-secret"},
			{Why: "store it once for this profile, never in argv", Cmd: "printf %s \"$PREVIEW_SECRET\" | pay auth preview-secret --stdin"},
			{Why: "reuse the variable the site already reads instead of storing a copy",
				Cmd: "pay config set profiles.stage.preview_secret_env PREVIEW_SECRET"},
			{Why: "forget the stored copy", Cmd: "pay auth preview-secret --clear"},
			{Why: "then mint a preview cookie", Cmd: "pay url pages 47 --preview"},
		},
		Mistakes: []Mistake{
			{Wrong: "Writing preview_secret = \"…\" into pay.toml.",
				Right: "Every config file refuses it (config_secret_in_plaintext); pay.toml is committed. Use preview_secret_env or --stdin."},
			{Wrong: "Passing the secret as an argument.",
				Right: "There is no such flag: argv is visible in ps and shell history. Pipe it to --stdin."},
			{Wrong: "Expecting --clear to unset PAY_PREVIEW_SECRET.",
				Right: "It removes the stored copy only; an exported variable still wins. .data.source shows what resolves after the clear."},
		},
		SeeAlso: []string{"pay url <collection> <id> --preview", "pay auth status", "pay doctor"},
	})
	return cmd
}

func (rt *Runtime) runPreviewSecret(f *previewSecretFlags) (*output.Envelope, error) {
	if f.stdin && f.clear {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--stdin stores and --clear removes; pass one of them").
			WithHint("pay auth preview-secret --stdin, or pay auth preview-secret --clear")
	}
	store := rt.SecretStore()
	profile := rt.Cfg.Profile
	data := map[string]any{"profile": profile, "credentials_file": store.Path()}
	switch {
	case f.stdin:
		line, err := readLine(rt.App.Stdin)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeInvalidArgs, "cannot read the preview secret from stdin")
		}
		value := strings.TrimSpace(line)
		if err := store.File.SetPreviewSecret(profile, value, rt.Now()); err != nil {
			return nil, err
		}
		data["stored"] = true
	case f.clear:
		removed, err := store.File.ClearPreviewSecret(profile)
		if err != nil {
			return nil, err
		}
		data["removed"] = removed
	}

	resolved, err := secret.ResolvePreviewSecret(secret.PreviewInput{
		Profile: profile, Env: rt.Env, SecretEnv: rt.Cfg.PreviewSecretEnv, Store: store,
	})
	if err != nil {
		return nil, err
	}
	data["configured"] = resolved != nil
	data["source"] = secret.SourceNone
	data["fingerprint"] = nil
	if resolved != nil {
		data["source"] = resolved.Source
		data["fingerprint"] = resolved.Fingerprint
	}
	data["env_vars"] = secret.PreviewSecretEnvNames(profile)
	data["profile_env_var"] = rt.Cfg.PreviewSecretEnv
	env := output.New("auth preview-secret", output.KindOpResult, data)
	if f.stdin && resolved != nil && resolved.Source != secret.SourcePreviewCredFile {
		env.AddWarning(output.Warning{
			Code:    "preview_secret_shadowed",
			Message: "the stored value was saved, but " + resolved.Source + " is set and wins over credentials.json",
			Hint:    "unset that variable to use the stored value; `pay auth preview-secret` shows which one resolves",
		})
	}
	return env, nil
}
