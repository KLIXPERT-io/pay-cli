package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newRestoreCmd builds `pay restore <collection> <id>` — the inverse of a soft
// delete: PATCH ?trash=true {"<trashField>": null} (§12.4).
func init() { Register(newRestoreCmd) }

func newRestoreCmd(rt *Runtime) *cobra.Command {
	var (
		depth   int
		selectF []string
		locale  string
	)
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "restore <collection> <id>",
		Short:   "Un-trash a soft-deleted document",
		Long: `Restore a soft-deleted document by clearing its trash timestamp.

This is the inverse of ` + "`pay delete <id>`" + ` on a trash-enabled collection. It
cannot bring back a document removed with --permanent: that delete is real.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = runData(rt, safety.CmdRestore, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
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
			if err := checkTrash(t.Slug, t.flags().Trash); err != nil {
				return nil, err
			}

			field := trashField(t.Shard)
			body := map[string]any{field: nil}
			p := query.Params{Depth: query.IntPtr(depth), Select: selectF, Trash: query.BoolPtr(true)}
			// §7.9a, same as update: restore echoes the document it un-deleted.
			if warn, err := applyWriteLocale(d, t, locale, &p); err != nil {
				return nil, err
			} else if warn != nil {
				d.RT.Warn(*warn)
			}
			if err := query.ValidateSelect(selectF, t.schema(cfg)); err != nil {
				return nil, err
			}

			op := safety.Op{Command: safety.CmdRestore, Selector: safety.SelectorID, TrashEnabled: true}
			w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug+"/"+id).withIDs([]any{id})

			if cfg.DryRun {
				q, _ := p.Encode()
				return emitDryRun(d, w, safety.CmdRestore, "PATCH",
					client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/" + id, Query: q}),
					body, 1, []any{id})
			}
			if err := w.confirm(1); err != nil {
				return nil, err
			}
			if err := w.pre(1); err != nil {
				return nil, err
			}

			res, err := client.Update(ctx, t.Slug, id, body, p,
				payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body))), payload.WithKnownRoute())
			if err != nil {
				w.post(false, statusOf(res), 0, 1, err.Error(), "")
				return nil, err
			}
			env := output.New(safety.CmdRestore, output.KindDoc, res.Doc).
				WithTarget(t.envTarget(res.Doc.ID())).
				WithChanged(&output.Changed{Updated: 1, IDs: []any{res.Doc.ID()}}).
				WithMeta(withLocaleMeta(w.run.meta(), p)).
				WithRawBody(res.Raw, !cfg.Redact).
				WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
				})
			return w.finish(env, res.HTTP, 1, 0, "")
		}
	})
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth of the echoed document")
	cmd.Flags().StringSliceVar(&selectF, "select", nil, "return only these fields")
	cmd.Flags().StringVar(&locale, "locale", "", "locale to operate in")
	SetHelp(cmd, restoreHelp())
	return cmd
}

// restoreHelp is §10.5's model for `pay restore`.
func restoreHelp() *Help {
	return &Help{
		Synopsis:    []string{"pay restore <collection> <id> [--depth N] [--select a,b] [--locale CODE]"},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "crm-contacts"},
			{Name: "id", Required: true, Type: "string", Example: "224"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":  {Min: intPtr(0), Max: intPtr(10)},
			"select": {Grammar: "field,field.sub", Repeatable: true},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":224,"deletedAt":null,"restored":true}`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "see the request before sending it",
				Cmd: "pay restore crm-contacts 174 --dry-run"},
			{Why: "un-trash a soft-deleted document",
				Cmd: "pay restore crm-contacts 174"},
			{Why: "find what there is to restore in the first place",
				Cmd: "pay find crm-contacts --trash --where 'deletedAt exists' --select id,name,deletedAt"},
			{Why: "restore and get the full document back, relationships embedded",
				Cmd: "pay restore crm-contacts 174 --depth 1"},
			{Why: "restore and keep the response small",
				Cmd: "pay restore crm-contacts 174 --select id,name,deletedAt"},
		},
		Mistakes: []Mistake{
			{Wrong: "Calling restore on a collection without trash (`pay restore pages 16`).",
				Right: "It fails with feature_unavailable (exit 10) — pages has no trash, so its deletes were permanent. `pay collections` lists `trash` under features."},
			{Wrong: "Expecting restore to undo `pay delete --permanent`.",
				Right: "That DELETE is real. Recover from `pay versions list <collection> --id <id>` if the collection has versions, or from a backup."},
			{Wrong: "Looking for the document with a normal find and concluding it is gone.",
				Right: "A soft-deleted document is excluded by default. Pass --trash to see it: `pay find <collection> --trash --where 'id eq <id>'`."},
			{Wrong: "Restoring a document that was never trashed.",
				Right: "It succeeds and does nothing but bump updatedAt. Check deletedAt first, or use --dry-run."},
		},
		SeeAlso: []string{
			"pay delete <collection> <id>   # the soft delete this undoes",
			"pay find <collection> --trash   # what is currently trashed",
			"pay versions list <collection> --id <id>",
		},
	}
}
