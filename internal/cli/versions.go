package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// versionScope is the resolved target of a versions sub-command: either a
// collection or a global, never both.
type versionScope struct {
	target payload.VersionTarget
	coll   *collTarget
	global *globalTarget
}

func (s versionScope) slug() string { return s.target.Slug() }

func (s versionScope) envTarget() *output.Target {
	if s.global != nil {
		return s.global.envTarget()
	}
	return s.coll.envTarget(nil)
}

// resolveVersionScope applies the `<collection> | --global SLUG` grammar and
// pre-empts §11.3's second opaque-500 cause: /versions on a collection that has
// no versions enabled.
func resolveVersionScope(ctx context.Context, d *Deps, args []string, globalSlug string) (versionScope, error) {
	switch {
	case globalSlug != "" && len(args) > 0:
		return versionScope{}, apierr.New(apierr.CodeInvalidArgs,
			"give a collection slug or --global SLUG, not both")
	case globalSlug != "":
		g, err := d.global(ctx, globalSlug)
		if err != nil {
			return versionScope{}, err
		}
		if e := apierr.CheckVersions(g.Slug, g.flags().Versions); e != nil {
			return versionScope{}, e
		}
		return versionScope{target: payload.GlobalTarget(g.Slug), global: g}, nil
	case len(args) > 0:
		t, err := d.collection(ctx, args[0])
		if err != nil {
			return versionScope{}, err
		}
		if e := apierr.CheckVersions(t.Slug, t.flags().Versions); e != nil {
			return versionScope{}, e
		}
		return versionScope{target: payload.CollectionTarget(t.Slug), coll: t}, nil
	default:
		return versionScope{}, apierr.New(apierr.CodeInvalidArgs,
			"a collection slug or --global SLUG is required").
			WithHint("pay versions list pages, or pay versions list --global header")
	}
}

// ---------------------------------------------------------------------------
// sort validation on the versions route (§9.3)
// ---------------------------------------------------------------------------

// versionFieldPrefix is how a version record exposes the document it wraps. A
// version row is {id, parent, createdAt, updatedAt, autosave, latest,
// publishedLocale, version:{…the document…}}, so `--sort title` sorts on
// nothing at all while `--sort -version.publishedAt` really sorts — and Payload
// answers the first with HTTP 200 and unsorted rows. That is the same silent
// wrong answer `pay find` already rejects locally, and the prefix is documented
// nowhere, so the rejection has to teach it.
const versionFieldPrefix = "version."

// versionRecordSortable is the version ROW's own sortable column set. It is
// enumerated rather than wildcarded so that `-version.bogusfield` is rejected
// just as firmly as `-bogusfield`.
var versionRecordSortable = []string{
	"id", "parent", "createdAt", "updatedAt", "autosave", "latest", "publishedLocale",
}

const versionSortHint = "a version record sorts on id/parent/createdAt/updatedAt/autosave/latest/publishedLocale; " +
	"the document's own fields need the version. prefix, e.g. --sort -version.publishedAt. " +
	"Payload answers an unknown sort field with HTTP 200 and no sorting at all, so PayCLI " +
	"rejects it here; --no-validate-sort sends it anyway"

// versionDocSortable returns the wrapped document's sortable paths, or nil when
// nothing was learned — §9.3's tri-state, where unknown means "send it".
func versionDocSortable(scope versionScope) []string {
	switch {
	case scope.coll != nil:
		return scope.coll.Shard.SortablePaths()
	case scope.global != nil:
		return scope.global.Shard.SortablePaths()
	}
	return nil
}

// validateVersionSort applies find's sort check to the versions route, against
// a version-record schema rather than the collection's own.
func validateVersionSort(scope versionScope, fields []string) error {
	docPaths := versionDocSortable(scope)
	if docPaths == nil {
		return nil
	}
	candidates := append([]string(nil), versionRecordSortable...)
	for _, p := range docPaths {
		candidates = append(candidates, versionFieldPrefix+p)
	}
	for _, raw := range fields {
		name := strings.TrimPrefix(strings.TrimSpace(raw), "-")
		if name == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(name, versionFieldPrefix); ok {
			// The prefix is right, so the tail is judged exactly as `pay find`
			// judges a sort field on this collection.
			if rest != "" && query.ValidateSort([]string{rest}, query.Schema{Sortable: docPaths}) == nil {
				continue
			}
			return versionSortError(scope, name, candidates)
		}
		if slices.Contains(versionRecordSortable, name) {
			continue
		}
		return versionSortError(scope, name, candidates)
	}
	return nil
}

func versionSortError(scope versionScope, name string, candidates []string) error {
	return apierr.New(apierr.CodeInvalidSortField,
		"%q is not a sortable field on %s versions", name, scope.slug()).
		WithDidYouMean(apierr.DidYouMean(name, candidates)...).
		WithHint("%s", versionSortHint)
}

