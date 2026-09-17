package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// `pay apply` is the write half of §9.11's edit pipeline.
//
// It exists so that the two facts a pipeline already knows do not have to be
// retyped at the end of it: WHICH document is being edited (the upstream
// envelope's `target`) and WHICH fields changed (its `edits.fields`).
//
// The second is the one that matters. The obvious implementation of "write the
// piped document back" is to PATCH the whole thing, and that is wrong in a way
// that is hard to see and expensive to undo: a document read from Payload
// carries `createdAt`, `updatedAt`, `_status`, every unrelated field, and — at
// any depth above 0 — relationships expanded into whole documents. Sending all
// of it back means a pipeline that moved one block also rewrites the publish
// state and flattens every relationship on the page.
//
// So `apply` writes exactly the fields the transforms recorded. Everything else
// about it is `pay update <id>`: the same risk level, the same confirmation,
// the same audit record, the same echo-diff. It shares runUpdateOne with it for
// that reason rather than reimplementing the write.

func init() { Register(newApplyCmd) }

type applyFlags struct {
	fields    []string
	allFields bool
	depth     int
	draft     bool
	publish   bool
	unpublish bool
	locale    string
	selectF   []string
	noEcho    bool
}

func newApplyCmd(rt *Runtime) *cobra.Command {
	f := &applyFlags{}
	cmd := &cobra.Command{
		GroupID: GroupEdit,
		Use:     "apply [collection] [id]",
		Short:   "Write a piped, locally edited document back to Payload (PATCH)",
		Long: `Reads an edited document from stdin and writes it back.

The collection and id come from the piped envelope's ` + "`target`" + `, so a pipeline that
started with ` + "`pay get pages 12`" + ` needs no arguments here. Give them explicitly to
write a document that has no target — one built by hand, or read from a file.

ONLY the fields the pipeline changed are sent. That list is the piped envelope's
` + "`edits.fields`" + `, which every ` + "`pay blocks`" + ` verb appends to. A document read from
Payload also carries createdAt, _status and — above --depth 0 — relationships
expanded into whole documents; PATCHing all of that back is how a one-block
reorder silently rewrites a page. Use --all-fields to send everything anyway.`,
		Args: rangeArgs(0, 2, "pay apply [collection] [id]"),
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.fields, "field", nil,
		"write only this field (repeatable; defaults to every field the pipeline touched)")
	fl.BoolVar(&f.allFields, "all-fields", false,
		"send every field in the piped document, not just the edited ones")
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.StringSliceVar(&f.selectF, "select", nil, "limit the echoed document to these fields")
	fl.BoolVar(&f.draft, "draft", false, "save as a draft, skipping required-field validation")
	fl.BoolVar(&f.publish, "publish", false, "also set _status=published")
	fl.BoolVar(&f.unpublish, "unpublish", false, "also set _status=draft")
	fl.StringVar(&f.locale, "locale", "", "write into this locale")
	fl.BoolVar(&f.noEcho, "no-echo-check", false, "skip the §10.2 echo-diff")

	cmd.RunE = runData(rt, safety.CmdApply, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runApply(ctx, c, d, f, args)
	})
	SetHelp(cmd, applyHelp())
	return cmd
}

func runApply(ctx context.Context, cmd *cobra.Command, d *Deps, f *applyFlags, args []string) (*output.Envelope, error) {
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	in, err := readPipeInput(d, safety.CmdApply)
	if err != nil {
		return nil, err
	}
	if in.Bare {
		return nil, apierr.New(apierr.CodeNoEdits,
			"stdin holds a bare array, and `pay apply` writes a document").
			WithHint("pipe the whole document rather than `--path .<field>`: `pay get <collection> <id> --depth 0 | pay blocks … | pay apply`")
	}

	slug, id, err := applyTarget(in, args)
	if err != nil {
		return nil, err
	}
	body, fields, warnings, err := applyBody(in, f)
	if err != nil {
		return nil, err
	}

	if f.publish && f.unpublish {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--publish and --unpublish are mutually exclusive")
	}

	// A global has no id and goes to POST /globals/{slug}; a collection
	// document goes to PATCH /{slug}/{id}. The two diverge only here.
	if in.Target != nil && in.Target.Kind == "global" {
		return applyGlobal(ctx, d, client, f, in, slug, body, fields, warnings)
	}
	if id == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"`pay apply` needs the document's id, and the piped envelope carries none").
			WithHint("name it: `pay apply %s <id>`; a document read with `pay get` carries its own target and needs no arguments", slug)
	}

	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if f.draft || f.publish || f.unpublish {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	if f.publish {
		body["_status"] = "published"
	}
	if f.unpublish {
		body["_status"] = "draft"
	}

	p := query.Params{Select: f.selectF, Depth: query.IntPtr(f.depth)}
	locWarn, err := applyWriteLocale(d, t, f.locale, &p)
	if err != nil {
		return nil, err
	}
	if locWarn != nil {
		warnings = append(warnings, *locWarn)
	}
	if f.draft {
		p.Draft = query.BoolPtr(true)
	}
	if err := query.ValidateSelect(f.selectF, t.schema(d.cfg())); err != nil {
		return nil, err
	}

	uf := &updateFlags{selectF: f.selectF, noEchoCheck: f.noEcho}
	env, err := runUpdateOne(ctx, d, client, safety.CmdApply, uf, t, id, body, p, warnings)
	if err != nil {
		return nil, err
	}
	return env.WithEdits(appliedEdits(in, fields)), nil
}

