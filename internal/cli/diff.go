package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// §9.14 `pay diff` — a structural diff of two documents
// ---------------------------------------------------------------------------
//
// Proving that an edit touched exactly one block used to take a read to a
// file, a hand edit, a second read and `diff <(jq -S …) <(jq -S …)` — which
// reports every re-indented line and every shifted array index. `pay diff`
// is that comparison done by internal/docdiff: rows matched by id, rich text
// reported as text, and — for a body about to be written — Payload's own
// PATCH semantics applied first, so the answer is "what this write would
// change", not "how this partial body differs from a whole document".

// Warning codes `pay diff` raises.
const (
	// WarnNeverPublished marks a --draft-vs-published diff whose "published"
	// side is not a published document.
	WarnNeverPublished = "never_published"
	// WarnFieldNotOnDocument marks body keys the stored document does not
	// have — usually a typo, which Payload drops without an error.
	WarnFieldNotOnDocument = "field_not_on_document"
	// WarnBodyIDMismatch marks a body whose own id names another document.
	WarnBodyIDMismatch = "body_id_mismatch"
	// WarnBodyPublishes marks a body carrying _status "published": the write
	// publishes even when it is sent with --draft.
	WarnBodyPublishes = "body_publishes"
)

// Side sources in data.left/data.right.
const (
	diffSourceFile    = "file"
	diffSourceStdin   = "stdin"
	diffSourceServer  = "server"
	diffSourcePatched = "server+patch"
)

func init() { Register(newDiffCmd) }

// diffFlags is `pay diff`'s flag set.
type diffFlags struct {
	left, right       string
	data              writeData
	draftVsPublished  bool
	draft             bool
	global            string
	ignore            []string
	includeTimestamps bool
	full              bool
}

func (f *diffFlags) hasData() bool {
	return f.data.data != "" || f.data.dataFile != "" || len(f.data.set) > 0 || len(f.data.setJSON) > 0 || len(f.data.setMD) > 0
}

// local reports the one form that needs no server: both sides are files.
func (f *diffFlags) local() bool { return f.left != "" || f.right != "" }

func newDiffCmd(rt *Runtime) *cobra.Command {
	f := &diffFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "diff [<collection> <id> [<id2>]]",
		Short:   "Structural diff: two documents, a write body vs the server, or draft vs published",
		Long: `Compare two Payload documents and report every difference as a path-addressed change.

Rows of arrays and blocks fields are matched by their id, so a reorder is reported
as moves and an edit inside a moved row is reported once, at the row's new path.
Rich text (Lexical) is compared by structure and reported as plain text
(from_text/to_text) instead of as the editor's JSON tree. createdAt and updatedAt
are ignored at every depth unless --include-timestamps.

With --data-file (or --data/--set/--set-json) the right side is what Payload would
STORE after this write: the body is applied to the server's newest draft with
Payload's PATCH semantics — keys you omit keep their value, named groups merge key
by key, a row whose id matches a stored row keeps its unsent keys, rows you omit
are deleted. So a whole edited copy of a document reports exactly the fields you
changed, and nothing is written.

The exit status is 0 whether or not the two sides differ; read data.identical.`,
		Args: maxArgs(3, "pay diff <collection> <id> (--data-file F | --draft-vs-published) | pay diff <collection> <idA> <idB> | pay diff --global SLUG --data-file F | pay diff --left A.json --right B.json"),
	}
	handler := func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runDiff(ctx, d, f, args)
	}
	online := runData(rt, safety.CmdDiff, handler)
	offline := runOfflineData(rt, safety.CmdDiff, handler)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		// Two files need no server, and neither does rejecting an impossible
		// flag combination: both run without building a client, so a typo
		// never waits on discovery or fails on a missing profile first.
		if _, err := f.resolveMode(args); err != nil || f.local() {
			return offline(c, args)
		}
		return online(c, args)
	}

	fl := cmd.Flags()
	fl.StringVar(&f.left, "left", "", "left side: a JSON file (a document or a PayCLI envelope; - for stdin)")
	fl.StringVar(&f.right, "right", "", "right side: a JSON file (a document or a PayCLI envelope; - for stdin)")
	f.data.register(cmd)
	fl.BoolVar(&f.draftVsPublished, "draft-vs-published", false, "left = the published document, right = the newest draft")
	fl.BoolVar(&f.draft, "draft", false, "with two ids: compare the newest drafts instead of the stored documents")
	fl.StringVar(&f.global, "global", "", "diff this global instead of a collection document")
	fl.StringArrayVar(&f.ignore, "ignore", nil, "path pattern to ignore, e.g. meta.image or layout[].blockName (repeatable)")
	fl.BoolVar(&f.includeTimestamps, "include-timestamps", false, "also report createdAt/updatedAt")
	fl.BoolVar(&f.full, "full", false, "keep values lossless: rich-text editor states in from/to instead of text")

	SetHelp(cmd, diffHelp())
	return cmd
}

