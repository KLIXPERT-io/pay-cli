package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newDuplicateCmd builds `pay duplicate <collection> <id>` (§9.2).
//
// The id check is not optional here: POST /{coll}/{id}/duplicate with a
// non-castable id CREATES a document rather than failing, so a typo would
// silently produce a stray record.
func init() { Register(newDuplicateCmd) }

func newDuplicateCmd(rt *Runtime) *cobra.Command {
	var (
		noDraft bool
		depth   int
		selectF []string
		locale  string
	)
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "duplicate <collection> <id>",
		Short:   "Copy one document",
		Long: `Duplicate a document.

On a drafts-enabled collection the copy is created as a draft, which is what
keeps an unreviewed copy out of public output. --no-draft asks Payload to
publish the copy immediately, and therefore re-runs the collection's full
required-field validation.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = runData(rt, safety.CmdDuplicate, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			t, err := d.collection(ctx, args[0])
			if err != nil {
				return nil, err
			}
			id := args[1]
			if e := d.checkID(t, t.Slug, id); e != nil {
				return nil, e
			}
			if dup := t.flags().Duplicate; dup != nil && !*dup {
				return nil, apierr.New(apierr.CodeFeatureUnavailable,
					"%q does not expose the /duplicate route", t.Slug).
					WithHint("read the document and `pay create %s --data-file copy.json` instead", t.Slug)
			}

			p := query.Params{Depth: query.IntPtr(depth), Select: selectF}
			// §7.9a: the copy is echoed back, and a bare locale=de would show
			// the ORIGINAL's default-locale text as the copy's translation.
			if warn, err := applyWriteLocale(d, t, locale, &p); err != nil {
				return nil, err
			} else if warn != nil {
				d.RT.Warn(*warn)
			}
			if !noDraft {
				if drafts := t.flags().Drafts; drafts == nil || *drafts {
					p.Draft = query.BoolPtr(true)
				}
			}
			if err := query.ValidateSelect(selectF, t.schema(cfg)); err != nil {
				return nil, err
			}

			op := safety.Op{Command: safety.CmdDuplicate, Selector: safety.SelectorID}
			path := "/" + t.Slug + "/" + id + "/duplicate"
			w := d.newWriteOp(op, t.Slug, "POST", path).withIDs([]any{id})

			if cfg.DryRun {
				q, _ := p.Encode()
				return emitDryRun(d, w, safety.CmdDuplicate, "POST",
					client.URLFor(&payload.Request{Method: "POST", Path: path, Query: q}), nil, 1, []any{id})
			}
			if err := w.confirm(1); err != nil {
				return nil, err
			}
			if err := w.pre(1); err != nil {
				return nil, err
			}

			res, err := client.Duplicate(ctx, t.Slug, id, p,
				payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute())
			if err != nil {
				w.post(false, statusOf(res), 0, 1, err.Error(), "")
				return nil, err
			}
			env := output.New(safety.CmdDuplicate, output.KindDoc, res.Doc).
				WithTarget(t.envTarget(res.Doc.ID())).
				WithChanged(&output.Changed{Created: 1, IDs: []any{res.Doc.ID()}}).
				WithMeta(withLocaleMeta(w.run.meta(), p)).
				WithRawBody(res.Raw, !cfg.Redact).
				WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
				})
			if warn := createdAsDraftWarning(t.Slug, res.Doc, t.Shard, t.flags().Drafts); warn != nil {
				env.AddWarning(*warn)
			}
			return w.finish(env, res.HTTP, 1, 0, "")
		}
	})
	cmd.Flags().BoolVar(&noDraft, "no-draft", false, "publish the copy immediately instead of creating a draft")
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth of the echoed document")
	cmd.Flags().StringSliceVar(&selectF, "select", nil, "return only these fields")
	cmd.Flags().StringVar(&locale, "locale", "", "locale to operate in")
	SetHelp(cmd, duplicateHelp())
	return cmd
}

// duplicateHelp is §10.5's model for `pay duplicate`.
func duplicateHelp() *Help {
	return &Help{
		Synopsis:    []string{"pay duplicate <collection> <id> [--no-draft] [--depth N] [--select a,b]"},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
			{Name: "id", Required: true, Type: "string", Example: "16"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"select":   {Grammar: "field,field.sub", Repeatable: true},
			"no-draft": {Note: "publishes the copy immediately, so the full required-field validation runs"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":28,"title":"PayCLI Probe 2","_status":"draft"}   (a NEW id; the source is untouched)`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "see the route and the draft parameter before copying",
				Cmd: "pay duplicate pages 16 --dry-run"},
			{Why: "copy a page; on a drafts-enabled collection the copy is a draft",
				Cmd: "pay duplicate pages 16"},
			{Why: "copy and publish immediately — this re-runs full validation",
				Cmd: "pay duplicate pages 16 --no-draft"},
			{Why: "copy and see the new id only",
				Cmd: "pay duplicate pages 16 --select id"},
			{Why: "check the collection supports it before trying (look for `duplicate` in features)",
				Cmd: "pay collections --kind content"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the copy to have the same id, or the source to change.",
				Right: "Duplicate creates a NEW document with a new id. The source is untouched; read .data.id for the copy."},
			{Wrong: "Assuming the copy is live.",
				Right: "On a drafts-enabled collection it is created as a draft — deliberately, so an unreviewed copy is not served. Use --no-draft, or publish it later."},
			{Wrong: "Using --no-draft on a document that cannot pass validation.",
				Right: "It fails the same way `pay publish` would. Duplicate as a draft, fix the fields, then publish."},
			{Wrong: "Duplicating a unique field (a slug) and expecting a clean copy.",
				Right: "Payload copies the field verbatim and may 400 on a unique constraint. Follow the duplicate with `pay update <coll> <newId> --set slug=…`."},
		},
		SeeAlso: []string{
			"pay create <collection>   # a document from scratch",
			"pay publish <collection> <id>",
			"pay collections --kind content   # which collections offer duplicate",
		},
	}
}
