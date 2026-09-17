package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// globalTarget is the manifest's knowledge about one global.
type globalTarget struct {
	Slug   string
	Global *discovery.Global
	Shard  *discovery.Shard
}

func (g *globalTarget) envTarget() *output.Target {
	out := &output.Target{Kind: "global", Slug: g.Slug}
	if g.Global != nil {
		out.Singular = g.Global.Labels.Singular
	}
	return out
}

func (g *globalTarget) flags() discovery.GlobalFlags {
	if g == nil || g.Global == nil {
		return discovery.GlobalFlags{}
	}
	return g.Global.Flags
}

// asCollTarget adapts a global to the shared write helpers, which key off the
// shard for --set coercion and the echo-diff.
func (g *globalTarget) asCollTarget() *collTarget {
	return &collTarget{Slug: g.Slug, Shard: g.Shard}
}

// global resolves a global slug against the manifest.
func (d *Deps) global(ctx context.Context, slug string) (*globalTarget, error) {
	if slug == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "a global slug is required").
			WithHint("run `pay globals list` for this project's globals")
	}
	t := &globalTarget{Slug: slug}
	if d == nil || d.Manifest == nil {
		return t, nil
	}
	g, err := d.Manifest.ResolveGlobal(slug)
	if err != nil {
		return nil, err
	}
	t.Global = g
	t.Shard = d.shardFor(ctx, slug, cache.KindGlobal)
	return t, nil
}

// newGlobalsCmd builds the `pay globals` sub-tree (§9.2).
func init() { Register(newGlobalsCmd) }

func newGlobalsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "globals",
		Short:   "Read and write Payload globals",
		Long: `Globals are single documents with no id and no bulk verbs.

A global's update is a POST, not a PATCH (Payload models the update operation of
a global as a POST), and it is an L2 operation: there is exactly one of each, so
a wrong write has no sibling to fall back on.`,
	}
	cmd.AddCommand(newGlobalsListCmd(rt), newGlobalsGetCmd(rt), newGlobalsUpdateCmd(rt))

	SetHelp(cmd, globalsGroupHelp())
	return cmd
}

func newGlobalsListCmd(rt *Runtime) *cobra.Command {
	var includeInternal bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List this project's globals",
		Long: `List the globals discovery found, with their capability flags.

This reads the cached manifest and issues no request of its own.`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = runData(rt, safety.CmdGlobalsList, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			if d.Manifest == nil {
				return nil, apierr.New(apierr.CodeDiscoveryFailed,
					"no capability manifest is cached for this profile").
					WithHint("run `pay discover` first")
			}
			run := d.beginRun()
			rows := make([]map[string]any, 0, len(d.Manifest.Globals))
			for _, g := range d.Manifest.Globals {
				if g.Internal && !includeInternal {
					continue
				}
				rows = append(rows, map[string]any{
					"slug":         g.Slug,
					"label":        g.Labels.Singular,
					"internal":     g.Internal,
					"reachability": g.Reachability,
					"versions":     g.Flags.Versions,
					"drafts":       g.Flags.Drafts,
					"read":         g.Permissions.Read,
					"update":       g.Permissions.Update,
					"fields_count": g.FieldsCount,
				})
			}
			env := output.New(safety.CmdGlobalsList, output.KindDocList, rows).
				WithMeta(run.meta())
			return env, nil
		}
	})
	cmd.Flags().BoolVar(&includeInternal, "include-internal", false, "include Payload's own internal globals")

	SetHelp(cmd, globalsListHelp())
	return cmd
}

// globalsGroupHelp is §10.5's model for the `pay globals` group itself.
func globalsGroupHelp() *Help {
	return &Help{
		Synopsis: []string{"pay globals <list|get|update> [slug] …"},
		Globals:  true,
		Output:   OutputSpec{Kind: output.KindGlobal},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth,
			apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitConfig,
			apierr.ExitCapability},
		Examples: []Example{
			{Why: "what globals exist here", Cmd: "pay globals list"},
			{Why: "read one", Cmd: "pay globals get header"},
			{Why: "its schema, field by field", Cmd: "pay describe header"},
			{Why: "preview a write — a global update is L2", Cmd: "pay globals update header --set 'title=New title' --dry-run"},
			{Why: "its history, where the global has versions", Cmd: "pay versions list --global crm-brand --limit 2"},
		},
		Mistakes: []Mistake{
			{Wrong: "Using `pay find` or `pay get` on a global.",
				Right: "Those are collection commands. A global has one document and no id: `pay globals get <slug>`."},
			{Wrong: "Expecting `globals update` to merge like a PATCH.",
				Right: "It is a POST and REPLACES the fields you send. Read the global first, then send complete values."},
			{Wrong: "Running `globals update` in a script without --yes.",
				Right: "It is L2 and answers exit 11 (confirmation required). Preview with --dry-run, then pass --yes."},
			{Wrong: "Assuming every project has `header` and `footer`.",
				Right: "Globals are project-defined. `pay globals list` is the inventory for THIS project; the block above shows it."},
		},
		SeeAlso: []string{"pay globals list", "pay globals get <slug>",
			"pay globals update <slug>", "pay describe <slug>"},
	}
}