// newVersionsCmd builds the `pay versions` sub-tree (§9.2).
func init() { Register(newVersionsCmd) }

func newVersionsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "versions",
		Short:   "Inspect, compare and restore document versions",
		Long: `Version history for a collection or a global.

Versions exist only where the collection or global enables them. PayCLI checks
the manifest first, because /versions on a collection without versions answers
an opaque HTTP 500.`,
	}
	cmd.AddCommand(newVersionsListCmd(rt), newVersionsGetCmd(rt),
		newVersionsRestoreCmd(rt), newVersionsDiffCmd(rt))

	SetHelp(cmd, versionsHelp())
	return cmd
}

// versionsHelp is §10.5's model for the `pay versions` group itself.
func versionsHelp() *Help {
	return &Help{
		Synopsis: []string{"pay versions <list|get|diff|restore> [collection|--global SLUG] …"},
		Discovered: func(rt *Runtime, m *discovery.Manifest) []string {
			var colls, globals []string
			for _, c := range m.Collections {
				if c.Flags.Versions != nil && *c.Flags.Versions {
					colls = append(colls, c.Slug)
				}
			}
			for _, g := range m.Globals {
				if g.Flags.Versions != nil && *g.Flags.Versions {
					globals = append(globals, g.Slug)
				}
			}
			lines := []string{fmt.Sprintf("collections with versions (%d):", len(colls))}
			lines = append(lines, wrapList("  ", colls, 84)...)
			lines = append(lines, fmt.Sprintf("globals with versions (%d):", len(globals)))
			return append(lines, wrapList("  ", globals, 84)...)
		},
		Output:    OutputSpec{Kind: output.KindVersionList, Paginated: true},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "one document's history", Cmd: "pay versions list pages --id 16"},
			{Why: "the newest version of anything in the collection", Cmd: "pay versions list pages --latest"},
			{Why: "read one version record in full", Cmd: "pay versions get pages 20"},
			{Why: "what changed between two versions", Cmd: "pay versions diff pages 20 37"},
			{Why: "put a version back (L2: preview first)", Cmd: "pay versions restore pages 20 --dry-run"},
			{Why: "globals have versions too", Cmd: "pay versions list --global crm-brand --limit 2"},
		},
		Mistakes: []Mistake{
			{Wrong: "Running `pay versions` against a collection that has no versions.",
				Right: "Payload answers /versions with an opaque HTTP 500 there; PayCLI checks the manifest first and fails with exit 10. The list above is this project's versioned entities."},
			{Wrong: "Passing the DOCUMENT id where a version id is expected.",
				Right: "`versions get|diff|restore` take the VERSION record's id. Get it from `pay versions list <collection> --id <docId>`; the record's `parent` is the document id."},
			{Wrong: "Expecting a --where on the versions route.",
				Right: "There is none. Narrow with --id/--autosave/--latest, or drop to `pay raw GET <collection>/versions --query 'where[…]'`."},
			{Wrong: "Reading a version record as if it were the document.",
				Right: "It is {id, parent, createdAt, updatedAt, autosave, latest, version:{…the document…}}. The document lives under .version."},
		},
		SeeAlso: []string{"pay versions list --help", "pay versions restore --help",
			"pay collections   # `versions` appears under features"},
	}
}