// applyTarget decides which document is being written. Explicit arguments win
// over the envelope's target — a caller who names both has said which one they
// mean — and disagreeing with the envelope is worth a warning, not a refusal.
func applyTarget(in *pipeInput, args []string) (slug, id string, err error) {
	if in.Target != nil {
		slug = in.Target.Slug
		if in.Target.ID != nil {
			id = fmt.Sprint(in.Target.ID)
		}
	}
	switch len(args) {
	case 2:
		return args[0], args[1], nil
	case 1:
		if slug == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgs,
				"`pay apply %s` is a collection with no id, and the piped envelope carries no target", args[0]).
				WithHint("pass both: `pay apply <collection> <id>`")
		}
		return args[0], id, nil
	}
	if slug == "" {
		return "", "", apierr.New(apierr.CodeInvalidArgs,
			"the piped envelope carries no target, so there is nothing to say WHERE to write it").
			WithHint("name it: `pay apply <collection> <id>`. A document from `pay get` carries its own target.")
	}
	return slug, id, nil
}

// applyBody assembles the PATCH body from the edited document.
func applyBody(in *pipeInput, f *applyFlags) (map[string]any, []string, []output.Warning, error) {
	var warnings []output.Warning

	if f.allFields {
		body := map[string]any{}
		for k, v := range in.Doc {
			body[k] = v
		}
		// The server owns these. Sending them back is at best ignored and at
		// worst a validation failure on a field the caller never touched.
		for _, k := range []string{"id", "createdAt", "updatedAt"} {
			delete(body, k)
		}
		warnings = append(warnings, output.Warning{
			Code: "apply_all_fields",
			Message: "--all-fields sends every field of the piped document, including ones no transform touched; " +
				"a field the read expanded (--depth > 0) is written back expanded",
			Hint: "drop --all-fields to write only the fields recorded in edits.fields",
		})
		return body, sortedMapKeys(body), warnings, nil
	}

	fields := f.fields
	if len(fields) == 0 && in.Edits != nil {
		fields = in.Edits.Fields
	}
	if len(fields) == 0 {
		return nil, nil, nil, apierr.New(apierr.CodeNoEdits,
			"the piped envelope records no edited fields, so there is nothing to write")
	}

	body := map[string]any{}
	var missing []string
	for _, path := range fields {
		v, ok := docValueAt(in.Doc, path)
		if !ok {
			missing = append(missing, path)
			continue
		}
		setPath(body, path, v)
	}
	if len(missing) > 0 {
		return nil, nil, nil, apierr.New(apierr.CodeInvalidArgs,
			"the piped document has no %s, so it cannot be written", strings.Join(missing, ", ")).
			WithHint("a `--path` or `--select` between the edit and `pay apply` drops the field the edit recorded; keep the whole document in the pipe")
	}
	return body, fields, warnings, nil
}