// globalsListHelp is §10.5's model for `pay globals list`.
func globalsListHelp() *Help {
	return &Help{
		Synopsis: []string{"pay globals list [--include-internal]"},
		Globals:  true,
		Output: OutputSpec{
			Kind:     output.KindDocList,
			Skeleton: `[{"slug":"header","label":"Header","read":true,"update":true,"versions":false,"drafts":false,"fields_count":10}]`,
		},
		// No server round trip of its own, so no 3/4/8 — but a bad argument is
		// still 5 and a cold cache is still 10.
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth,
			apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitConfig,
			apierr.ExitCapability},
		Examples: []Example{
			{Why: "every global this project defines", Cmd: "pay globals list"},
			{Why: "the slugs only, ready to loop over", Cmd: "pay globals list --path '[].slug'"},
			{Why: "include Payload's own internal globals", Cmd: "pay globals list --include-internal"},
			{Why: "then read one of them", Cmd: "pay globals get header"},
			{Why: "and its schema", Cmd: "pay describe header"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting `pay find` to work on a global.",
				Right: "A global is a single document with no id and no list route. Use `pay globals get <slug>`."},
			{Wrong: "Reading `update: false` as \"broken\".",
				Right: "It is this credential's permission on that global, as discovery measured it. `pay can update <slug>` explains it, and `pay whoami` says who you are."},
			{Wrong: "Assuming a fresh list needs a network call.",
				Right: "It reads the cached manifest. Run `pay discover --refresh` after a schema change, or pass --refresh."},
			{Wrong: "Looking for a global in `pay collections`.",
				Right: "The two inventories are separate: collections have many documents, globals exactly one. `pay explain` shows both."},
		},
		SeeAlso: []string{"pay globals get <slug>", "pay globals update <slug>",
			"pay describe <slug>", "pay explain"},
	}
}

func newGlobalsGetCmd(rt *Runtime) *cobra.Command {
	var (
		draft   bool
		depth   int
		selectF []string
		locale  string
	)
	cmd := &cobra.Command{
		Use:   "get <slug>",
		Short: "Read one global",
		Long: `Read a global document.

` + draftTrapGlobalHelp,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdGlobalsGet, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			g, err := d.global(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if draft {
				if err := checkDrafts(g.Slug, g.flags().Drafts); err != nil {
					return nil, err
				}
			}
			p := query.Params{Depth: query.IntPtr(depth), Select: selectF, Locale: locale}
			loc, locWarn, err := resolveLocale(d, g.asCollTarget(), locale, "")
			if err != nil {
				return nil, err
			}
			if loc.Fallback != nil {
				p.FallbackLocale = *loc.Fallback
			}

			run := d.beginRun()
			doc, resp, err := client.GlobalGet(ctx, g.Slug, p, payload.WithKnownRoute())
			if err != nil {
				return nil, err
			}
			meta := run.meta()
			meta.Locale = loc
			var raw []byte
			if resp != nil {
				raw = resp.Body
			}
			env := output.New(safety.CmdGlobalsGet, output.KindGlobal, doc).
				WithTarget(g.envTarget()).
				WithMeta(meta).
				WithRawBody(raw, !cfg.Redact)
			if locWarn != nil {
				env.AddWarning(*locWarn)
			}
			return env, nil
		}
	})
	cmd.Flags().BoolVar(&draft, "draft", false, "read the newest draft")
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth")
	cmd.Flags().StringSliceVar(&selectF, "select", nil, "return only these fields")
	cmd.Flags().StringVar(&locale, "locale", "", "locale code, or 'all'")

	SetHelp(cmd, globalsGetHelp())
	return cmd
}