// runOfflineData is runData for a command form that needs no server at all.
// Unlike runLocalData it does not even build a client or consult discovery:
// comparing two files must work with no profile, no network and a cold cache.
func runOfflineData(rt *Runtime, name string, h dataHandler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt.Command = name
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		env, err := h(ctx, cmd, &Deps{RT: rt}, args)
		if err != nil {
			env = output.NewError(name, err)
		} else if env == nil {
			env = output.NewError(name, apierr.New(apierr.CodeInternal,
				"command %q produced no result", name))
		}
		rt.emit(env)
		return nil
	}
}

// diffSide describes where one side of the comparison came from.
type diffSide struct {
	Source     string `json:"source"`
	Path       string `json:"path,omitempty"`
	Collection string `json:"collection,omitempty"`
	Global     string `json:"global,omitempty"`
	ID         any    `json:"id,omitempty"`
	Draft      *bool  `json:"draft,omitempty"`
	Status     any    `json:"status,omitempty"`
	UpdatedAt  any    `json:"updated_at,omitempty"`
	// DataKind is the envelope's data_kind when a file held an envelope.
	DataKind string `json:"data_kind,omitempty"`
	// Semantics names how the right side of a body diff was computed.
	Semantics string `json:"semantics,omitempty"`
	// BodyKeys are the top-level keys the body sent.
	BodyKeys []string `json:"body_keys,omitempty"`
}

// diffMode is the one form a `pay diff` invocation resolved to.
type diffMode int

const (
	diffFiles diffMode = iota
	diffBody
	diffDraftVsPublished
	diffTwoIDs
)

// diffUsage is the hint every argument error carries: the five forms.
const diffUsage = "pay diff pages 12 --data-file page.json | pay diff pages 12 --draft-vs-published | " +
	"pay diff pages 12 13 | pay diff --global header --data-file h.json | pay diff --left a.json --right b.json"

// resolveMode validates the flag/argument combination. Every rejection names
// the forms that do exist, because the failure is always "wrong form", never
// "wrong value".
func (f *diffFlags) resolveMode(args []string) (diffMode, error) {
	bad := func(format string, a ...any) error {
		return apierr.New(apierr.CodeInvalidArgs, format, a...).WithHint("the forms are: %s", diffUsage)
	}
	if f.local() {
		switch {
		case f.left == "" || f.right == "":
			return 0, bad("--left and --right go together: each names one side of the comparison")
		case len(args) > 0 || f.global != "":
			return 0, bad("--left/--right compare two files; a collection, id or --global cannot be combined with them")
		case f.hasData() || f.draftVsPublished || f.draft:
			return 0, bad("--left/--right compare two files as they are; --data*, --set*, --draft and --draft-vs-published do not apply")
		case f.left == "-" && f.right == "-":
			return 0, bad("only one side can be read from stdin")
		}
		return diffFiles, nil
	}
	if f.draftVsPublished && f.hasData() {
		return 0, bad("--draft-vs-published and a write body are two different comparisons; pick one")
	}
	if f.global != "" {
		switch {
		case len(args) > 0:
			return 0, bad("--global takes no collection or id arguments")
		case f.draft:
			return 0, bad("--draft only applies to the two-id form")
		case f.hasData():
			return diffBody, nil
		case f.draftVsPublished:
			return diffDraftVsPublished, nil
		}
		return 0, bad("--global %s needs --data-file (or --data/--set) or --draft-vs-published", f.global)
	}
	switch len(args) {
	case 3:
		if f.hasData() || f.draftVsPublished {
			return 0, bad("two ids are compared with each other; --data* and --draft-vs-published take ONE id")
		}
		return diffTwoIDs, nil
	case 2:
		if f.draft {
			return 0, bad("--draft only applies to the two-id form: --data-file always diffs against the newest draft (what Payload patches), and --draft-vs-published reads both")
		}
		switch {
		case f.hasData():
			return diffBody, nil
		case f.draftVsPublished:
			return diffDraftVsPublished, nil
		}
		return 0, bad("pay diff %s %s needs something to compare with: --data-file F, --draft-vs-published, or a second id", args[0], args[1])
	}
	return 0, bad("pay diff needs <collection> <id> plus a comparison, --global SLUG, or --left/--right files")
}

