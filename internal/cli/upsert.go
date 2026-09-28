package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// F6 — `pay upsert`
// ---------------------------------------------------------------------------
//
// "Find by slug → update, else create" was the script every migration agent
// wrote by hand. upsert is that script, with the plan engine's semantics:
// the match values come from the body; 0 matches → create, 1 → update with
// only the fields whose value changes, several → match_ambiguous (exit 5);
// nothing to change → no write at all (meta.upsert.action "unchanged").

// Upsert actions reported in meta.upsert.action after a real run.
const (
	upsertCreated   = "created"
	upsertUpdated   = "updated"
	upsertUnchanged = "unchanged"
)

type upsertFlags struct {
	data        writeData
	match       []string
	draft       bool
	publish     bool
	force       bool
	depth       int
	locale      string
	noEchoCheck bool
	ignore      []string
}

func init() { Register(newUpsertCmd) }

func newUpsertCmd(rt *Runtime) *cobra.Command {
	f := &upsertFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "upsert <collection>",
		Short:   "Create the document --match does not find, or update the one it does (idempotent)",
		Long: `Create or update one document, matched by the values of --match fields in the body.

  0 matches → create.  1 match → update, sending the top-level fields whose
  value would change (Payload PATCH semantics) plus the unchanged string,
  number and boolean fields the body declares, so a hook that fills a field
  the request leaves out (publishedAt) cannot overwrite it.  Several →
  match_ambiguous (exit 5).
  Nothing would change → nothing is written (meta.upsert.action "unchanged"),
  unless --force.

The match reads the newest draft (depth 0), so a document that exists only as a
draft is found, not duplicated. Publication state is set by --publish / --draft
only: a "_status" in the body is removed (warning body_status_ignored). With
neither flag the document keeps the state of its newest version: a published
document is saved published, a draft gets a new draft, a new document is
created as a draft, and a published document with a pending draft is saved
into that draft — the live version does not change (warning saved_as_draft;
--publish takes it live, together with the draft's other changes).

The body may name related documents instead of ids:
{"$ref":"pages","slug":"about"} (add "$as":"polymorphic" for {relationTo,value}).`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdUpsert, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runUpsert(ctx, d, f, args[0])
	})
	f.data.register(cmd)
	fl := cmd.Flags()
	fl.StringSliceVar(&f.match, "match", []string{manifest.DefaultMatch}, "field(s) whose values in the body identify the document")
	fl.BoolVar(&f.draft, "draft", false, "save the change as a draft (never unpublishes)")
	fl.BoolVar(&f.publish, "publish", false, "publish the result (_status published)")
	fl.BoolVar(&f.force, "force", false, "write the whole body even when nothing would change")
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.StringVar(&f.locale, "locale", "", "match, diff and write in this locale")
	fl.BoolVar(&f.noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")
	fl.StringArrayVar(&f.ignore, "ignore", nil, "path pattern the unchanged-check ignores (repeatable)")
	SetHelp(cmd, upsertHelp())
	return cmd
}

func upsertHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay upsert <collection> [--match FIELD[,FIELD…]] (--data-file F | --data JSON | --set k=v …)",
			"                        [--publish|--draft] [--force] [--dry-run]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"},
		},
		FlagInfo: map[string]FlagInfo{
			"match":    {Grammar: "field[,field…] — values are read from the body; default slug"},
			"set":      {Grammar: "key=value  (dotted keys nest: meta.title=…)", Repeatable: true},
			"set-json": {Grammar: "key=JSON", Repeatable: true},
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"ignore":   {Grammar: "a change path; [] matches any index", Repeatable: true},
		},
		Output: OutputSpec{
			Kind: output.KindDoc,
			Skeleton: `{"id":12,"title":"About","slug":"about",…}  meta.upsert: {"action":"created|updated|unchanged","matched_by":{"slug":"about"},"id":12}` +
				`  · --dry-run: op_result {"action":"update","matched_by":{…},"id":12,"diff":{…},"would_affect":1,"request":{…}}`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "what would happen, with the diff of an update", Cmd: "pay upsert pages --data-file about.json --dry-run"},
			{Why: "create or update by slug (the default --match)", Cmd: "pay upsert pages --data-file about.json"},
			{Why: "several match fields", Cmd: "pay upsert crm-contacts --match email,company --data-file contact.json"},
			{Why: "a relationship by slug instead of an id", Cmd: `pay upsert posts --set slug=launch --set title=Launch --set-json 'categories=[{"$ref":"categories","slug":"news"}]'`},
			{Why: "publish the result", Cmd: "pay upsert pages --data-file about.json --publish"},
			{Why: "rewrite even though nothing changed", Cmd: "pay upsert pages --data-file about.json --force"},
		},
		Mistakes: []Mistake{
			{Wrong: "\"_status\":\"published\" in the body (e.g. an edited `pay get` export).",
				Right: "It is removed (body_status_ignored): publishing is --publish, a draft is --draft, and without either the document keeps the state of its newest version (a published page with a pending draft is saved into that draft: saved_as_draft)."},
			{Wrong: "A body without the --match field (no slug in it).",
				Right: "The match values come from the body: include them, or choose --match fields the body has (invalid_args otherwise)."},
			{Wrong: "Treating match_ambiguous as retryable.",
				Right: "Several documents share those values: add a field to --match that makes it unique, or update the one you mean by id (error.did_you_mean)."},
			{Wrong: "Scripting find-then-create-or-update around `pay create`/`pay update`.",
				Right: "upsert is exactly that, and it writes nothing when nothing changed; for many documents use `pay sync <dir>`."},
		},
		SeeAlso: []string{
			"pay sync <DIR|FILE>...   # many documents and globals, dependency-ordered",
			"pay plan <DIR|FILE>...   # what a sync would do",
			"pay upsert <collection> --data-file F --dry-run   # the upsert's own diff (row ids adopted, equal dates equal)",
			"pay diff <collection> <id> --data-file F   # the body as a literal PATCH (no row-id adoption)",
		},
	}
}

// upsertDryRun is the --dry-run data: the decision plus §12.2's preview
// (would_affect, sample_ids, request, would_backup). An unchanged upsert has
// no request to preview, so the embedded preview is nil and only
// would_affect: 0 is said.
type upsertDryRun struct {
	Action      string         `json:"action"`
	MatchedBy   map[string]any `json:"matched_by"`
	ID          any            `json:"id"`
	WriteMode   string         `json:"write_mode,omitempty"`
	Fields      []string       `json:"fields"`
	Diff        map[string]any `json:"diff,omitempty"`
	WouldAffect int            `json:"would_affect"`
	*safety.DryRunResult
}

