package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
)

// ---------------------------------------------------------------------------
// §9.11 the edit pipeline
// ---------------------------------------------------------------------------
//
// `pay get pages 12 | pay blocks mv … | pay blocks rm … | pay apply` is one
// document passed hand to hand. This file is the hand-off: it reads whatever
// the previous stage wrote, decides which field holds the rows, and hands the
// result back in a shape the transform commands and `pay apply` share.
//
// Three properties make the chain safe, and all three live here rather than in
// the six command files:
//
//  1. **An upstream failure stays an upstream failure.** A stage whose stdin
//     holds `ok:false` re-emits that error with its original code and exit
//     status. Without this a failed `pay get` becomes `bad_request_body` from
//     `pay blocks mv`, which sends the caller to fix a selector that was never
//     wrong.
//  2. **Upstream warnings are carried forward.** `pay get` may have warned that
//     a locale fell back, and that warning is about the document the LAST stage
//     is going to write. A pipe that drops it hides it from the only command
//     that could act on it.
//  3. **The edit is recorded, not the document.** Every stage appends to
//     `edits`, so `pay apply` writes the fields the pipeline touched instead of
//     PATCHing back every key PayCLI happened to read.

// warn codes this file raises.
const (
	// WarnUpstreamError marks a stage that did nothing because its input was
	// already an error envelope.
	WarnUpstreamError = "upstream_error"
	// WarnBlocksFieldInferred marks a field chosen by looking at the document
	// rather than at the project's schema.
	WarnBlocksFieldInferred = "blocks_field_inferred"
	// WarnFieldAbsent marks a --field the document does not carry, which is
	// almost always a --select that trimmed it away.
	WarnFieldAbsent = "field_absent"
	// WarnPopulatedRelationship marks a row that carries an expanded
	// relationship object where the API expects an id.
	WarnPopulatedRelationship = "populated_relationship"
	// WarnEnvelopeUnwrapped marks a --data that was handed a whole PayCLI
	// envelope and was read as its .data.
	WarnEnvelopeUnwrapped = "envelope_unwrapped"
)

// pipeInput is one stage's decoded stdin.
type pipeInput struct {
	// Doc is the document, when the upstream data was an object. It is the
	// value every transform edits in place-by-copy.
	Doc map[string]any
	// BareRows is set instead of Doc when the upstream data was a bare array —
	// `pay get pages 1 --path .layout | pay blocks mv …`. The rows are then the
	// whole payload and there is no field name to record, which is why
	// `pay apply` refuses such an envelope.
	BareRows []any
	// Bare reports which of the two above is set. A nil Doc is not enough: an
	// empty object is a legal document.
	Bare bool

	Kind     output.DataKind
	Target   *output.Target
	Edits    *output.Edits
	Warnings []output.Warning
}

// readPipeInput decodes the stage's stdin.
func readPipeInput(d *Deps, command string) (*pipeInput, error) {
	raw, err := io.ReadAll(d.inStream())
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeNoInput, "stdin could not be read")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, apierr.New(apierr.CodeNoInput,
			"`pay %s` edits a document read from stdin, and stdin was empty", command)
	}

	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"stdin is not valid JSON").
			WithHint("the previous stage must emit one JSON value; `pay get … --output json` is the default and does")
	}

	switch t := v.(type) {
	case map[string]any:
		if env, ok := asEnvelope(t); ok {
			return fromEnvelope(env)
		}
		return &pipeInput{Doc: t, Kind: output.KindDoc}, nil
	case []any:
		return &pipeInput{BareRows: t, Bare: true, Kind: output.KindDocList}, nil
	default:
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"stdin holds %s, and a block edit needs a document or an array of rows", jsonKindOf(v))
	}
}

// asEnvelope reports whether an object is a PayCLI envelope rather than a
// Payload document that happens to have an `ok` field.
//
// All four keys are required together. A collection with a boolean `ok` column
// is entirely plausible; one that also has an integer `v`, a string
// `data_kind` and an object `meta` is not, and no Payload document has those
// three because PayCLI invented them.
func asEnvelope(obj map[string]any) (map[string]any, bool) {
	if _, ok := obj["ok"].(bool); !ok {
		return nil, false
	}
	if _, ok := obj["v"].(json.Number); !ok {
		return nil, false
	}
	if _, ok := obj["data_kind"].(string); !ok {
		return nil, false
	}
	if _, ok := obj["meta"].(map[string]any); !ok {
		return nil, false
	}
	return obj, true
}

