package cli

import (
	"context"
	"sort"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func init() { Register(newAccessCmd) }

func newAccessCmd(rt *Runtime) *cobra.Command {
	var docID, globalSlug string

	cmd := &cobra.Command{
		Use:               "access [collection]",
		Short:             "Show what this credential is permitted to do (GET /api/access).",
		GroupID:           GroupDiscovery,
		Args:              maxArgs(1, "pay access [<collection>] [--doc ID] [--global SLUG]"),
		ValidArgsFunction: CompleteCollections(rt),
		RunE: Handle(rt, "access", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			client, err := rt.Client(ctx)
			if err != nil {
				return nil, err
			}

			if docID != "" {
				if len(args) != 1 {
					return nil, apierr.New(apierr.CodeInvalidArgs,
						"--doc needs a collection").
						WithHint("pay access pages --doc 123")
				}
				doc, err := client.DocAccess(ctx, args[0], docID)
				if err != nil {
					return nil, err
				}
				masked, _ := redact.Value(map[string]any(doc))
				env := output.New("access", output.KindOpResult, map[string]any{
					"collection": args[0], "id": docID, "permissions": masked,
				})
				env.WithTarget(&output.Target{Kind: "collection", Slug: args[0], ID: docID})
				return env, nil
			}

			res, err := client.Access(ctx)
			if err != nil {
				return nil, err
			}
			switch {
			case globalSlug != "":
				entry, ok := res.Globals[globalSlug]
				if !ok {
					return nil, apierr.New(apierr.CodeGlobalUnknown,
						"this identity sees no global named %q", globalSlug).
						WithDidYouMean(apierr.DidYouMean(globalSlug, res.GlobalSlugs())...)
				}
				masked, _ := redact.Value(entry)
				env := output.New("access", output.KindOpResult, map[string]any{
					"global": globalSlug, "permissions": masked,
				})
				env.WithTarget(&output.Target{Kind: "global", Slug: globalSlug})
				return env, nil
			case len(args) == 1:
				entry, ok := res.Collections[args[0]]
				if !ok {
					return nil, apierr.New(apierr.CodeCollectionUnknown,
						"this identity sees no collection named %q", args[0]).
						WithHint("pay collections lists every collection PayCLI can see with this credential").
						WithDidYouMean(apierr.DidYouMean(args[0], res.Slugs())...)
				}
				masked, _ := redact.Value(entry)
				env := output.New("access", output.KindOpResult, map[string]any{
					"collection": args[0], "permissions": masked,
				})
				env.WithTarget(&output.Target{Kind: "collection", Slug: args[0]})
				return env, nil
			}

			colls, _ := redact.Value(res.Collections)
			globals, _ := redact.Value(res.Globals)
			slugs := res.Slugs()
			sort.Strings(slugs)
			gslugs := res.GlobalSlugs()
			sort.Strings(gslugs)
			return output.New("access", output.KindOpResult, map[string]any{
				"can_access_admin": res.CanAccessAdmin,
				"collections":      colls,
				"globals":          globals,
				"collection_slugs": slugs,
				"global_slugs":     gslugs,
			}), nil
		}),
	}
	cmd.Flags().StringVar(&docID, "doc", "", "ask about one document (POST /api/{collection}/access/{id})")
	cmd.Flags().StringVar(&globalSlug, "global", "", "ask about a global instead of a collection")
	_ = cmd.RegisterFlagCompletionFunc("global", CompleteGlobals(rt))

	SetHelp(cmd, &Help{
		Synopsis:    []string{"pay access [<collection>] [--doc ID] [--global SLUG]"},
		Collections: true,
		Globals:     true,
		Long: "/api/access is PayCLI's primary discovery source: it answers with the full\n" +
			"collection and global inventory for THIS credential. An unauthenticated call also\n" +
			"returns 200 with a reduced set, so compare rather than reading the status code.",
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"can_access_admin":true,"collections":{"pages":{"create":{…},"read":{…}}},"collection_slugs":["pages",…]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNotFound, apierr.ExitNetwork, apierr.ExitAccessDenied, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "the whole permission matrix", Cmd: "pay access"},
			{Why: "one collection", Cmd: "pay access pages"},
			{Why: "one document (field-level permissions included)", Cmd: "pay access pages --doc 12"},
			{Why: "a global", Cmd: "pay access --global header"},
			{Why: "every slug this credential can see", Cmd: "pay access --path .collection_slugs[]"},
		},
		Mistakes: []Mistake{
			{Wrong: "Using collections[].fields as a field-name source.",
				Right: "It is `true` for a privileged key and an object only for a restricted one. Use `pay describe <collection>` for fields."},
			{Wrong: "Reading a 200 as \"my credential works\".",
				Right: "Anonymous callers also get 200 with fewer entries. `pay auth test` is the auth check."},
			{Wrong: "Scripting `pay access` per collection in a loop.",
				Right: "One call already returns every collection; filter locally with --path."},
		},
		SeeAlso: []string{"pay can <op> <collection>", "pay collections", "pay whoami"},
	})
	return cmd
}