func newVersionsListCmd(rt *Runtime) *cobra.Command {
	var (
		globalSlug     string
		docID          string
		autosave       bool
		latest         bool
		limit          int
		page           int
		sortFields     []string
		noValidateSort bool
	)
	cmd := &cobra.Command{
		Use:   "list [collection]",
		Short: "List the versions of a document or global",
		Long: `List version records.

--id narrows the list to one document's history (the version record's parent).
--latest is shorthand for --limit 1 --sort -updatedAt.

A version record is NOT the document: it is {id, parent, createdAt, updatedAt,
autosave, latest, publishedLocale, version:{…the document…}}. Sort accordingly.

There is no --where on this command. To filter version records, use
pay raw GET pages/versions --query 'where[version._status][equals]=published'`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdVersionsList, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			scope, err := resolveVersionScope(ctx, d, args, globalSlug)
			if err != nil {
				return nil, err
			}

			p := query.Params{Sort: sortFields, Page: page}
			if latest {
				limit = 1
				if len(sortFields) == 0 {
					p.Sort = []string{"-updatedAt"}
				}
			}
			if !noValidateSort {
				if err := validateVersionSort(scope, p.Sort); err != nil {
					return nil, err
				}
			}
			if err := query.ValidateLimit(&limit); err != nil {
				return nil, err
			}
			p.Limit = query.IntPtr(limit)

			var terms []query.Where
			if docID != "" {
				if scope.coll != nil {
					if e := d.checkID(scope.coll, scope.slug(), docID); e != nil {
						return nil, e
					}
				}
				id, err := query.TypedID(docID, idTypeOf(scope))
				if err != nil {
					return nil, err
				}
				terms = append(terms, query.Term("parent", query.OpEquals, id))
			}
			if autosave {
				terms = append(terms, query.Term("autosave", query.OpEquals, true))
			}
			if len(terms) > 0 {
				p.Where = query.Combine(terms, nil)
			}

			run := d.beginRun()
			list, hasDocs, err := client.VersionsList(ctx, scope.target, p, payload.WithKnownRoute())
			if err != nil {
				return nil, err
			}
			if !hasDocs {
				return nil, apierr.New(apierr.CodeVersionNotFound,
					"%q answered the versions route without a docs[] array", scope.slug()).
					WithHint("a custom endpoint is shadowing /versions on this collection; `pay raw GET /%s/versions` shows the real body", scope.slug())
			}
			docs := list.Docs
			if docs == nil {
				docs = []payload.Doc{}
			}
			env := output.New(safety.CmdVersionsList, output.KindVersionList, docs).
				WithTarget(scope.envTarget()).
				WithPage(envPage(list.Page, len(docs), list.Page.HasNextPage)).
				WithMeta(run.meta()).
				WithRawBody(list.Raw, !cfg.Redact)
			return env, nil
		}
	})
	cmd.Flags().StringVar(&globalSlug, "global", "", "target this global instead of a collection")
	cmd.Flags().StringVar(&docID, "id", "", "only versions of this document")
	cmd.Flags().BoolVar(&autosave, "autosave", false, "only autosave versions")
	cmd.Flags().BoolVar(&latest, "latest", false, "only the newest version")
	cmd.Flags().IntVar(&limit, "limit", 20, "page size")
	cmd.Flags().IntVar(&page, "page", 0, "page number (1-based)")
	cmd.Flags().StringSliceVar(&sortFields, "sort", nil, "sort field(s); prefix with - for descending (document fields need the version. prefix)")
	cmd.Flags().BoolVar(&noValidateSort, "no-validate-sort", false, "skip client-side sort-field validation")

	SetHelp(cmd, versionsListHelp())
	return cmd
}