// applyGlobal is the same write against a global. It reuses the CmdGlobalsUpdate
// risk level rather than apply's own: writing a global has no per-document
// recovery path, and routing it through a pipeline must not make it cheaper to
// confirm than `pay globals update` is.
func applyGlobal(ctx context.Context, d *Deps, client *payload.Client, f *applyFlags, in *pipeInput,
	slug string, body map[string]any, fields []string, warnings []output.Warning) (*output.Envelope, error) {
	cfg := d.cfg()
	g, err := d.global(ctx, slug)
	if err != nil {
		return nil, err
	}
	t := g.asCollTarget()
	if f.draft {
		if err := checkDrafts(g.Slug, g.flags().Drafts); err != nil {
			return nil, err
		}
	}

	p := query.Params{Depth: query.IntPtr(f.depth), Select: f.selectF}
	if warn, err := applyWriteLocale(d, t, f.locale, &p); err != nil {
		return nil, err
	} else if warn != nil {
		warnings = append(warnings, *warn)
	}
	if f.draft {
		p.Draft = query.BoolPtr(true)
	}

	op := safety.Op{Command: safety.CmdGlobalsUpdate, Selector: safety.SelectorNone, Global: true}
	path := payload.GlobalPath(g.Slug)
	w := d.newWriteOp(op, g.Slug, "POST", path)
	w.global = true

	if cfg.DryRun {
		q, _ := p.Encode()
		env, err := emitDryRun(d, w, safety.CmdApply, "POST",
			client.URLFor(&payload.Request{Method: "POST", Path: path, Query: q}), body, 1, nil, warnings...)
		if err != nil {
			return nil, err
		}
		// The preview carries the same edit log the real write does. The
		// collection path gets this from runApply wrapping runUpdateOne; the
		// global path returns straight from here, so without this line
		// `pay apply --dry-run` on a global is the one envelope in the family
		// that cannot say what the pipeline did.
		return env.WithEdits(&output.Edits{Fields: fields, Ops: opsOf(in)}), nil
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

	env := output.New(safety.CmdApply, output.KindGlobal, res.Doc).
		WithTarget(g.envTarget()).
		WithChanged(&output.Changed{Updated: 1, IDs: []any{}}).
		WithEdits(&output.Edits{Fields: fields, Ops: opsOf(in)}).
		WithMeta(w.run.meta()).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    fmt.Sprintf("pay globals get %s --depth 0%s", g.Slug, d.profileFlag()),
		})
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	for _, warn := range echoWarnings(g.Slug, body, res.Doc, f.selectF, echoConfig{
		Shard: g.Shard, Ignore: cfg.EchoCheckIgnore, LocaleAll: f.locale == "all", Disabled: f.noEcho,
	}) {
		env.AddWarning(warn)
	}
	return w.finish(env, res.HTTP, 1, 0, "")
}

// opsOf is the piped envelope's op log, or nil when there is none.
func opsOf(in *pipeInput) []output.EditOp {
	if in == nil || in.Edits == nil {
		return nil
	}
	return in.Edits.Ops
}

// appliedEdits carries the pipeline's op log onto the write's envelope, so the
// record of WHAT was done survives into the thing that did it. Fields is
// restated as the list actually sent, which --field may have narrowed.
func appliedEdits(in *pipeInput, fields []string) *output.Edits {
	return &output.Edits{Fields: fields, Ops: opsOf(in)}
}

func applyHelp() *Help {
	return &Help{
		Synopsis: []string{
			"… | pay apply [collection] [id] [--field PATH ...] [--all-fields]",
			"              [--draft|--publish|--unpublish] [--locale CODE] [--dry-run|--yes]",
		},
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum", ValuesFrom: "discovery.collections",
				Example: "pages"},
			{Name: "id", Required: false, Type: "string", Example: "12"},
		},
		FlagInfo: map[string]FlagInfo{
			"field":      {Grammar: "PATH", Repeatable: true, Note: "narrows the write further; it can never widen it beyond the piped document"},
			"all-fields": {Note: "sends every field, including server-owned ones and any relationship the read expanded"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the document Payload echoed back, with changed.updated = 1 and edits.ops listing what the pipeline did`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "the whole loop, previewed first",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv type:cta --first | pay apply --dry-run"},
			{Why: "the whole loop, written",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm id:67f3a1 | pay apply --yes"},
			{Why: "write and publish in the same call",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add mediaBlock | pay apply --publish --yes"},
			{Why: "a document edited by hand, with no target in the pipe",
				Cmd: "cat page.json | pay apply pages 12 --all-fields --yes"},
			{Why: "a global's blocks field",
				Cmd: "pay globals get header --depth 0 | pay blocks rm last | pay apply --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Putting `--path .layout` between the edit and `pay apply`.",
				Right: "apply needs the whole document to read the edited fields out of it. Narrow the OUTPUT of apply instead, or use `pay blocks ls` to inspect."},
			{Wrong: "Reaching for --all-fields because the write \"should\" include everything.",
				Right: "It sends createdAt, _status and every expanded relationship back. The default — the fields edits.fields records — is what you want unless you edited the JSON yourself."},
			{Wrong: "Reading without --draft and applying with it.",
				Right: "A read without --draft returns the PUBLISHED document, so `pay apply --draft` would save the published content as a new draft and throw the draft away. Use --draft on BOTH ends of the pipe, or on neither."},
			{Wrong: "Editing a document read at the default depth.",
				Right: "Read with --depth 0. Above it, relationships come back as whole documents and are written back that way; the pipeline warns with populated_relationship when it sees one."},
			{Wrong: "Piping `pay find` into the pipeline.",
				Right: "apply writes ONE document. `pay find` returns a list; loop over the ids with `pay get`."},
		},
		SeeAlso: []string{
			"pay blocks ls                    # what is in the field, and how to address it",
			"pay update <collection> <id>     # the same PATCH, with the body typed on the command line",
		},
	}
}