// fromEnvelope unpacks a PayCLI envelope into a pipeInput.
func fromEnvelope(env map[string]any) (*pipeInput, error) {
	if ok, _ := env["ok"].(bool); !ok {
		return nil, upstreamError(env)
	}

	in := &pipeInput{}
	in.Kind = output.DataKind(envString(env["data_kind"]))
	in.Target = decodeInto[output.Target](env["target"])
	in.Edits = decodeInto[output.Edits](env["edits"])
	if ws := decodeInto[[]output.Warning](env["warnings"]); ws != nil {
		in.Warnings = *ws
	}

	switch data := env["data"].(type) {
	case map[string]any:
		in.Doc = data
	case []any:
		// An array is either a LIST OF DOCUMENTS (`pay find`) or the rows of
		// one field (`pay get … --path .layout`). data_kind is what tells them
		// apart: --path narrows `data` but deliberately leaves data_kind
		// describing what the command produced (§10.1), so only a genuine
		// doc_list says "these are documents".
		if in.Kind == output.KindDocList {
			return nil, apierr.New(apierr.CodeBadRequestBody,
				"the piped envelope is a doc_list, and a block edit acts on ONE document").
				WithHint("`pay get <collection> <id>` returns one; `pay find` returns a list")
		}
		in.BareRows, in.Bare = data, true
	case nil:
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"the piped envelope carries no data").
			WithHint("a `--path` that narrowed data to nothing, or a command that returns no document, cannot be edited")
	default:
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"the piped envelope's data is %s, and a block edit needs a document or an array of rows",
			jsonKindOf(env["data"])).
			WithHint("pipe a single document: `pay get <collection> <id> --depth 0`")
	}

	return in, nil
}

// upstreamError rebuilds the previous stage's error so this stage exits with
// the SAME code and status. Everything the upstream diagnosed — the field list,
// the HTTP block, did_you_mean — is preserved, because re-classifying it here
// would throw away the only evidence that exists.
func upstreamError(env map[string]any) error {
	e := decodeInto[apierr.Error](env["error"])
	if e == nil || e.Code == "" {
		return apierr.New(apierr.CodeBadRequestBody,
			"the previous command in the pipe failed and its envelope carries no error object")
	}
	// Exit and Retriable are derived, never trusted from the wire: a truncated
	// or hand-edited envelope must not be able to choose this process's exit
	// status.
	e.Exit = e.Code.Exit()
	e.Retriable = e.Code.Retriable()
	return e
}

// decodeInto re-decodes a generic JSON value into a typed struct. The envelope
// arrives as map[string]any because the document inside it must keep exact
// numbers; the metadata around it is better handled as its real type.
func decodeInto[T any](v any) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return &out
}

// envString reads a string out of a decoded envelope key.
func envString(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// field resolution
// ---------------------------------------------------------------------------

// rowField is the array a transform is about to edit, plus everything known
// about what may legally go in it.
type rowField struct {
	// Path is the document field path, "" when the rows arrived bare.
	Path string
	// Rows is the current array.
	Rows []any
	// Accepts are the blockType slugs this field takes, nil when discovery
	// could not resolve them. nil is "unknown", never "accepts nothing", so it
	// never blocks a write (§7.10).
	Accepts []string
	// Shard is the collection's field schema when one was available.
	Shard *discovery.Shard
}

// resolveRowField finds the array to edit.
//
// The order is deliberate: an explicit --field always wins, then the project's
// schema, then the document's own shape. The last step is what makes the family
// usable against a project that has never been discovered, and it announces
// itself with a warning rather than pretending it knew.
func resolveRowField(ctx context.Context, d *Deps, in *pipeInput, field string) (*rowField, []output.Warning, error) {
	if in.Bare {
		if field != "" {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"--field %s was given, but stdin holds a bare array with no document around it", field).
				WithHint("pipe the whole document instead of `--path .<field>`, or drop --field")
		}
		return &rowField{Rows: in.BareRows}, nil, nil
	}

	var warns []output.Warning
	shard := pipeShard(ctx, d, in.Target)
	if w := localValidationWarning(in.Target, shard); w != nil {
		warns = append(warns, *w)
	}

	path := field
	if path == "" {
		chosen, inferred, err := pickRowField(in.Doc, shard)
		if err != nil {
			return nil, nil, err
		}
		path = chosen
		if inferred {
			warns = append(warns, output.Warning{
				Code: WarnBlocksFieldInferred,
				Message: fmt.Sprintf(
					"%q was chosen by looking at the document (it is the only array whose rows all carry a blockType); this project's schema was not available to confirm it", path),
				Paths: []string{path},
				Hint:  "run `pay discover --refresh`, or name the field explicitly with --field",
			})
		}
	}

	value, present := docValueAt(in.Doc, path)
	list, isArray := value.([]any)
	switch {
	case !present || value == nil:
		// An absent field is empty, not an error: `pay blocks add` has to be
		// able to fill a layout that is null, and every other verb fails
		// immediately afterwards with selector_no_match, which says the same
		// thing more precisely.
		warns = append(warns, output.Warning{
			Code:    WarnFieldAbsent,
			Message: fmt.Sprintf("the piped document has no %q, so it is being treated as an empty array", path),
			Paths:   []string{path},
			Hint:    "a --select on the read that produced this document drops the fields it does not name; re-read without it",
		})
		list = nil
	case !isArray:
		return nil, nil, apierr.New(apierr.CodeInvalidArgs,
			"%s is %s, not an array of rows", path, jsonKindOf(value)).
			WithHint("`pay describe <collection> --path .blocks` lists this project's blocks fields")
	}

	rf := &rowField{Path: path, Rows: list, Shard: shard}
	if shard != nil {
		if slugs, ok := shard.BlockTypesFor(path); ok {
			rf.Accepts = slugs
		}
	}
	return rf, warns, nil
}