// versionsListHelp is §10.5's model for `pay versions list`.
func versionsListHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay versions list <collection> [--id DOCID] [--latest] [--autosave] [--sort FIELD] [--limit N]",
			"pay versions list --global <slug> [--latest] [--limit N]",
		},
		Sort: true,
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages  (omit it when you pass --global)"},
		},
		FlagInfo: map[string]FlagInfo{
			"id":     {Note: "the DOCUMENT id; it filters on the version record's `parent`"},
			"sort":   {Grammar: "[-]FIELD — document fields need the version. prefix", Repeatable: true},
			"latest": {Note: "shorthand for --limit 1 --sort -updatedAt"},
			"limit":  {Min: intPtr(1)},
			"global": {Note: "target a global instead of a collection; the positional slug is then omitted"},
		},
		Output: OutputSpec{
			Kind: output.KindVersionList, Paginated: true,
			Skeleton: `[{"id":20,"parent":16,"latest":false,"autosave":false,"createdAt":"…","version":{…the document…}}]`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "the history of one document", Cmd: "pay versions list pages --id 16"},
			{Why: "the newest version in the collection", Cmd: "pay versions list pages --latest"},
			{Why: "sort on a field of the wrapped document — note the version. prefix",
				Cmd: "pay versions list pages --sort -version.publishedAt --limit 3"},
			{Why: "newest first across the whole collection",
				Cmd: "pay versions list pages --sort -updatedAt --limit 5"},
			{Why: "a global's history", Cmd: "pay versions list --global crm-brand --limit 2"},
			{Why: "just the version ids, ready to feed to `versions get`",
				Cmd: "pay versions list pages --id 16 --path '[].id'"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--sort -publishedAt`.",
				Right: "publishedAt lives INSIDE the wrapped document, so it is --sort -version.publishedAt. Payload answers an unknown sort field with HTTP 200 and unsorted rows, so PayCLI rejects it (exit 5) instead."},
			{Wrong: "Passing --where to filter version records.",
				Right: "This command has no --where. Use --id/--autosave/--latest, or `pay raw GET <collection>/versions --query 'where[version._status][equals]=published'`."},
			{Wrong: "Using the returned `id` as the document id.",
				Right: "`id` is the VERSION record; `parent` is the document. Feed `id` to `pay versions get|diff|restore`."},
			{Wrong: "Expecting --id to accept a version id.",
				Right: "--id is the DOCUMENT id and is validated against the collection's id_type. To read one record use `pay versions get <collection> <versionId>`."},
		},
		SeeAlso: []string{"pay versions get <collection> <versionId>",
			"pay versions diff <collection> <a> <b>", "pay versions restore <collection> <versionId>"},
	}
}

func idTypeOf(scope versionScope) string {
	if scope.coll != nil {
		return scope.coll.idType()
	}
	return ""
}

func newVersionsGetCmd(rt *Runtime) *cobra.Command {
	var (
		globalSlug string
		depth      int
		rich       richTextFlag
	)
	cmd := &cobra.Command{
		Use:   "get [collection] <versionId>",
		Short: "Read one version record",
		Long: `Read a single version by its version id.

The version id is the id of the VERSION record, not of the document. Get it from
` + "`pay versions list`" + `.

`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdVersionsGet, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			if err := rich.validate(); err != nil {
				return nil, err
			}
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			scopeArgs, versionID, err := splitVersionArgs(args, globalSlug)
			if err != nil {
				return nil, err
			}
			scope, err := resolveVersionScope(ctx, d, scopeArgs, globalSlug)
			if err != nil {
				return nil, err
			}

			run := d.beginRun()
			doc, resp, err := client.VersionGet(ctx, scope.target, versionID,
				query.Params{Depth: query.IntPtr(depth)}, payload.WithKnownRoute())
			if err != nil {
				return nil, err
			}
			var raw []byte
			if resp != nil {
				raw = resp.Body
			}
			env := output.New(safety.CmdVersionsGet, output.KindVersion, doc).
				WithTarget(scope.envTarget()).
				WithMeta(run.meta()).
				WithRawBody(raw, !cfg.Redact)
			rich.apply(env)
			return env, nil
		}
	})
	cmd.Flags().StringVar(&globalSlug, "global", "", "target this global instead of a collection")
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth")
	rich.register(cmd)

	SetHelp(cmd, versionsGetHelp())
	return cmd
}

// versionsGetHelp is §10.5's model for `pay versions get`.
func versionsGetHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay versions get <collection> <versionId> [--depth N]",
			"pay versions get --global <slug> <versionId> [--depth N]",
		},
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages  (omit it when you pass --global)"},
			{Name: "versionId", Required: true, Type: "string", Example: "20"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"global":   {Note: "target a global instead of a collection"},
			"richtext": richTextFlagInfo,
		},
		Output: OutputSpec{
			Kind:     output.KindVersion,
			Skeleton: `{"id":20,"parent":16,"latest":false,"autosave":false,"version":{…the document…}}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "find a version id first — `id` is the record, `parent` is the document",
				Cmd: "pay versions list pages --id 16 --limit 3"},
			{Why: "read that version record", Cmd: "pay versions get pages 20"},
			{Why: "the document as it was, with relationships embedded",
				Cmd: "pay versions get pages 20 --depth 1"},
			{Why: "one field as it was at that point",
				Cmd: "pay versions get pages 20 --path '.version.title'"},
			{Why: "a global's version", Cmd: "pay versions get --global crm-brand 11"},
			{Why: "how an old version's text read, as Markdown", Cmd: "pay versions get pages 20 --richtext md --path '.version.layout'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing the document id.",
				Right: "This takes the VERSION record's id, from `pay versions list <collection> --id <docId>`. A document id usually resolves to someone else's version or to exit 4."},
			{Wrong: "Reading .data.title.",
				Right: "The document is nested: .data.version.title. .data.id is the version record, .data.parent is the document."},
			{Wrong: "Expecting a version to reflect later edits.",
				Right: "A version is a snapshot. `pay get <collection> <id>` is the current document; `pay versions diff` compares two snapshots."},
			{Wrong: "Using it on a collection without versions.",
				Right: "It fails with exit 10 before any request. `pay collections` lists `versions` under features."},
		},
		SeeAlso: []string{"pay versions list <collection> --id <docId>",
			"pay versions diff <collection> <a> <b>", "pay get <collection> <id>"},
	}
}