func runDiff(ctx context.Context, d *Deps, f *diffFlags, args []string) (*output.Envelope, error) {
	mode, err := f.resolveMode(args)
	if err != nil {
		return nil, err
	}
	opts := docdiff.Options{
		IncludeTimestamps: f.includeTimestamps,
		IgnorePaths:       append([]string(nil), f.ignore...),
		Full:              f.full,
	}
	run := d.beginRun()

	var (
		left, right any
		ls, rs      *diffSide
		target      *output.Target
		warns       []output.Warning
	)
	switch mode {
	case diffFiles:
		left, ls, err = readDiffFile(d, "--left", f.left)
		if err != nil {
			return nil, err
		}
		right, rs, err = readDiffFile(d, "--right", f.right)
		if err != nil {
			return nil, err
		}
	case diffTwoIDs:
		t, err := d.collection(ctx, args[0])
		if err != nil {
			return nil, err
		}
		if f.draft {
			if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
				return nil, err
			}
		}
		var w []output.Warning
		left, ls, w, err = fetchDiffDoc(ctx, d, t, nil, args[1], f.draft)
		if err != nil {
			return nil, err
		}
		warns = append(warns, w...)
		right, rs, _, err = fetchDiffDoc(ctx, d, t, nil, args[2], f.draft)
		if err != nil {
			return nil, err
		}
		// Two documents always differ in their own id; that is not news.
		opts.IgnorePaths = append(opts.IgnorePaths, "id")
		target = t.envTarget(ls.ID)
	case diffDraftVsPublished:
		t, g, id, err := diffEntity(ctx, d, f, args)
		if err != nil {
			return nil, err
		}
		if g != nil {
			err = checkDiffDrafts(g.Slug, g.flags().Drafts, "--global "+g.Slug)
		} else {
			err = checkDiffDrafts(t.Slug, t.flags().Drafts, t.Slug+" "+id)
		}
		if err != nil {
			return nil, err
		}
		var w []output.Warning
		left, ls, w, err = fetchDiffDoc(ctx, d, t, g, id, false)
		if err != nil {
			return nil, err
		}
		warns = append(warns, w...)
		right, rs, _, err = fetchDiffDoc(ctx, d, t, g, id, true)
		if err != nil {
			return nil, err
		}
		if s, _ := ls.Status.(string); s != "" && s != "published" {
			warns = append(warns, neverPublishedWarning(ls, id))
		}
		target = diffTarget(t, g, ls.ID)
	case diffBody:
		t, g, id, err := diffEntity(ctx, d, f, args)
		if err != nil {
			return nil, err
		}
		shardTarget := t
		if g != nil {
			shardTarget = g.asCollTarget()
		}
		body, bw, err := f.data.build(d, shardTarget, shardTarget.Shard)
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			return nil, apierr.New(apierr.CodeBadRequestBody, "the write body is empty, so there is nothing to diff").
				WithHint("pass --data-file body.json, --data '{...}' or --set k=v")
		}
		warns = append(warns, bw...)
		// Payload applies a write to the NEWEST version (the draft, when the
		// collection has drafts), so that is what the body is diffed against.
		var w []output.Warning
		left, ls, w, err = fetchDiffDoc(ctx, d, t, g, id, true)
		if err != nil {
			return nil, err
		}
		warns = append(warns, w...)
		current, _ := left.(map[string]any)
		// F6: a body with {"$ref": …} is diffed as the ids it would write,
		// resolved in the locale the document was read in (as a write
		// resolves them in its own); §9.15.2: masked values as a write
		// handles them.
		locale := ""
		if loc, _, lerr := resolveLocale(d, shardTarget, "", ""); lerr == nil && loc.Requested != nil {
			locale = *loc.Requested
		}
		body, w, err = d.prepareWriteBody(ctx, writeScope{shard: shardTarget.Shard,
			preimage: func(context.Context) (map[string]any, error) { return current, nil }}, body, locale)
		if err != nil {
			return nil, err
		}
		warns = append(warns, w...)
		right = docdiff.ApplyPatch(current, body, opts)
		rs = &diffSide{
			Source: diffSourcePatched, Collection: ls.Collection, Global: ls.Global, ID: ls.ID,
			Path: f.data.dataFile, Semantics: "payload-patch", BodyKeys: sortedMapKeys(body),
		}
		warns = append(warns, bodyWarnings(current, body, ls, shardTarget.Slug, g != nil)...)
		// --set-md into a field the schema cannot type: the document is
		// right here, so what it holds there says whether that is rich text.
		// A preview warns where update refuses.
		// When the document holds nothing there, `pay update` samples other
		// documents; so does the preview, or it would be clean where update
		// refuses.
		for _, key := range f.data.mdUnchecked {
			v, ok := lookupPath(current, key)
			where := "the document"
			if (!ok || emptyMDValue(v)) && g == nil {
				if client, cerr := d.requireClient(); cerr == nil {
					v, where = d.sampleMDField(ctx, client, orphanTarget{t: t, id: id}, query.Params{}, key)
				}
			}
			if !emptyMDValue(v) && !lexical.IsEditorState(v) {
				verdict := "`pay update` refuses this write (Payload would store the editor state as a JSON string)"
				if isMap(v) {
					verdict = "the write replaces it with a Lexical editor state"
				}
				warns = append(warns, output.Warning{
					Code: warnSetMDNotRichText,
					Message: fmt.Sprintf("--set-md %s: %s holds %s there, not rich text; %s",
						key, where, jsonKindOf(v), verdict),
					Paths: []string{key},
					Hint:  "use --set " + key + "=… for a text field; `pay describe " + shardTarget.Slug + "` lists the known fields",
				})
			}
		}
		// §9.12.8.3: what update refuses against the stored document, the
		// preview warns about.
		warns = append(warns, storedShapeWarnings(body, current)...)
		target = diffTarget(t, g, ls.ID)
	}

	res := docdiff.Diff(left, right, opts)
	if d.cfg().Redact {
		if paths := redactChanges(res.Changes); len(paths) > 0 {
			warns = append(warns, output.DataRedactedWarning(paths))
		}
	}
	var data any = diffData{Left: ls, Right: rs, Result: res}
	if d.RT != nil && d.RT.Out != nil && d.RT.Out.Format == output.FormatTable && d.cfg().Path == "" {
		// --output table is for a person reading a change list; the envelope
		// keeps the lossless shape for every other format.
		data = diffTableRows(res)
		d.RT.Out.Columns = []string{"op", "path", "from", "to"}
	}
	env := output.New(safety.CmdDiff, output.KindOpResult, data).WithMeta(run.meta())
	if target != nil {
		env.WithTarget(target)
	}
	for _, w := range warns {
		env.AddWarning(w)
	}
	return env, nil
}

