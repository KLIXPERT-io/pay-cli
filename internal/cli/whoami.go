package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func init() { Register(newWhoamiCmd) }

func newWhoamiCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "whoami",
		Short:   "Show the identity this credential resolves to (GET /api/{auth}/me).",
		GroupID: GroupDiscovery,
		Args:    maxArgs(0, "pay whoami"),
		RunE: Handle(rt, "whoami", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			cred, err := rt.Credential(ctx)
			if err != nil {
				return nil, err
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
			// §7.8.3: the /me body carries the plaintext apiKey on a project
			// with useAPIKey, so it is redacted before it can reach stdout and
			// it is never written to the cache.
			user, paths := redact.Value(map[string]any(identity.User))
			if len(paths) > 0 {
				rt.Warn(output.Warning{
					Code:    output.WarnRawRedacted,
					Message: "secret-bearing fields in the identity document were replaced with <redacted>",
					Paths:   paths,
				})
			}
			data := map[string]any{
				"auth_mode":              string(cred.Mode),
				"auth_collection":        slug,
				"auth_collection_source": slugSource,
				"user_id":                identity.UserID,
				"can_access_admin":       identity.CanAccessAdmin,
				"strategy":               identity.Strategy,
				"key_fingerprint":        cred.Fingerprint,
				"key_source":             cred.Source,
				"verified":               true,
				"user":                   user,
			}
			env := output.New("whoami", output.KindDoc, data)
			env.WithTarget(&output.Target{Kind: "collection", Slug: slug, ID: identity.UserID})
			return env, nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay whoami"},
		Long: "Verification is the body, not the status: a wrong API key returns HTTP 200 with\n" +
			"{\"user\":null} on Payload 3.x (verified live). PayCLI asserts user != null and\n" +
			"fails with auth_invalid (exit 2) otherwise.",
		Output:    OutputSpec{Kind: output.KindDoc, Skeleton: `{"auth_mode":"api-key","auth_collection":"users","user_id":66,"can_access_admin":true,"user":{…}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "who am I on this project?", Cmd: "pay whoami"},
			{Why: "just the id", Cmd: "pay whoami --path .user_id --output id"},
			{Why: "can this credential reach the admin panel?", Cmd: "pay whoami --path .can_access_admin"},
			{Why: "check a different profile", Cmd: "pay whoami --profile staging"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the apiKey field of the user document.",
				Right: "It is replaced with <redacted> before output. Compare .key_fingerprint instead."},
			{Wrong: "Running it in anonymous mode and expecting a user.",
				Right: "Anonymous mode never requests /me. Log in, or pass --auth-mode api-key to force the attempt."},
			{Wrong: "Using it to test read permissions.",
				Right: "`pay access` returns the full permission matrix; `pay can read pages` answers one question with an exit code."},
		},
		SeeAlso: []string{"pay auth test", "pay access", "pay can read <collection>", "pay doctor"},
	})
	return cmd
}