func newVersionsRestoreCmd(rt *Runtime) *cobra.Command {
	var (
		globalSlug string
		draft      bool
		unpublish  bool
		depth      int
	)
	cmd := &cobra.Command{
		Use:   "restore [collection] <versionId>",
		Short: "Restore a document or global to one of its versions",
		Long: `Restore a previous version.

This overwrites the CURRENT document with the version's content. It is an L2
operation: it prompts on a TTY and requires --yes in a non-TTY. The overwritten
content becomes a new version of its own, so the operation is itself reversible
on a versioned collection.

`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdVersionsRestore, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			scopeArgs, versionID, err := splitVersionArgs(args, globalSlug)
			if err != nil {
				return nil, err
			}
			scope, err := resolveVersionScope(ctx, d, scopeArgs, globalSlug)
			if err != nil {
				return nil, err
			}
			if err := checkStatusFlags(draft, false, unpublish); err != nil {
				return nil, err
			}
			if unpublish && scope.global != nil {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"--unpublish applies to a collection document; a global's restore always rewrites the stored global").
					WithHint("drop --unpublish")
			}
			// The version record is read at most once: the guard below and
			// the backup (its parent document id) both need it.
			var version payload.Doc
			readVersion := func() (payload.Doc, error) {
				if version != nil {
					return version, nil
				}
				v, _, err := client.VersionGet(ctx, scope.target, versionID, query.Params{Depth: query.IntPtr(0)},
					payload.WithKnownRoute())
				if err == nil {
					version = v
				}
				return v, err
			}
			// §10.2.3: a draft version restored without --draft unpublishes
			// a published document — refused unless --unpublish says so.
			if err := d.checkVersionRestore(ctx, client, scope, versionID, draft, unpublish, readVersion); err != nil {
				return nil, err
			}

			p := query.Params{Depth: query.IntPtr(depth)}
			if draft {
				p.Draft = query.BoolPtr(true)
			}
			op := safety.Op{
				Command:  safety.CmdVersionsRestore,
				Selector: safety.SelectorID,
				Global:   scope.global != nil,
			}
			path := "/" + scope.slug() + "/versions/" + versionID
			if scope.global != nil {
				path = payload.GlobalPath(scope.slug()) + "/versions/" + versionID
			}
			w := d.newWriteOp(op, scope.slug(), "POST", path).withIDs([]any{versionID})
			w.global = scope.global != nil
			// §12.8: back up the document the version is about to overwrite.
			// The ids on this writeOp are VERSION ids, so the document's own
			// id is looked up from the version record — only when backups are
			// on, so nobody else pays for the read.
			if bt, err := versionRestoreBackupTarget(d, scope, versionID, p, readVersion); err != nil {
				return nil, err
			} else if bt != nil {
				w = w.withBackup(ctx, *bt)
			}

			if cfg.DryRun {
				// F7 stage B / §10.2.1: what the restore changes, and the
				// nested rows it would leave behind.
				diff, dw := d.versionRestorePreview(ctx, client, scope, versionID, p)
				if diff != nil {
					w.dryDiff = diff
				}
				q, _ := p.Encode()
				return emitDryRun(d, w, safety.CmdVersionsRestore, "POST",
					client.URLFor(&payload.Request{Method: "POST", Path: path, Query: q}), nil, 1, []any{versionID}, dw...)
			}
			if err := w.confirm(1); err != nil {
				return nil, err
			}
			if err := w.pre(1); err != nil {
				return nil, err
			}

			res, err := client.VersionRestore(ctx, scope.target, versionID, p, payload.WithKnownRoute())
			if err != nil {
				w.post(false, statusOf(res), 0, 1, err.Error(), "")
				return nil, err
			}
			kind := output.KindDoc
			if scope.global != nil {
				kind = output.KindGlobal
			}
			env := output.New(safety.CmdVersionsRestore, kind, res.Doc).
				WithTarget(scope.envTarget()).
				WithChanged(&output.Changed{Updated: 1, IDs: []any{res.Doc.ID()}}).
				WithMeta(w.run.meta()).
				WithRawBody(res.Raw, !cfg.Redact)
			if scope.global != nil {
				env.WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    fmt.Sprintf("pay globals get %s --depth 0%s", scope.slug(), d.profileFlag()),
				})
			} else {
				env.WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", scope.slug(), res.Doc.IDString(), d.profileFlag()),
				})
			}
			// §10.2.1: restoring a version with a different nested structure
			// is a restructure like any other; the stored row is read back.
			o := orphanTarget{t: scope.coll, g: scope.global, id: res.Doc.IDString()}
			if scope.global != nil || (scope.coll != nil && o.id != "") {
				warns, _ := d.orphanCheckAfterWrite(ctx, client, o, nil, res.Doc, p, false, false)
				for _, warn := range warns {
					env.AddWarning(warn)
				}
			}
			return w.finish(env, res.HTTP, 1, 0, "")
		}
	})
	cmd.Flags().StringVar(&globalSlug, "global", "", "target this global instead of a collection")
	cmd.Flags().BoolVar(&draft, "draft", false, "restore as a new draft on top; without it the version's own _status is written to the live document")
	cmd.Flags().BoolVar(&unpublish, "unpublish", false, "allow restoring a DRAFT version over a published document, taking it offline")
	cmd.Flags().IntVar(&depth, "depth", 0, "relationship expansion depth of the echoed document")

	SetHelp(cmd, versionsRestoreHelp())
	return cmd
}