// draftTrapGlobalHelp is the draft trap stated for a global. A global has no
// list route and therefore no --published-only flag, so find's "always pass
// --published-only" wording would send an agent to a flag that does not exist
// here (verified: exit 5, unknown flag). The verbatim §9.6.2 sentences stay on
// find and count, which do have it.
const draftTrapGlobalHelp = `A read without --draft does NOT filter out unpublished content, and a global cannot be
filtered at all: there is exactly one document and no list route, so there is no
--published-only here. On a drafts-enabled global a bare read returns _status:"draft" for
content that was never published — READ _status ON THE RETURNED DOCUMENT. --draft swaps in
the newest draft for a global that does have a published version; on a global without
drafts it fails with feature_unavailable (exit 10).`

// globalsGetHelp is §10.5's model for `pay globals get`.
func globalsGetHelp() *Help {
	return &Help{
		Synopsis: []string{"pay globals get <slug> [--depth N] [--select a,b] [--draft] [--locale CODE]"},
		Globals:  true,
		Args: []ArgSpec{
			{Name: "slug", Required: true, Type: "enum",
				ValuesFrom: "discovery.globals", Example: "header"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":  {Min: intPtr(0), Max: intPtr(10)},
			"select": {Grammar: "field,field.sub", Repeatable: true},
			"draft":  {Note: "only on a global whose versions.drafts is enabled; exit 10 otherwise"},
		},
		Output: OutputSpec{
			Kind:     output.KindGlobal,
			Skeleton: `{"id":1,"globalType":"header","navItems":[],"updatedAt":"…"}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "which globals exist at all", Cmd: "pay globals list"},
			{Why: "read one", Cmd: "pay globals get header"},
			{Why: "embed relationships one level", Cmd: "pay globals get header --depth 1"},
			{Why: "one field only", Cmd: "pay globals get footer --select navItems"},
			{Why: "a scalar straight out of it", Cmd: "pay globals get crm-brand --path '.companyName'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing --draft to a global that has no drafts.",
				Right: "It fails with feature_unavailable (exit 10). `pay globals list` reports drafts per global."},
			{Wrong: "Expecting a stable `id` to address the global by.",
				Right: "A global has exactly one document; the slug IS the address. `id` is an implementation detail of the storage."},
			{Wrong: "Using `pay get header`.",
				Right: "`pay get` is for collections. Globals live under `pay globals get <slug>`."},
			{Wrong: "Reading a draft-enabled global without --draft and assuming it is live content.",
				Right: "Same trap as a collection read: check _status, or pass --draft deliberately."},
		},
		SeeAlso: []string{"pay globals list", "pay globals update <slug>",
			"pay describe <slug>", "pay versions list --global <slug>"},
	}
}

func newGlobalsUpdateCmd(rt *Runtime) *cobra.Command {
	var (
		data        writeData
		draft       bool
		depth       int
		selectF     []string
		locale      string
		noEchoCheck bool
	)
	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Write one global",
		Long: `Update a global.

A global update is a POST to /globals/{slug} and it REPLACES the fields you
send; it is an L2 operation, so it prompts on a TTY and requires --yes in a
non-TTY. --data, --data-file, --set and --set-json combine exactly as they do on
create (§9.10.2), later winning.`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdGlobalsUpdate, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			g, err := d.global(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if draft {
				if err := checkDrafts(g.Slug, g.flags().Drafts); err != nil {
					return nil, err
				}
			}
			t := g.asCollTarget()
			body, warnings, err := data.build(d, t, g.Shard)
			if err != nil {
				return nil, err
			}
			if len(body) == 0 {
				return nil, apierr.New(apierr.CodeBadRequestBody, "globals update needs at least one field").
					WithHint("pass --set k=v, --data '{...}' or --data-file body.json")
			}

			p := query.Params{Depth: query.IntPtr(depth), Select: selectF}
			// §7.9a: `globals update` echoes the global back, exactly like the
			// `globals get` at the top of this file, which already routes here.
			if warn, err := applyWriteLocale(d, t, locale, &p); err != nil {
				return nil, err
			} else if warn != nil {
				warnings = append(warnings, *warn)
			}
			if draft {
				p.Draft = query.BoolPtr(true)
			}

			op := safety.Op{Command: safety.CmdGlobalsUpdate, Selector: safety.SelectorNone, Global: true}
			path := payload.GlobalPath(g.Slug)
			w := d.newWriteOp(op, g.Slug, "POST", path)
			w.global = true

			if cfg.DryRun {
				q, _ := p.Encode()
				return emitDryRun(d, w, safety.CmdGlobalsUpdate, "POST",
					client.URLFor(&payload.Request{Method: "POST", Path: path, Query: q}), body, 1, nil)
			}
			if err := w.confirm(1); err != nil {
				return nil, err
			}
			if err := w.pre(1); err != nil {
				return nil, err
			}

			res, err := client.GlobalUpdate(ctx, g.Slug, body, p,
				payload.WithClassify(payload.ClassifyContext{SentPaths: payload.SentPaths(body), IncludeRaw: true, NoRedact: !cfg.Redact}),
				payload.WithKnownRoute())
			if err != nil {
				w.post(false, statusOf(res), 0, 1, err.Error(), "")
				return nil, err
			}
			env := output.New(safety.CmdGlobalsUpdate, output.KindGlobal, res.Doc).
				WithTarget(g.envTarget()).
				WithChanged(&output.Changed{Updated: 1, IDs: []any{}}).
				WithMeta(w.run.meta()).
				WithRawBody(res.Raw, !cfg.Redact).
				WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    fmt.Sprintf("pay globals get %s --depth 0%s", g.Slug, d.profileFlag()),
				})
			for _, warn := range warnings {
				env.AddWarning(warn)
			}
			// See create.go: --select narrows the echo, so the dropped-input
			// check has to be narrowed with it or it fires on fields the
			// server was explicitly asked not to return.
			for _, warn := range echoWarnings(g.Slug, body, res.Doc, selectF, echoConfig{
				Shard:     g.Shard,
				Ignore:    cfg.EchoCheckIgnore,
				LocaleAll: locale == "all",
				Disabled:  noEchoCheck,
			}) {
				env.AddWarning(warn)
			}
			return w.finish(env, res.HTTP, 1, 0, "")
		}
	})
	data.register(cmd)
	cmd.Flags().BoolVar(&draft, "draft", false, "save as a draft")
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth of the echoed document")
	cmd.Flags().StringSliceVar(&selectF, "select", nil, "return only these fields")
	cmd.Flags().StringVar(&locale, "locale", "", "locale to write into")
	cmd.Flags().BoolVar(&noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")

	SetHelp(cmd, globalsUpdateHelp())
	return cmd
}

// globalsUpdateHelp is §10.5's model for `pay globals update`.
func globalsUpdateHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay globals update <slug> [--set k=v ...] [--set-json k=JSON ...]",
			"                          [--data JSON] [--data-file FILE] --yes",
		},
		Globals: true,
		Args: []ArgSpec{
			{Name: "slug", Required: true, Type: "enum",
				ValuesFrom: "discovery.globals", Example: "header"},
		},
		FlagInfo: map[string]FlagInfo{
			"set":       {Grammar: "key=value  (dotted keys nest)", Repeatable: true},
			"set-json":  {Grammar: "key=JSON", Repeatable: true},
			"data-file": {Grammar: "a path, or - for stdin"},
			"depth":     {Min: intPtr(0), Max: intPtr(10)},
		},
		Output: OutputSpec{
			Kind:     output.KindGlobal,
			Skeleton: `{"id":1,"globalType":"header","navItems":[],"updatedAt":"…"}`,
		},
		ExitCodes: DestructiveExitCodes,
		Examples: []Example{
			{Why: "always first — it is an L2 write with exactly one document to get wrong",
				Cmd: "pay globals update header --set 'title=New title' --dry-run"},
			{Why: "read what is there before replacing any of it",
				Cmd: "pay globals get footer --select navItems"},
			{Why: "run it; --yes is required in a non-TTY (else exit 11)",
				Cmd: "pay globals update header --set 'title=New title' --yes"},
			{Why: "structured values need --set-json, not --set",
				Cmd: "pay globals update footer --set-json navItems='[]' --yes"},
			{Why: "a whole body from a file",
				Cmd: "pay globals update footer --data-file ./footer.json --dry-run"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting a PATCH-style merge.",
				Right: "A global update is a POST and REPLACES the fields you send. Read the global first and send the full value of anything structured."},
			{Wrong: "Treating exit 11 as a failure.",
				Right: "11 is \"confirmation required\": there is exactly one of each global, so a wrong write has no sibling to fall back on. Re-run with --yes after --dry-run."},
			{Wrong: "`--set navItems=[]` for an array.",
				Right: "--set values are scalars. Use --set-json navItems='[]' (or --data/--data-file) for anything structured."},
			{Wrong: "Calling it with no field to change.",
				Right: "It fails with bad_request_body (exit 5). Pass at least one of --set, --set-json, --data or --data-file."},
			{Wrong: "Assuming the write is unrecoverable.",
				Right: "If the global has versions, `pay versions list --global <slug>` has the previous state and `pay versions restore` puts it back."},
		},
		SeeAlso: []string{"pay globals get <slug>", "pay globals list",
			"pay describe <slug>", "pay versions list --global <slug>"},
	}
}