// diffData is the op_result payload: {left, right, identical, summary, changes}.
type diffData struct {
	Left  *diffSide
	Right *diffSide
	docdiff.Result
}

// MarshalJSON flattens the embedded result after left/right, in a fixed order.
func (d diffData) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Left      *diffSide        `json:"left"`
		Right     *diffSide        `json:"right"`
		Identical bool             `json:"identical"`
		Summary   docdiff.Summary  `json:"summary"`
		Changes   []docdiff.Change `json:"changes"`
	}{d.Left, d.Right, d.Identical, d.Summary, d.Changes})
}

// diffEntity resolves the collection+id or the global of a single-document
// form.
func diffEntity(ctx context.Context, d *Deps, f *diffFlags, args []string) (*collTarget, *globalTarget, string, error) {
	if f.global != "" {
		g, err := d.global(ctx, f.global)
		return nil, g, "", err
	}
	t, err := d.collection(ctx, args[0])
	return t, nil, args[1], err
}

func diffTarget(t *collTarget, g *globalTarget, id any) *output.Target {
	if g != nil {
		return g.envTarget()
	}
	return t.envTarget(id)
}

// fetchDiffDoc reads one side from the server at depth 0 — the depth a write
// body is expressed in, so a relationship compares as the id it is stored as.
func fetchDiffDoc(ctx context.Context, d *Deps, t *collTarget, g *globalTarget, id string, draft bool) (any, *diffSide, []output.Warning, error) {
	client, err := d.requireClient()
	if err != nil {
		return nil, nil, nil, err
	}
	locTarget := t
	if g != nil {
		locTarget = g.asCollTarget()
	}
	loc, locWarn, err := resolveLocale(d, locTarget, "", "")
	if err != nil {
		return nil, nil, nil, err
	}
	var warns []output.Warning
	if locWarn != nil {
		warns = append(warns, *locWarn)
	}
	p := query.Params{Depth: query.IntPtr(0)}
	if draft {
		p.Draft = query.BoolPtr(true)
	}
	if loc.Requested != nil {
		p.Locale = *loc.Requested
	}
	if loc.Fallback != nil {
		p.FallbackLocale = *loc.Fallback
	}

	side := &diffSide{Source: diffSourceServer, Draft: query.BoolPtr(draft)}
	var doc payload.Doc
	if g != nil {
		side.Global = g.Slug
		doc, _, err = client.GlobalGet(ctx, g.Slug, p, payload.WithKnownRoute())
	} else {
		if e := d.checkID(t, t.Slug, id); e != nil {
			return nil, nil, nil, e
		}
		side.Collection = t.Slug
		doc, _, err = client.Get(ctx, t.Slug, id, p,
			payload.WithClassify(classifyFor(t, d.cfg(), nil)), payload.WithKnownRoute())
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if g == nil {
		side.ID = doc.ID()
		if side.ID == nil {
			side.ID = id
		}
	}
	side.Status = doc["_status"]
	side.UpdatedAt = doc["updatedAt"]
	return map[string]any(doc), side, warns, nil
}

// readDiffFile reads one local side. It accepts a bare JSON value or a PayCLI
// envelope (its data is used; a version envelope's `version` sub-document is
// unwrapped); an error envelope fails with the upstream's own code.
func readDiffFile(d *Deps, flag, path string) (any, *diffSide, error) {
	side := &diffSide{Source: diffSourceFile, Path: path}
	var raw []byte
	var err error
	if path == "-" {
		side.Source, side.Path = diffSourceStdin, ""
		raw, err = io.ReadAll(d.inStream())
		if err != nil {
			return nil, nil, apierr.Wrap(err, apierr.CodeNoInput, "%s -: stdin could not be read", flag)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, nil, apierr.Wrap(err, apierr.CodeFileMissing, "%s %s could not be read", flag, path)
		}
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil, apierr.New(apierr.CodeNoInput, "%s %s is empty", flag, displayPath(path))
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil, apierr.New(apierr.CodeBadRequestBody, "%s %s is not valid JSON", flag, displayPath(path)).
			WithHint("save a document with `pay get <collection> <id> --depth 0 > page.json`")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return v, side, nil
	}
	env, ok := asEnvelope(obj)
	if !ok {
		return obj, side, nil
	}
	if okv, _ := env["ok"].(bool); !okv {
		return nil, nil, upstreamError(env)
	}
	side.DataKind = envString(env["data_kind"])
	if tgt := decodeInto[output.Target](env["target"]); tgt != nil {
		if tgt.Kind == "global" {
			side.Global = tgt.Slug
		} else {
			side.Collection = tgt.Slug
		}
		side.ID = tgt.ID
	}
	data := env["data"]
	if side.DataKind == string(output.KindVersion) {
		if m, ok := data.(map[string]any); ok {
			if inner, ok := m["version"].(map[string]any); ok {
				data = inner
			}
		}
	}
	if m, ok := data.(map[string]any); ok {
		side.Status = m["_status"]
		side.UpdatedAt = m["updatedAt"]
	}
	return data, side, nil
}

func displayPath(path string) string {
	if path == "-" {
		return "(stdin)"
	}
	return path
}

// checkDiffDrafts is checkDrafts worded for --draft-vs-published: an entity
// known to have no drafts has exactly one state, so there is nothing to
// compare. Unknown (nil) is attempted, per §9.3's tri-state rule.
func checkDiffDrafts(slug string, drafts *bool, target string) error {
	if drafts == nil || *drafts {
		return nil
	}
	return apierr.New(apierr.CodeFeatureUnavailable,
		"%q does not have drafts enabled: every save is the published state, so there is no draft to compare it with", slug).
		WithHint("compare it with a body instead: pay diff %s --data-file F", target)
}

// neverPublishedWarning explains a --draft-vs-published diff whose left side
// is not a published document. Payload stores a draft save only in the
// versions table, so a document that was never published has no published
// state at all — the plain read returns the record as it was last written by
// a non-draft save (usually the create).
func neverPublishedWarning(ls *diffSide, id string) output.Warning {
	what := ls.Collection + " " + id
	if ls.Global != "" {
		what = "global " + ls.Global
	}
	return output.Warning{
		Code: WarnNeverPublished,
		Message: fmt.Sprintf("%s has no published version (_status is %v): the left side is the stored record as last written without --draft, not live content",
			what, ls.Status),
		Hint: "check the version history with `pay versions list " + versionsTarget(ls) + "`",
	}
}

func versionsTarget(s *diffSide) string {
	if s.Global != "" {
		return "--global " + s.Global
	}
	return fmt.Sprintf("%s --id %v", s.Collection, s.ID)
}

// bodyWarnings flags body keys the stored document does not carry. Payload
// returns every field it has (null when unset), so a top-level key that is
// absent from the stored document is almost always a typo or a field this
// project does not define — and Payload drops those without an error.
func bodyWarnings(current, body map[string]any, ls *diffSide, slug string, global bool) []output.Warning {
	var out []output.Warning
	if current == nil {
		return out
	}
	var unknown []string
	for _, k := range sortedMapKeys(body) {
		switch k {
		case "id", "createdAt", "updatedAt", "_status":
			continue
		}
		if _, ok := current[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		describe := "pay describe " + slug
		out = append(out, output.Warning{
			Code: WarnFieldNotOnDocument,
			Message: fmt.Sprintf("the body sets %s, which the stored document does not have; Payload drops a field it does not define without an error",
				strings.Join(quoteAll(unknown), ", ")),
			Paths: unknown,
			Hint:  "`" + describe + "` lists this " + map[bool]string{true: "global", false: "collection"}[global] + "'s fields",
		})
	}
	if st, _ := body["_status"].(string); st == "published" {
		// payload@3.87.1 collections/operations/utilities/update.js:
		// isSavingDraft = draftArg && drafts && data._status !== 'published'.
		// A whole exported document carries _status, so "edit the export,
		// update --draft" silently publishes a page that was published before.
		out = append(out, output.Warning{
			Code: WarnBodyPublishes,
			Message: "the body sets _status \"published\": writing it publishes the document, even with --draft " +
				"(Payload only saves a draft when the body's _status is not \"published\")",
			Paths: []string{"_status"},
			Hint:  "`pay update <collection> <id> --data-file F --draft` removes _status from the body for you and saves a draft; `pay diff <collection> <id> --draft-vs-published` reviews it before publishing",
		})
	}
	if bid, ok := body["id"]; ok && ls != nil && ls.ID != nil && !docdiff.Equal(bid, ls.ID) &&
		fmt.Sprint(bid) != fmt.Sprint(ls.ID) {
		out = append(out, output.Warning{
			Code: WarnBodyIDMismatch,
			Message: fmt.Sprintf("the body's own id is %v but the diff (and a write) targets %v; the id in the URL decides which document is written, the body's id is ignored",
				bid, ls.ID),
			Paths: []string{"id"},
			Hint:  fmt.Sprintf("check the target: `pay get %s %v --depth 0`", ls.Collection, bid),
		})
	}
	return out
}

// redactChanges applies §5.3 to the change list. The envelope's own
// redaction pass is key-based, and in a diff a secret sits under "from"/"to"
// with its field name only in "path" — so `pay diff users 1 2` would print
// two API keys verbatim. A change whose path ends in a secret key is masked
// here (the fact that it changed is kept); secrets NESTED inside a reported
// value keep their own key and are masked by the envelope pass as usual. It
// returns the masked paths.
func redactChanges(changes []docdiff.Change) []string {
	var paths []string
	for i := range changes {
		c := &changes[i]
		if !redact.Key(lastPathKey(c.Path)) {
			continue
		}
		if c.From != nil {
			c.From = redact.Mask
		}
		if c.To != nil {
			c.To = redact.Mask
		}
		paths = append(paths, c.Path)
	}
	return paths
}

// lastPathKey is the last object key of a change path: "sessions" for
// "sessions[0]", "apiKey" for "users[2].apiKey".
func lastPathKey(path string) string {
	for strings.HasSuffix(path, "]") {
		i := strings.LastIndex(path, "[")
		if i < 0 {
			break
		}
		path = path[:i]
	}
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// diffTableRows is the --output table rendering: one line per change.
func diffTableRows(res docdiff.Result) []map[string]any {
	if len(res.Changes) == 0 {
		return []map[string]any{{"op": "=", "path": "(no differences)", "from": "", "to": ""}}
	}
	out := make([]map[string]any, 0, len(res.Changes))
	for _, c := range res.Changes {
		row := map[string]any{"op": string(c.Op), "path": c.Path, "from": "", "to": ""}
		switch {
		case c.Op == docdiff.OpMove:
			row["from"] = fmt.Sprintf("index %d", *c.FromIndex)
			row["to"] = fmt.Sprintf("index %d", *c.ToIndex)
		case c.Kind == docdiff.KindRow:
			if c.Op != docdiff.OpAdd {
				row["from"] = diffRowLabel(c.From)
			}
			if c.Op != docdiff.OpRemove {
				row["to"] = diffRowLabel(c.To)
			}
		default:
			if c.FromText != nil {
				row["from"] = *c.FromText
			} else if c.Op != docdiff.OpAdd {
				row["from"] = c.From
			}
			if c.ToText != nil {
				row["to"] = *c.ToText
			} else if c.Op != docdiff.OpRemove {
				row["to"] = c.To
			}
		}
		out = append(out, row)
	}
	return out
}

// diffRowLabel is a one-cell summary of a row: its blockType and id.
func diffRowLabel(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		b, _ := json.Marshal(v)
		return string(b)
	}
	parts := []string{}
	if bt, ok := m["blockType"].(string); ok {
		parts = append(parts, "<"+bt+">")
	}
	if id, ok := m["id"]; ok && id != nil {
		parts = append(parts, fmt.Sprintf("id:%v", id))
	}
	if len(parts) == 0 {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "{" + strings.Join(keys, ",") + "}"
	}
	return strings.Join(parts, " ")
}

// diffHelp is §10.5's model for `pay diff`.
func diffHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay diff <collection> <id> (--data-file F | --data JSON | --set k=v …)   # server vs this write",
			"pay diff <collection> <id> --draft-vs-published                        # published vs newest draft",
			"pay diff <collection> <idA> <idB> [--draft]                            # two documents",
			"pay diff --global <slug> (--data-file F | --draft-vs-published)",
			"pay diff --left A.json --right B.json                                  # two files, no network",
			"         [--ignore PATH]… [--include-timestamps] [--full]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages  (omit it with --global or --left/--right)"},
			{Name: "id", Required: false, Type: "string", Example: "47"},
			{Name: "id2", Required: false, Type: "string", Example: "48  (the second document of the two-id form)"},
		},
		FlagInfo: map[string]FlagInfo{
			"left":               {Note: "a `pay versions get` envelope is compared by its version sub-document"},
			"right":              {Note: "only one side may be -"},
			"data-file":          {Note: "applied with Payload's PATCH semantics to the NEWEST draft; nothing is written"},
			"draft-vs-published": {Note: "a never-published document has no published side: warning never_published"},
			"ignore":             {Grammar: "path, [] or [*] = any index", Repeatable: true},
			"set":                {Grammar: "key=value", Repeatable: true},
			"set-json":           {Grammar: "key=JSON", Repeatable: true},
			"set-md":             {Grammar: setMDGrammar, Repeatable: true, Note: setMDBodyNote},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"left":{"source":"server","collection":"pages","id":2,"draft":true},` +
				`"right":{"source":"server+patch","semantics":"payload-patch","body_keys":["layout"]},` +
				`"identical":false,"summary":{"total":1,"added":0,"removed":0,"changed":1,"moved":0,"rich_text":0,"fields":["layout"]},` +
				`"changes":[{"op":"change","path":"layout[2].blocks[1].title","from":"Alt","to":"Neu"}]}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "prove a locally edited copy changes exactly what you meant, before writing it",
				Cmd: "pay diff pages 2 --data-file page-2.json"},
			{Why: "what an update would change (same flags as pay update)",
				Cmd: "pay diff pages 2 --set title='Neuer Titel'"},
			{Why: "what publishing would take live",
				Cmd: "pay diff pages 47 --draft-vs-published"},
			{Why: "just the changed paths",
				Cmd: "pay diff pages 47 --draft-vs-published --path '.changes[].path'"},
			{Why: "two exported files, offline",
				Cmd: "pay diff --left before.json --right after.json"},
			{Why: "a saved copy against the live document, piped",
				Cmd: "pay get pages 2 --depth 0 --draft | pay diff --left saved.json --right -"},
			{Why: "a readable change list for a person",
				Cmd: "pay diff pages 2 --data-file page-2.json --output table"},
			{Why: "a global",
				Cmd: "pay diff --global header --data-file header.json"},
		},
		Mistakes: []Mistake{
			{Wrong: "Diffing with `diff <(jq -S …) <(jq -S …)` to check a dry-run.",
				Right: "`pay diff <collection> <id> --data-file F` matches rows by id and applies Payload's PATCH rules, so a whole edited copy reports only the fields you changed."},
			{Wrong: "Reading a partial --data-file body as \"everything else will be deleted\".",
				Right: "Payload keeps every key the body omits and merges named groups; only arrays/blocks are replaced (rows you omit are deleted, rows with a known id keep their unsent keys). The diff shows exactly that."},
			{Wrong: "Expecting a non-zero exit when the sides differ.",
				Right: "The exit status is 0 either way. Branch on data.identical or data.summary.total."},
			{Wrong: "Comparing a --depth 1 export with the server.",
				Right: "The server side is read at --depth 0. Export with `pay get <collection> <id> --depth 0`, or every expanded relationship shows up as a change."},
			{Wrong: "Treating a --draft-vs-published diff of a never-published page as \"what is live\".",
				Right: "A never_published warning says there is no published version; the left side is then just the stored record."},
		},
		SeeAlso: []string{
			"pay update <collection> <id> --data-file F --dry-run   # the request that would be sent",
			"pay versions diff <collection> <vA> <vB>               # two version snapshots",
			"pay get <collection> <id> --depth 0                    # the document itself",
		},
	}
}

// quoteAll quotes every string for a human-readable list in a message.
func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return out
}