// versionsRestoreHelp is §10.5's model for `pay versions restore`.
func versionsRestoreHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay versions restore <collection> <versionId> [--draft|--unpublish] [--depth N] --yes",
			"pay versions restore --global <slug> <versionId> --yes",
		},
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages  (omit it when you pass --global)"},
			{Name: "versionId", Required: true, Type: "string", Example: "20"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth": {Min: intPtr(0), Max: intPtr(10)},
			"draft": {Note: "restore as a new draft on top; the live document stays as it is. Without it the version's own " +
				"_status is written to the live row, so a draft version would unpublish a published document (refused unless --unpublish)"},
			"unpublish": {Note: "restore a draft version into the live row of a published document, taking it offline deliberately"},
			"global":    {Note: "target a global instead of a collection"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":16,"title":"…as it was at version 20…","_status":"draft"}`,
		},
		ExitCodes: DestructiveExitCodes,
		Examples: []Example{
			{Why: "pick the version to go back to",
				Cmd: "pay versions list pages --id 16 --limit 5"},
			{Why: "check what is actually different before overwriting anything",
				Cmd: "pay versions diff pages 20 37"},
			{Why: "always preview: this OVERWRITES the current document",
				Cmd: "pay versions restore pages 20 --dry-run"},
			{Why: "run it; --yes is required in a non-TTY (else exit 11)",
				Cmd: "pay versions restore pages 20 --yes"},
			{Why: "restore as a new draft on top; the live page stays as it is",
				Cmd: "pay versions restore pages 20 --draft --yes"},
			{Why: "a global", Cmd: "pay versions restore --global crm-brand 11 --dry-run"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating exit 11 as a failure.",
				Right: "11 is \"confirmation required\": this overwrites the live document. Re-run with --yes once --dry-run showed you the request."},
			{Wrong: "Believing the restore is unrecoverable.",
				Right: "On a versioned collection the overwritten content becomes a version of its own, so `pay versions list` still has it."},
			{Wrong: "Passing the document id instead of the version id.",
				Right: "Use the `id` from `pay versions list <collection> --id <docId>`; `parent` there is the document id."},
			{Wrong: "Restoring a draft version of a published page without --draft.",
				Right: "Payload writes the version's _status \"draft\" into the live row and the page goes offline, so PayCLI refuses it. --draft restores it as a new draft on top; --unpublish takes the page offline deliberately."},
			{Wrong: "Restoring without diffing first.",
				Right: "`pay versions diff <collection> <old> <new>` lists every change (rows by id, rich text as text), client-side, before you overwrite anything."},
		},
		SeeAlso: []string{"pay versions diff <collection> <a> <b>",
			"pay versions list <collection> --id <docId>", "pay update <collection> <id>"},
	}
}

func newVersionsDiffCmd(rt *Runtime) *cobra.Command {
	var globalSlug string
	var ignore []string
	var includeTimestamps, full bool
	cmd := &cobra.Command{
		Use:   "diff [collection] <versionA> <versionB>",
		Short: "Compare two versions field by field",
		Long: `Compare two version records CLIENT-SIDE and report what changed.

Payload has no diff endpoint, so this fetches both versions and compares their
"version" sub-documents with the same structural diff as ` + "`pay diff`" + `: rows are
matched by id (a reorder is reported as moves, an edit inside a moved row at its
new path), a block whose type changed is one row change, and rich text is
reported once per field as plain text. createdAt/updatedAt (at every depth) and
the version record's own id are excluded.

`,
		Args: cobra.RangeArgs(2, 3),
	}
	cmd.RunE = runData(rt, safety.CmdVersionsDiff, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			var scopeArgs []string
			var vA, vB string
			switch len(args) {
			case 3:
				scopeArgs, vA, vB = args[:1], args[1], args[2]
			default:
				if globalSlug == "" {
					return nil, apierr.New(apierr.CodeInvalidArgs,
						"give a collection slug or --global SLUG").
						WithHint("pay versions diff pages 412 398")
				}
				vA, vB = args[0], args[1]
			}
			scope, err := resolveVersionScope(ctx, d, scopeArgs, globalSlug)
			if err != nil {
				return nil, err
			}

			run := d.beginRun()
			docA, _, err := client.VersionGet(ctx, scope.target, vA, query.Params{Depth: query.IntPtr(0)}, payload.WithKnownRoute())
			if err != nil {
				return nil, err
			}
			docB, _, err := client.VersionGet(ctx, scope.target, vB, query.Params{Depth: query.IntPtr(0)}, payload.WithKnownRoute())
			if err != nil {
				return nil, err
			}

			opts := versionDiffOptions(ignore, includeTimestamps, full)
			res := docdiff.Diff(versionBody(docA), versionBody(docB), opts)
			var warns []output.Warning
			if d.cfg().Redact {
				if paths := redactChanges(res.Changes); len(paths) > 0 {
					warns = append(warns, output.DataRedactedWarning(paths))
				}
			}
			var data any = versionsDiffData(vA, vB, res, full)
			if d.RT != nil && d.RT.Out != nil && d.RT.Out.Format == output.FormatTable && d.cfg().Path == "" {
				// --output table is for a person reading a change list, exactly
				// like `pay diff`; every other format gets the object.
				data = diffTableRows(res)
				d.RT.Out.Columns = []string{"op", "path", "from", "to"}
			}
			env := output.New(safety.CmdVersionsDiff, output.KindOpResult, data).
				WithTarget(scope.envTarget()).
				WithMeta(run.meta())
			for _, w := range warns {
				env.AddWarning(w)
			}
			return env, nil
		}
	})
	cmd.Flags().StringVar(&globalSlug, "global", "", "target this global instead of a collection")
	cmd.Flags().StringArrayVar(&ignore, "ignore", nil, "ignore a path and everything below it (repeatable; `[]` matches any index: layout[].blockName)")
	cmd.Flags().BoolVar(&includeTimestamps, "include-timestamps", false, "also compare createdAt/updatedAt")
	cmd.Flags().BoolVar(&full, "full", false, "keep rich-text editor states and nested values lossless instead of abbreviating them to text")

	SetHelp(cmd, versionsDiffHelp())
	return cmd
}