// pipeShard loads the field schema for the collection the envelope names. It
// never fails: no target, no profile or no cache all mean "no schema", and the
// commands degrade to the document's own shape.
func pipeShard(ctx context.Context, d *Deps, target *output.Target) *discovery.Shard {
	if target == nil || target.Slug == "" {
		return nil
	}
	kind := cache.KindCollection
	if target.Kind == "global" {
		kind = cache.KindGlobal
	}
	return d.shardFor(ctx, target.Slug, kind)
}

// pickRowField chooses the field when --field was not given.
func pickRowField(doc map[string]any, shard *discovery.Shard) (path string, inferred bool, err error) {
	// 1. The project's schema, narrowed to the blocks fields this document
	//    actually carries. A collection may declare a blocks field the caller
	//    trimmed away with --select, and offering it as a candidate would name
	//    a field the document cannot be edited through.
	if shard != nil && len(shard.BlockFields) > 0 {
		declared := make([]string, 0, len(shard.BlockFields))
		for p := range shard.BlockFields {
			declared = append(declared, p)
		}
		sort.Strings(declared)

		present := declared[:0:0]
		for _, p := range declared {
			if v, ok := docValueAt(doc, p); ok && v != nil {
				present = append(present, p)
			}
		}
		switch {
		case len(present) == 1:
			return present[0], false, nil
		case len(present) > 1:
			return "", false, ambiguousField(present, "this collection has more than one blocks field")
		case len(declared) == 1:
			// Declared but absent: name it anyway so the caller gets the
			// field_absent warning rather than a guess off the document.
			return declared[0], false, nil
		case len(declared) > 1:
			return "", false, ambiguousField(declared, "this collection has more than one blocks field and the piped document carries none of them")
		}
	}

	// 2. The document itself. Only an array whose every row carries a
	//    blockType qualifies, which excludes plain `array` fields — editing
	//    one is legal, but choosing one for the caller is not.
	var candidates []string
	for _, key := range sortedMapKeys(doc) {
		list, ok := doc[key].([]any)
		if ok && rows.LooksLikeBlocks(list) {
			candidates = append(candidates, key)
		}
	}
	switch len(candidates) {
	case 1:
		return candidates[0], true, nil
	case 0:
		return "", false, apierr.New(apierr.CodeFieldAmbiguous,
			"no blocks field could be identified in the piped document").
			WithHint("name it with --field <path>; `pay describe <collection> --path .blocks` lists this project's blocks fields")
	default:
		return "", false, ambiguousField(candidates, "the piped document carries more than one blocks field")
	}
}

