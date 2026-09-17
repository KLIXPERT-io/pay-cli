package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func init() { Register(newCanCmd) }

// canOperations is the closed set of permissions /api/access reports per
// collection.
var canOperations = []string{"create", "read", "update", "delete"}

func newCanCmd(rt *Runtime) *cobra.Command {
	var globalSlug string

	cmd := &cobra.Command{
		Use:       "can <create|read|update|delete> <collection>",
		Short:     "Answer one permission question with an exit code (0 permitted, 8 denied).",
		GroupID:   GroupDiscovery,
		ValidArgs: canOperations,
		Args:      rangeArgs(1, 2, "pay can <create|read|update|delete> <collection>"),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return CompleteEnum(canOperations...)(cmd, args, toComplete)
			}
			return CompleteCollections(rt)(cmd, nil, toComplete)
		},
		RunE: Handle(rt, "can", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			op := args[0]
			if err := oneOf("operation", op, canOperations); err != nil {
				return nil, err
			}
			target := ""
			if len(args) == 2 {
				target = args[1]
			}
			if target == "" && globalSlug == "" {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"pay can needs a collection, or --global SLUG").
					WithHint("pay can read pages")
			}
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			client, err := rt.Client(ctx)
			if err != nil {
				return nil, err
			}
			res, err := client.Access(ctx)
			if err != nil {
				return nil, err
			}

			var permitted, known bool
			kind, slug := "collection", target
			if globalSlug != "" {
				kind, slug = "global", globalSlug
				entry, ok := res.Globals[globalSlug]
				if !ok {
					return nil, apierr.New(apierr.CodeGlobalUnknown,
						"this identity sees no global named %q", globalSlug).
						WithDidYouMean(apierr.DidYouMean(globalSlug, res.GlobalSlugs())...)
				}
				permitted, known = permissionOf(entry, op)
			} else {
				if _, ok := res.Collections[target]; !ok {
					return nil, apierr.New(apierr.CodeCollectionUnknown,
						"this identity sees no collection named %q", target).
						WithHint("pay collections lists every collection visible to this credential").
						WithDidYouMean(apierr.DidYouMean(target, res.Slugs())...)
				}
				permitted, known = res.Can(target, op)
			}

			data := map[string]any{
				"operation": op, "kind": kind, "slug": slug,
				"permitted": permitted, "known": known,
			}
			if !known {
				// §7.6's tri-state rule: an absent permission key is not a
				// denial. Say so rather than inventing exit 8.
				rt.Warnf("capability_unknown",
					"/api/access reported no %q permission for %s; attempt the operation and classify the server's answer", op, slug)
				env := output.New("can", output.KindOpResult, data)
				env.WithTarget(&output.Target{Kind: kind, Slug: slug})
				return env, nil
			}
			if !permitted {
				return nil, apierr.New(apierr.CodeAccessDenied,
					"this identity may not %s %s %q", op, kind, slug).
					WithHint("pay access %s shows the full permission entry for this credential", slug)
			}
			env := output.New("can", output.KindOpResult, data)
			env.WithTarget(&output.Target{Kind: kind, Slug: slug})
			return env, nil
		}),
	}
	cmd.Flags().StringVar(&globalSlug, "global", "", "ask about a global instead of a collection")
	_ = cmd.RegisterFlagCompletionFunc("global", CompleteGlobals(rt))

	SetHelp(cmd, &Help{
		Synopsis:    []string{"pay can <create|read|update|delete> <collection>", "pay can <read|update> --global <slug>"},
		Collections: true,
		Args: []ArgSpec{
			{Name: "operation", Required: true, Type: "enum", Values: canOperations, Example: "read"},
			{Name: "collection", Required: true, Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"},
		},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"operation":"read","kind":"collection","slug":"pages","permitted":true,"known":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitAccessDenied, apierr.ExitCapability},
		Examples: []Example{
			{Why: "guard a write in a shell script", Cmd: "pay can update pages && pay update pages 12 --set title='New'"},
			{Why: "check delete permission", Cmd: "pay can delete media"},
			{Why: "check a global", Cmd: "pay can update --global header"},
			{Why: "boolean for a JSON consumer", Cmd: "pay can create posts --path .permitted"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating exit 8 as an error to retry.",
				Right: "It is a permanent denial for this credential. Use a key with more rights or change the collection's access control."},
			{Wrong: "Treating `known:false` as denied.",
				Right: "It means /api/access did not report that permission. Attempt the operation; the server's answer is authoritative."},
			{Wrong: "Calling it once per document.",
				Right: "This is collection-level. For one document use `pay access <collection> --doc ID`."},
		},
		SeeAlso: []string{"pay access", "pay whoami", "pay explain --section collections"},
	})
	return cmd
}

// permissionOf reads one permission out of an /api/access entry. Both shapes
// Payload emits are handled in internal/payload, so `pay can` and every other
// consumer of /api/access agree (§7.6).
func permissionOf(entry any, op string) (permitted bool, known bool) {
	return payload.Permission(entry, op)
}