func runUpsert(ctx context.Context, d *Deps, f *upsertFlags, slug string) (*output.Envelope, error) {
	cfg := d.cfg()
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if f.publish && f.draft {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--publish and --draft are mutually exclusive")
	}
	status := manifest.StatusKeep
	switch {
	case f.publish:
		status = manifest.StatusPublished
	case f.draft:
		status = manifest.StatusDraft
	}
	if status != manifest.StatusKeep {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, apierr.From(err).WithHint("drop --publish/--draft: every write to %s is live", t.Slug)
		}
	}
	fields := make([]string, 0, len(f.match))
	for _, m := range f.match {
		if m = strings.TrimSpace(m); m != "" {
			fields = append(fields, m)
		}
	}
	if len(fields) == 0 {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--match needs at least one field").
			WithHint("e.g. --match slug (the default) or --match email,company")
	}

	raw, warnings, err := f.data.build(d, t, t.Shard)
	if err != nil {
		return nil, err
	}
	var bodyID any
	if id, ok := raw["id"]; ok && manifest.IsScalar(id) {
		bodyID = id
	}
	body, removed := cleanBody(raw)
	if removed != nil {
		warnings = append(warnings, statusIgnoredWarning(nil))
	}
	// §9.15.2: masked values in merged fields are left out now; those
	// inside rich text or json are put back once the match is known.
	whole := wholesaleValues(t.Shard)
	body, masked, maskedWhole := withoutMasked(body, whole)
	if len(masked) > 0 {
		warnings = append(warnings, maskedWarning(masked, nil))
	}
	if len(body) == 0 {
		return nil, apierr.New(apierr.CodeBadRequestBody, "upsert needs a document body").
			WithHint("pass --data-file body.json, --data '{...}' or --set k=v (including the --match fields)")
	}

	p := query.Params{Depth: query.IntPtr(0)}
	if warn, err := applyWriteLocale(d, t, f.locale, &p); err != nil {
		return nil, err
	} else if warn != nil {
		warnings = append(warnings, *warn)
	}
	body, refWarn, err := d.resolveBodyRefs(ctx, t.Shard, body, p.Locale)
	if err != nil {
		return nil, err
	}
	if refWarn != nil {
		warnings = append(warnings, *refWarn)
	}
	if w := d.relationIDWarning(body); w != nil {
		warnings = append(warnings, *w)
	}

	matched := map[string]any{}
	for _, field := range fields {
		v, ok := manifest.Lookup(body, field)
		if field == "id" {
			// The body's id is never sent, but it may be matched on.
			v, ok = bodyID, bodyID != nil
		}
		if !ok || v == nil {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--match %s: the body has no value for %q, and the value to match on comes from the body", field, field).
				WithHint("add it (e.g. --set %s=<value>), or choose --match fields the body carries: %s",
					field, strings.Join(sortedMapKeys(body), ", "))
		}
		if !manifest.IsScalar(v) {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--match %s: the body's value is not a string, number or boolean, so it cannot be matched on", field).
				WithHint("match on a scalar field (a slug, an e-mail), or write the relationship id itself")
		}
		matched[field] = v
	}
	spec := matchSpec(t, fields, matched, p.Locale)
	list, err := d.lookupMatches(ctx, spec, true)
	if err != nil {
		return nil, err
	}
	if len(list.Docs) > 1 {
		return nil, matchAmbiguous(t, list, spec, "--match")
	}
	var current map[string]any
	var id any
	idString := ""
	if len(list.Docs) == 0 {
		if err := idMatchCreate(t, fields, matched, "upsert"); err != nil {
			return nil, err
		}
	}
	if len(list.Docs) == 1 {
		doc := list.Docs[0]
		current, id, idString = map[string]any(doc), doc.ID(), doc.IDString()
		for _, w := range bodyWarnings(current, body, nil, t.Slug, false) {
			if w.Code == WarnFieldNotOnDocument {
				warnings = append(warnings, w)
			}
		}
		// §9.12.8.3: a string over an editor state, an object over an array.
		if err := storedShapeError(d.commandName(), orphanTarget{t: t, id: idString}, body, current); err != nil {
			return nil, err
		}
	}
	if w := bodyIDMismatch(t, bodyID, id, matched, "the body"); w != nil {
		warnings = append(warnings, *w)
	}
	// --set-md into a field the schema cannot type: checked against the
	// matched document, else against other documents of the collection.
	mdWarns, err := f.data.guardMDTargets(ctx, d, client, orphanTarget{t: t, id: idString}, p)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, mdWarns...)
	if maskedWhole {
		var removed []string
		body, _, removed = restoreMasked(current, body, whole)
		if len(removed) > 0 {
			warnings = append(warnings, maskRemovedWarning(removed, nil))
		}
	}
	opts := diffOptions(t.Shard, f.ignore, false)
	dc := decide(current, body, t.flags().Drafts, status, opts, f.force)
	if dc.action != actionCreate {
		// §9.7 block advice is about the rows the write sends, checked
		// against the matched document as it is now (which outranks an
		// older census); an unchanged upsert sends nothing.
		kept := warnings[:0:0]
		for _, w := range warnings {
			if !blockCheckCodes[w.Code] {
				kept = append(kept, w)
			}
		}
		warnings = kept
		if dc.action == actionUpdate {
			warnings = append(warnings, blockTypeWarningsWith(d, t, t.Shard, dc.writeBody(), current)...)
		}
	}
	if dc.action == actionCreate {
		if err := uploadCreateError(t, "upsert"); err != nil {
			return nil, err
		}
	}
	if paths := redactPlanDiff(d, dc.diff); len(paths) > 0 {
		warnings = append(warnings, output.DataRedactedWarning(paths))
	}
	// status keep over a published document with a pending draft saves a
	// draft (saved_as_draft); a --publish is warned by the update path.
	_, pendingWarns, _ := d.pendingDraftCheck(ctx, orphanTarget{t: t, id: idString}, current, dc, status, p)
	warnings = append(warnings, pendingWarns...)
	meta := &output.UpsertMeta{MatchedBy: matched, ID: id, Forced: f.force && dc.action == actionUpdate &&
		dc.diff != nil && dc.diff.Identical}

	if dc.action == actionUnchanged {
		meta.Action = upsertUnchanged
		run := d.beginRun()
		if cfg.DryRun {
			env := output.New(safety.CmdUpsert, output.KindOpResult, upsertDryRun{
				Action: actionUnchanged, MatchedBy: matched, ID: id, Fields: []string{}, Diff: upsertDiff(dc, current),
			}).WithTarget(t.envTarget(id)).WithMeta(run.meta())
			env.Meta.DryRun = true
			env.Meta.Upsert = meta
			for _, w := range warnings {
				env.AddWarning(w)
			}
			return env, nil
		}
		env := output.New(safety.CmdUpsert, output.KindDoc, payload.Doc(current)).
			WithTarget(t.envTarget(id)).
			WithChanged(&output.Changed{IDs: []any{}}).
			WithMeta(run.meta())
		env.Meta.Upsert = meta
		for _, w := range warnings {
			env.AddWarning(w)
		}
		return env, nil
	}

	writeBody := dc.writeBody()
	wp := dc.writeParams(p)
	wp.Depth = query.IntPtr(f.depth)
	var env *output.Envelope
	if dc.action == actionCreate {
		env, err = runCreateOne(ctx, d, client, safety.CmdUpsert, &createFlags{depth: f.depth, noEchoCheck: f.noEchoCheck},
			t, writeBody, wp, warnings)
	} else {
		env, err = runUpdateOne(ctx, d, client, safety.CmdUpsert, &updateFlags{depth: f.depth, noEchoCheck: f.noEchoCheck},
			t, idString, writeBody, wp, warnings)
	}
	if err != nil {
		if dc.action == actionCreate {
			_, err = matchFieldRejected(err, t, fields, matched)
		}
		return nil, err
	}
	if cfg.DryRun {
		meta.Action = dc.action
		if res, ok := env.Data.(safety.DryRunResult); ok {
			env.Data = upsertDryRun{Action: dc.action, MatchedBy: matched, ID: id, WriteMode: dc.mode,
				Fields: dc.fields, Diff: upsertDiff(dc, current), WouldAffect: res.WouldAffect, DryRunResult: &res}
		}
		env.Meta.Upsert = meta
		return env, nil
	}
	meta.Action = upsertUpdated
	if dc.action == actionCreate {
		meta.Action = upsertCreated
		if doc, ok := env.Data.(payload.Doc); ok {
			meta.ID = doc.ID()
		}
	}
	if doc, ok := env.Data.(payload.Doc); ok {
		if w := matchNormalized(t, fields, matched, doc, "the body"); w != nil {
			env.AddWarning(*w)
		}
		if f.depth == 0 && dc.action == actionUpdate {
			if w := hookRewroteWarning(doc, dc, opts, "run the same upsert again: it writes the declared value back"); w != nil {
				env.AddWarning(*w)
			}
		}
	}
	env.Meta.Upsert = meta
	return env, nil
}

// upsertDiff is the decision's diff in the shape every dry run shares
// (docDiffData): its base is the newest version the match read. nil for a
// create, which has nothing to compare with.
func upsertDiff(dc decision, current map[string]any) map[string]any {
	if dc.diff == nil {
		return nil
	}
	return docDiffData(diffBaseLatest, *dc.diff, current, false, false)
}