func ambiguousField(candidates []string, why string) error {
	return apierr.New(apierr.CodeFieldAmbiguous,
		"%s (%s), so --field is required", why, strings.Join(candidates, ", ")).
		WithDidYouMean(candidates...)
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// docValueAt reads a dotted path out of a document. The bool is presence, so a
// field that exists and is null is distinguishable from one that is absent —
// the difference between "this page has no blocks yet" and "--select dropped
// the column".
func docValueAt(doc map[string]any, path string) (any, bool) {
	cur := any(doc)
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// ---------------------------------------------------------------------------
// blockType validation
// ---------------------------------------------------------------------------

// checkBlockType rejects a blockType the field does not accept.
//
// This is the one local rejection in the family that is worth an exit code of
// its own. Payload does not validate blockType: it DROPS a row whose blockType
// it does not recognise and answers 201, so the failure surfaces as "the block
// I added is not there" long after the command that caused it, with nothing in
// either envelope pointing at the typo.
//
// A nil Accepts is unknown and never blocks (§7.10 conflict 37): PayCLI
// refusing a slug it merely failed to discover would be a wrong answer it
// produced itself.
func checkBlockType(rf *rowField, slug string) error {
	if rf == nil || len(rf.Accepts) == 0 {
		return nil
	}
	for _, s := range rf.Accepts {
		if s == slug {
			return nil
		}
	}
	where := rf.Path
	if where == "" {
		where = "this field"
	}
	return apierr.New(apierr.CodeBlockTypeUnknown,
		"%s accepts no blockType %q", where, slug).
		WithDidYouMean(apierr.DidYouMean(slug, rf.Accepts)...).
		WithHint("the slugs it accepts are: %s", strings.Join(rf.Accepts, ", "))
}

// ---------------------------------------------------------------------------
// stage output
// ---------------------------------------------------------------------------

// emitStage assembles the envelope a transform command returns: the edited
// document, the target it came from, the accumulated edit log, and every
// warning raised upstream or here.
//
// The document — not a summary of the change — is the payload, because the
// next stage's input is this stage's output. That symmetry is the whole design:
// any number of transforms compose, in any order, with no temporary files.
func emitStage(command string, in *pipeInput, rf *rowField, op output.EditOp, warns []output.Warning) *output.Envelope {
	kind := in.Kind
	var data any
	if in.Bare {
		data = rf.Rows
		kind = output.KindDocList
	} else {
		doc := shallowCopyDoc(in.Doc)
		if rf.Path != "" {
			setPath(doc, rf.Path, rf.Rows)
		}
		data = doc
		if kind == "" {
			kind = output.KindDoc
		}
	}

	edits := in.Edits
	if edits == nil {
		edits = &output.Edits{}
	}
	op.Command = command
	op.Field = rf.Path
	op.Rows = len(rf.Rows)
	edits.Ops = append(edits.Ops, op)
	edits.Touch(rf.Path)

	env := output.New(command, kind, data).WithEdits(edits)
	if in.Target != nil {
		env.WithTarget(in.Target)
	}
	for _, w := range in.Warnings {
		env.AddWarning(w)
	}
	for _, w := range warns {
		env.AddWarning(w)
	}
	for _, w := range populatedRelationshipWarnings(rf) {
		env.AddWarning(w)
	}
	if next := applyNext(in.Target, rf.Path); next != nil {
		env.WithNext(next)
	}
	return env
}

// shallowCopyDoc copies the top level so the edited field can be replaced
// without mutating the decoded input, which the error paths still print.
func shallowCopyDoc(doc map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	return out
}

// applyNext is the hand-off to the write half. It is a runnable string, so the
// pipeline never ends in "now what?".
func applyNext(target *output.Target, field string) *output.Next {
	if target == nil || target.Slug == "" {
		return nil
	}
	return &output.Next{
		Reason: output.ReasonVerifyWrite,
		Cmd:    "pay apply --dry-run",
		Args:   map[string]any{"collection": target.Slug, "id": target.ID, "field": field},
		Alternatives: []output.Alternative{
			{Why: "write it, showing the request first", Cmd: "pay apply --dry-run"},
			{Why: "write it", Cmd: "pay apply --yes"},
		},
	}
}

// populatedRelationshipWarnings flags rows carrying an expanded relationship.
//
// A read at --depth 1 replaces `"media": 7` with the whole media document.
// Writing that object back is not an error PayCLI can see locally and not one
// Payload reliably rejects — it is how a pipeline silently rewrites a
// relationship into an embedded copy. The fix is one flag on the READ, three
// stages upstream, which is why the warning names it.
func populatedRelationshipWarnings(rf *rowField) []output.Warning {
	if rf == nil {
		return nil
	}
	var paths []string
	for i, row := range rf.Rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range sortedMapKeys(obj) {
			if key == rows.KeyID || key == rows.KeyBlockType || key == rows.KeyBlockName {
				continue
			}
			if looksPopulated(obj[key]) {
				paths = append(paths, fmt.Sprintf("%s[%d].%s", rf.Path, i, key))
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return []output.Warning{{
		Code: WarnPopulatedRelationship,
		Message: "these rows carry a relationship that the read expanded into a full document; writing it back " +
			"stores the expansion instead of the id",
		Paths: paths,
		Hint:  "re-read the document with --depth 0 before editing it, and re-run the pipeline",
	}}
}

// looksPopulated reports an expanded relationship: an object with an `id` and
// at least one of the timestamps Payload puts on every document. A lexical
// richText value is an object too, and it has neither.
func looksPopulated(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if _, hasID := obj["id"]; !hasID {
		return false
	}
	_, created := obj["createdAt"]
	_, updated := obj["updatedAt"]
	return created || updated
}