// versionsDiffHelp is §10.5's model for `pay versions diff`.
func versionsDiffHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay versions diff <collection> <versionA> <versionB>",
			"pay versions diff --global <slug> <versionA> <versionB>",
		},
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages  (omit it when you pass --global)"},
			{Name: "versionA", Required: true, Type: "string", Example: "20"},
			{Name: "versionB", Required: true, Type: "string", Example: "37"},
		},
		FlagInfo: map[string]FlagInfo{
			"global":             {Note: "target a global instead of a collection"},
			"ignore":             {Grammar: "PATH", Repeatable: true, Note: "a path and its subtree; `[]` or `[*]` matches any index (layout[].blockName), `[3]` one index"},
			"include-timestamps": {Note: "createdAt/updatedAt are ignored at every depth unless this is set"},
			"full":               {Note: "rich-text changes carry the editor states in a/b; values nested in added/removed rows are not abbreviated"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"a":"20","b":"37","identical":false,"changed":3,
 "summary":{"total":3,"added":0,"removed":1,"changed":1,"moved":1,"rich_text":1,"fields":["layout","slug"]},
 "changes":[{"path":"slug","kind":"changed","a":"old","b":"new","op":"change"},
            {"path":"layout[1]","kind":"moved","a":null,"b":null,"op":"move","value_kind":"row","id":"6ab8…","block_type":"cta","from_index":3,"to_index":1},
            {"path":"layout[2].content","kind":"changed","a":"Alter Text","b":"Neuer Text","op":"change","value_kind":"richtext","a_text":"Alter Text","b_text":"Neuer Text"}]}
--output table prints one line per change: OP PATH FROM TO.`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "list the versions you want to compare",
				Cmd: "pay versions list pages --id 16 --limit 5"},
			{Why: "compare two of them",
				Cmd: "pay versions diff pages 20 37"},
			{Why: "the changed paths only",
				Cmd: "pay versions diff pages 20 37 --path '.changes[].path'"},
			{Why: "how many changes at all",
				Cmd: "pay versions diff pages 20 37 --path '.changed'"},
			{Why: "which top-level fields a restore would rewrite",
				Cmd: "pay versions diff pages 20 37 --path '.summary.fields'"},
			{Why: "a readable change list",
				Cmd: "pay versions diff pages 20 37 --output table"},
			{Why: "a global", Cmd: "pay versions diff --global crm-brand 11 11"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting Payload to compute the diff.",
				Right: "There is no diff endpoint. This fetches BOTH versions (two requests) and compares them client-side."},
			{Wrong: "Reading `changed: 0` as \"the versions are byte-identical\".",
				Right: "updatedAt, createdAt (at every depth) and the version record's own id are excluded as volatile bookkeeping, and null equals an absent key. Two snapshots with no content change report `identical: true` deliberately; --include-timestamps compares the timestamps too."},
			{Wrong: "Expecting one change per changed leaf inside a rich-text field or a re-typed block.",
				Right: "Rich text is one change per field (a/b and a_text/b_text are its plain text; --full keeps the editor states), and a block whose blockType changed is one row change (value_kind: row). A reorder is reported as moves, not as every row after it changing."},
			{Wrong: "Passing document ids.",
				Right: "Both arguments are VERSION record ids from `pay versions list`."},
			{Wrong: "Diffing across two different documents and trusting the result.",
				Right: "Nothing stops you, but the paths are only meaningful when both versions share a `parent`. Check it in `pay versions list <collection> --id <docId>`."},
		},
		SeeAlso: []string{"pay versions list <collection> --id <docId>",
			"pay versions get <collection> <versionId>", "pay versions restore <collection> <versionId>"},
	}
}

// splitVersionArgs separates an optional collection slug from the version id.
func splitVersionArgs(args []string, globalSlug string) (scopeArgs []string, versionID string, err error) {
	switch {
	case len(args) == 2:
		if globalSlug != "" {
			return nil, "", apierr.New(apierr.CodeInvalidArgs,
				"give a collection slug or --global SLUG, not both")
		}
		return args[:1], args[1], nil
	case len(args) == 1 && globalSlug != "":
		return nil, args[0], nil
	case len(args) == 1:
		return nil, "", apierr.New(apierr.CodeInvalidArgs,
			"a collection slug is required before the version id").
			WithHint("pay versions get <collection> %s, or --global <slug> %s", args[0], args[0])
	default:
		return nil, "", apierr.New(apierr.CodeInvalidArgs, "a version id is required")
	}
}

// versionBody unwraps the version record's "version" sub-document.
func versionBody(doc payload.Doc) map[string]any {
	if doc == nil {
		return map[string]any{}
	}
	if inner, ok := doc["version"].(map[string]any); ok {
		return inner
	}
	return map[string]any(doc)
}

// versionDiffOptions are the docdiff options of `pay versions diff`. The
// version record's own id (and a Mongo _id) always differs between two
// records and is never news.
func versionDiffOptions(ignore []string, includeTimestamps, full bool) docdiff.Options {
	return docdiff.Options{
		IncludeTimestamps: includeTimestamps,
		IgnorePaths:       append([]string{"id", "_id"}, ignore...),
		Full:              full,
	}
}

// versionChange is one change in `pay versions diff`'s data.changes.
//
// The first four keys are the shape this command has always printed —
// {path, kind: added|removed|changed, a, b} — so a caller written against
// v0.1.0 keeps working; kind gained "moved". Everything after them is the
// docdiff change in this command's a/b vocabulary: op (add|remove|change|move,
// the same values `pay diff` prints), value_kind (row|richtext), the row's id
// and block_type, from/to_index for a move, a_text/b_text for rich text, and
// detail.
type versionChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	A    any    `json:"a"`
	B    any    `json:"b"`

	Op        docdiff.Op `json:"op"`
	ValueKind string     `json:"value_kind,omitempty"`
	ID        any        `json:"id,omitempty"`
	BlockType string     `json:"block_type,omitempty"`
	FromIndex *int       `json:"from_index,omitempty"`
	ToIndex   *int       `json:"to_index,omitempty"`
	AText     *string    `json:"a_text,omitempty"`
	BText     *string    `json:"b_text,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// legacyVersionKind maps a docdiff op to the word this command has always
// used in `kind`.
var legacyVersionKind = map[docdiff.Op]string{
	docdiff.OpAdd: "added", docdiff.OpRemove: "removed",
	docdiff.OpChange: "changed", docdiff.OpMove: "moved",
}

// versionChanges renders a docdiff result in this command's vocabulary. A
// rich-text change that docdiff abbreviated carries its plain text in a/b
// as well as in a_text/b_text, so a v0.1.0 caller reading a/b sees words,
// not two nulls.
func versionChanges(res docdiff.Result, full bool) []versionChange {
	out := make([]versionChange, 0, len(res.Changes))
	for _, c := range res.Changes {
		vc := versionChange{
			Path: c.Path, Kind: legacyVersionKind[c.Op], A: c.From, B: c.To,
			Op: c.Op, ValueKind: c.Kind, ID: c.ID, BlockType: c.BlockType,
			FromIndex: c.FromIndex, ToIndex: c.ToIndex,
			AText: c.FromText, BText: c.ToText, Detail: c.Detail,
		}
		if c.Kind == docdiff.KindRichText && !full {
			vc.A, vc.B = nil, nil
			if c.FromText != nil {
				vc.A = *c.FromText
			}
			if c.ToText != nil {
				vc.B = *c.ToText
			}
		}
		out = append(out, vc)
	}
	return out
}

// versionsDiffData is data for `pay versions diff`: the two version ids,
// identical, the docdiff summary, changes, and changed (their count).
func versionsDiffData(vA, vB string, res docdiff.Result, full bool) map[string]any {
	return map[string]any{
		"a":         vA,
		"b":         vB,
		"identical": res.Identical,
		"summary":   res.Summary,
		"changes":   versionChanges(res, full),
		"changed":   len(res.Changes),
	}
}
