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
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// §9.10 write input semantics — shared by create, update, upload and
// globals update.
// ---------------------------------------------------------------------------

// writeData is §9.10.2's combinable data flag set. The flags are NOT
// alternatives: they are deep-merged in a fixed order with later winning.
type writeData struct {
	alt      string // upload only; the lowest-precedence contributor
	data     string
	dataFile string
	set      []string
	setJSON  []string
}

func (w *writeData) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&w.data, "data", "", "JSON object body (use @- to read stdin)")
	fl.StringVar(&w.dataFile, "data-file", "", "read the JSON object body from a file")
	fl.StringArrayVar(&w.set, "set", nil, "k=v field (repeatable, typed and coerced against the schema)")
	fl.StringArrayVar(&w.setJSON, "set-json", nil, "k=JSON field (repeatable, no coercion — the escape hatch)")
}

// build merges the data flags into one request body and applies §9.10.1's
// typing and coercion. It performs no network I/O.
func (w *writeData) build(d *Deps, t *collTarget, shard *discovery.Shard) (map[string]any, []output.Warning, error) {
	body := map[string]any{}
	var warnings []output.Warning

	if w.alt != "" {
		body["alt"] = w.alt
	}

	// --data / --data-file (base object)
	for _, raw := range []struct{ src, val string }{{"--data", w.data}, {"--data-file", w.dataFile}} {
		if raw.val == "" {
			continue
		}
		bytes, err := readBody(d, raw.src, raw.val)
		if err != nil {
			return nil, nil, err
		}
		obj, err := decodeObject(raw.src, bytes)
		if err != nil {
			return nil, nil, err
		}
		deepMerge(body, obj)
	}

	// --set (typed + coerced scalars)
	for _, pair := range w.set {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs, "--set %q is not key=value", pair)
		}
		typed, err := query.TypeValue(raw)
		if err != nil {
			return nil, nil, err
		}
		coerced, warn, err := coerceSet(d, t, shard, key, raw, typed)
		if err != nil {
			return nil, nil, err
		}
		if warn != nil {
			warnings = append(warnings, *warn)
		}
		setPath(body, key, coerced)
	}

	// --set-json (raw JSON, bypasses coercion entirely)
	for _, pair := range w.setJSON {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs, "--set-json %q is not key=JSON", pair)
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"--set-json %s: the value is not valid JSON", key).
				WithHint("quote it for the shell: --set-json 'layout=[{\"blockType\":\"cta\"}]'")
		}
		setPath(body, key, v)
	}

	warnings = append(warnings, blockTypeWarnings(t, shard, body)...)
	return body, warnings, nil
}

// readBody resolves --data/--data-file, including `--data @-` for stdin.
func readBody(d *Deps, flag, value string) ([]byte, error) {
	switch {
	case flag == "--data-file":
		b, err := os.ReadFile(value)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeFileMissing, "--data-file %s could not be read", value)
		}
		return b, nil
	case value == "@-":
		b, err := io.ReadAll(d.inStream())
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeBadRequestBody, "--data @- could not read stdin")
		}
		return b, nil
	case strings.HasPrefix(value, "@"):
		name := strings.TrimPrefix(value, "@")
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeFileMissing, "--data @%s could not be read", name)
		}
		return b, nil
	default:
		return []byte(value), nil
	}
}

// decodeObject parses a JSON object, keeping numbers exact.
func decodeObject(flag string, b []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"%s is not valid JSON", flag).
			WithHint("the body must be a single JSON object, e.g. '{\"title\":\"t\"}'")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"%s must be a JSON object, not %s", flag, jsonKindOf(v)).
			WithHint("wrap the value in an object: '{\"field\": <value>}'")
	}
	return obj, nil
}

func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number, float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	}
	return "a scalar"
}

// deepMerge merges src into dst. Objects merge recursively; arrays and scalars
// are replaced outright (§9.10.2).
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				deepMerge(existing, sub)
				continue
			}
			clone := map[string]any{}
			deepMerge(clone, sub)
			dst[k] = clone
			continue
		}
		dst[k] = v
	}
}

// setPath assigns a possibly dotted key. Dotted keys are supported so that
// `--set meta.title=x` reaches a group field without dropping to --set-json;
// §9.10 does not forbid them and a literal dot in a Payload field name is not
// legal anyway.
func setPath(body map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	cur := body
	for i, p := range parts {
		if i == len(parts)-1 {
			// Objects deep-merge with whatever an earlier flag contributed;
			// arrays and scalars replace it outright (§9.10.2).
			if obj, ok := value.(map[string]any); ok {
				if existing, ok := cur[p].(map[string]any); ok {
					deepMerge(existing, obj)
					return
				}
			}
			cur[p] = value
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// coerceSet implements §9.10.1. It returns the value to send, an optional
// warning, or a local error. When the manifest never learned the field's shape
// there is no coercion and no rejection: the §9.4-typed value is sent with a
// write_shape_unknown warning.
func coerceSet(d *Deps, t *collTarget, shard *discovery.Shard, key, raw string, typed any) (any, *output.Warning, error) {
	if typed == nil {
		return nil, nil, nil
	}
	fld, ok := fieldFor(shard, key)
	if !ok {
		return typed, &output.Warning{
			Code:    warnWriteShapeUnknown,
			Message: fmt.Sprintf("PayCLI does not know the shape of %q, so the value was sent as typed by --set", key),
			Paths:   []string{key},
			Hint:    "run `pay describe " + targetSlug(t) + " --field " + rootOf(key) + "`, or use --set-json to send an exact JSON value",
		}, nil
	}

	if fld.WriteShape != nil {
		switch *fld.WriteShape {
		case discovery.WriteShapeRelValue, discovery.WriteShapeRelList:
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"%s is a polymorphic relationship, whose value is {relationTo, value}; --set cannot express it", key).
				WithHint("use --set-json '%s={\"relationTo\":\"<collection>\",\"value\":<id>}' or --data", key)
		case discovery.WriteShapeJSON:
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"%s is a %s field, whose value is a JSON document; --set cannot express it", key, fld.PayloadType).
				WithHint("use --set-json '%s=<json>' or --data-file body.json", key)
		case discovery.WriteShapeID:
			return coerceRelationID(d, t, key, fld, typed)
		case discovery.WriteShapeIDArray:
			return coerceRelationIDs(d, t, key, fld, raw, typed)
		}
	}

	switch fld.PayloadType {
	case discovery.TypeNumber:
		if _, ok := typed.(json.Number); ok {
			return typed, nil, nil
		}
		return nil, nil, typeMismatch(t, key, fld, "a number", typed)
	case discovery.TypeCheckbox:
		if _, ok := typed.(bool); ok {
			return typed, nil, nil
		}
		return nil, nil, typeMismatch(t, key, fld, "true or false", typed)
	case discovery.TypeDate:
		s, ok := typed.(string)
		if !ok {
			return nil, nil, typeMismatch(t, key, fld, "an ISO-8601 date", typed)
		}
		iso, err := absoluteDate(key, s)
		if err != nil {
			return nil, nil, err
		}
		return iso, nil, nil
	case discovery.TypeBlocks, discovery.TypeRichText, discovery.TypeJSON, discovery.TypeArray, discovery.TypeGroup:
		return nil, nil, apierr.New(apierr.CodeInvalidArgs,
			"%s is a %s field, whose value is structured JSON; --set cannot express it", key, fld.PayloadType).
			WithHint("use --set-json '%s=<json>' or --data-file body.json", key)
	case discovery.TypeSelect, discovery.TypeRadio:
		s, ok := typed.(string)
		if ok && len(fld.Options) > 0 &&
			fld.OptionsSource != discovery.SourceUnknown && fld.OptionsSource != discovery.SourceNA &&
			!containsString(fld.Options, s) {
			return nil, nil, apierr.New(apierr.CodeInvalidOption,
				"%q is not a valid value for %s.%s", s, targetSlug(t), key).
				WithDidYouMean(apierr.DidYouMean(s, fld.Options)...).
				WithHint("%s", "allowed values: "+strings.Join(fld.Options, ", "))
		}
		return stringify(typed), nil, nil
	case discovery.TypeUnknown, "":
		return typed, &output.Warning{
			Code:    warnWriteShapeUnknown,
			Message: fmt.Sprintf("the type of %q was never discovered, so the value was sent as typed by --set", key),
			Paths:   []string{key},
			Hint:    "use --set-json to send an exact JSON value",
		}, nil
	default:
		return stringify(typed), nil, nil
	}
}

func targetSlug(t *collTarget) string {
	if t == nil || t.Slug == "" {
		return "<collection>"
	}
	return t.Slug
}

func rootOf(path string) string {
	root, _, _ := strings.Cut(path, ".")
	return root
}

// fieldFor looks a --set key up in the shard.
func fieldFor(shard *discovery.Shard, key string) (discovery.Field, bool) {
	if shard == nil || len(shard.Fields) == 0 {
		return discovery.Field{}, false
	}
	return shard.Field(key)
}

// coerceRelationID coerces a single relationship/upload value to the target
// collection's id type. This is §9.10's worked example: Payload rejects
// {"heroImage":"4"} with an unintelligible "invalid relationships" message and
// accepts {"heroImage":4}.
func coerceRelationID(d *Deps, t *collTarget, key string, fld discovery.Field, typed any) (any, *output.Warning, error) {
	idType, target := relationIDType(d, fld)
	switch idType {
	case discovery.IDTypeNumber:
		if v, ok := typed.(json.Number); ok {
			return v, nil, nil
		}
		// A *string* here is the caller explicitly forcing a string (--set
		// 'heroImage="4"'), and Payload answers {"heroImage":"4"} with the
		// unintelligible "invalid relationships" message §9.10 quotes. It is
		// rejected rather than silently re-typed.
		return nil, nil, relationMismatch(t, key, fld, target, "number", typed)
	case discovery.IDTypeString:
		switch v := typed.(type) {
		case string:
			return v, nil, nil
		case json.Number:
			return v.String(), nil, nil
		}
		return nil, nil, relationMismatch(t, key, fld, target, "string", typed)
	default:
		return typed, &output.Warning{
			Code: warnWriteShapeUnknown,
			Message: fmt.Sprintf("the id type of %s (%s → %s) was never discovered, so the value was sent uncoerced",
				key, targetSlug(t), target),
			Paths: []string{key},
			Hint:  "pin it once: pay config set profiles.<profile>.id_type number",
		}, nil
	}
}

// coerceRelationIDs is the hasMany form: comma-split, each element coerced.
func coerceRelationIDs(d *Deps, t *collTarget, key string, fld discovery.Field, raw string, typed any) (any, *output.Warning, error) {
	if list, ok := typed.([]any); ok {
		return list, nil, nil
	}
	parts := query.SplitList(raw)
	out := make([]any, 0, len(parts))
	var warn *output.Warning
	for _, part := range parts {
		v, err := query.TypeValue(part)
		if err != nil {
			return nil, nil, err
		}
		coerced, w, err := coerceRelationID(d, t, key, fld, v)
		if err != nil {
			return nil, nil, err
		}
		if w != nil && warn == nil {
			warn = w
		}
		out = append(out, coerced)
	}
	return out, warn, nil
}

// relationIDType returns the id type of a relationship's target collection.
func relationIDType(d *Deps, fld discovery.Field) (idType, target string) {
	if len(fld.RelationTo) == 0 {
		return discovery.IDTypeUnknown, "?"
	}
	target = fld.RelationTo[0]
	if d == nil || d.Manifest == nil {
		return discovery.IDTypeUnknown, target
	}
	c, ok := d.Manifest.Collection(target)
	if !ok || c.IDType == "" {
		return discovery.IDTypeUnknown, target
	}
	return c.IDType, target
}

func relationMismatch(t *collTarget, key string, fld discovery.Field, target, want string, got any) error {
	shape := ""
	if fld.WriteShape != nil {
		shape = *fld.WriteShape
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"%s expects a %s id (%s.%s → %s, id_type=%s); got %s. Payload rejects this with an unintelligible \"invalid relationships\" message.",
		key, want, targetSlug(t), key, target, want, describeValue(got)).
		WithHint("write_shape=%s — pass a %s, or use --set-json '%s=<json>'", shape, want, key)
}

func typeMismatch(t *collTarget, key string, fld discovery.Field, want string, got any) error {
	shape := "n/a"
	if fld.WriteShape != nil {
		shape = *fld.WriteShape
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"%s.%s is a %s field and expects %s; got %s", targetSlug(t), key, fld.PayloadType, want, describeValue(got)).
		WithHint("write_shape=%s — quote a literal string with --set '%s=\"value\"', or use --set-json", shape, key)
}

func describeValue(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("the string %q", t)
	case bool:
		return fmt.Sprintf("the boolean %v", t)
	case json.Number:
		return "the number " + t.String()
	case nil:
		return "null"
	}
	return fmt.Sprintf("%v", v)
}

func stringify(v any) any {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	return v
}

// absoluteDate accepts §9.4's absolute date forms. Relative forms are refused
// because a stored date must not depend on when the command happened to run.
func absoluteDate(key, s string) (string, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC().Format(time.RFC3339), nil
		}
	}
	if ts, err := time.Parse("2006-01-02", s); err == nil {
		return ts.UTC().Format(time.RFC3339), nil
	}
	return "", apierr.New(apierr.CodeInvalidArgs,
		"%s needs an absolute date: YYYY-MM-DD or a full RFC 3339 instant; got %q", key, s).
		WithHint("example: --set '%s=2026-08-01T09:30:00Z'", key)
}

// blockTypeWarnings implements §9.7: Payload silently drops an unknown
// blockType and answers 201, so the warning is emitted BEFORE sending.
func blockTypeWarnings(t *collTarget, shard *discovery.Shard, body map[string]any) []output.Warning {
	if shard == nil || len(shard.Fields) == 0 {
		return nil
	}
	var out []output.Warning
	for _, fld := range shard.Fields {
		if fld.PayloadType != discovery.TypeBlocks {
			continue
		}
		value, ok := lookupPath(body, fld.Path)
		if !ok {
			continue
		}
		items, ok := value.([]any)
		if !ok || len(items) == 0 {
			continue
		}
		allowed, resolved := shard.Blocks[fld.Path]
		var unknown []string
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			bt, _ := obj["blockType"].(string)
			if bt == "" {
				continue
			}
			if !resolved || !containsString(allowed, bt) {
				if !containsString(unknown, bt) {
					unknown = append(unknown, bt)
				}
			}
		}
		if len(unknown) == 0 {
			continue
		}
		hint := discovery.DescribeBlocksHelp(nil, "", fld.Path)
		if resolved {
			hint = "allowed blockType values for " + fld.Path + ": " + strings.Join(allowed, ", ")
		}
		out = append(out, output.Warning{
			Code: warnUnknownBlockType,
			Message: fmt.Sprintf("%s: blockType %s could not be confirmed against this project's block slugs. "+
				"Payload silently discards an unknown blockType and still answers 201.",
				fld.Path, strings.Join(quoteAll(unknown), ", ")),
			Paths: []string{fld.Path},
			Hint:  hint,
		})
	}
	return out
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return out
}

// ---------------------------------------------------------------------------
// §10.2 echo-diff
// ---------------------------------------------------------------------------

// echoConfig carries everything the echo-diff needs to avoid false positives.
type echoConfig struct {
	Shard     *discovery.Shard
	Ignore    []string
	LocaleAll bool
	Disabled  bool

	// localeRoots is computed by echoDiff: the root fields whose echoed value
	// is a locale map. See localeMappedRoots.
	localeRoots map[string]bool
}

// echoDiff compares the request body against the echoed document and splits
// the residue into the two §10.2 classes. `dropped` is the real Payload
// footgun; `normalized` is informational (a formatSlug hook, an e-mail
// lower-cased) and must never be reported as a dropped input.
func echoDiff(slug string, sent map[string]any, doc payload.Doc, cfg echoConfig) []output.Warning {
	if cfg.Disabled || cfg.LocaleAll || len(sent) == 0 || doc == nil {
		return nil
	}
	// doc is a named map type, so it is converted explicitly: the type
	// assertions inside collectLeaves match map[string]any, not payload.Doc.
	echoed := map[string]any(doc)
	cfg.localeRoots = localeMappedRoots(cfg.Shard, sent, echoed)
	leaves := map[string]any{}
	collectLeaves("", sent, echoed, leaves)

	droppedPaths := map[string]bool{}
	droppedSent := map[string]any{}
	normPaths := []string{}
	normSent := map[string]any{}
	normReturned := map[string]any{}

	for _, path := range sortedKeysOf(leaves) {
		if skipEchoPath(path, cfg) {
			continue
		}
		got, ok := lookupPath(map[string]any(doc), path)
		if !ok {
			droppedPaths[firstMissingPrefix(map[string]any(doc), path)] = true
			droppedSent[path] = leaves[path]
			continue
		}
		if jsonEqual(leaves[path], got) {
			continue
		}
		normPaths = append(normPaths, path)
		normSent[path] = leaves[path]
		normReturned[path] = got
	}

	var out []output.Warning
	if len(droppedSent) > 0 {
		paths := sortedKeysOf(droppedPaths)
		out = append(out, output.Warning{
			Code: output.WarnInputSilentlyDropped,
			Message: fmt.Sprintf("%d value(s) you sent are absent from the document Payload returned. "+
				"Payload discards unknown fields and unknown block types without an error.", len(droppedSent)),
			Paths: paths,
			Sent:  droppedSent,
			Hint:  droppedHint(slug, paths, cfg.Shard),
		})
	}
	if len(normPaths) > 0 {
		out = append(out, output.Warning{
			Code: output.WarnValueNormalizedByServer,
			Message: fmt.Sprintf("%d value(s) you sent came back different. This is normally a beforeChange hook "+
				"(formatSlug, e-mail normalisation), not a rejection.", len(normPaths)),
			Paths:    normPaths,
			Sent:     normSent,
			Returned: normReturned,
			Hint:     "list persistently noisy fields in the profile's echo_check_ignore, or pass --no-echo-check",
		})
	}
	return out
}

func droppedHint(slug string, paths []string, shard *discovery.Shard) string {
	for _, p := range paths {
		root := rootOf(strings.SplitN(p, "[", 2)[0])
		if shard != nil {
			if fld, ok := shard.Field(root); ok && fld.PayloadType == discovery.TypeBlocks {
				return "pay describe " + slug + " --field " + root +
					" prints this project's allowed blockType values, or states in plain words that they are not " +
					"determinable from the API and where to read them from instead."
			}
		}
	}
	root := "<field>"
	if len(paths) > 0 {
		root = rootOf(strings.SplitN(paths[0], "[", 2)[0])
	}
	return "pay describe " + slug + " --field " + root + " shows the field's real shape; Payload discards a field it does not recognise without saying so."
}

// collectLeaves flattens the request body into leaf paths. A hasMany array
// that the server merely reordered is skipped whole (§10.2's skip list).
func collectLeaves(prefix string, v any, echoed any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			var next any
			if m, ok := echoed.(map[string]any); ok {
				next = m[k]
			}
			collectLeaves(path, child, next, out)
		}
	case []any:
		if reorderedOnly(t, echoed) {
			return
		}
		for i, child := range t {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			var next any
			if list, ok := echoed.([]any); ok && i < len(list) {
				next = list[i]
			}
			collectLeaves(path, child, next, out)
		}
	default:
		if prefix != "" {
			out[prefix] = v
		}
	}
}

// reorderedOnly reports whether two lists hold the same scalar multiset, which
// is §10.2's "hasMany reordering" exemption.
func reorderedOnly(sent []any, echoed any) bool {
	list, ok := echoed.([]any)
	if !ok || len(list) != len(sent) || len(sent) == 0 {
		return false
	}
	norm := func(in []any) []string {
		out := make([]string, 0, len(in))
		for _, v := range in {
			b, err := json.Marshal(v)
			if err != nil {
				return nil
			}
			out = append(out, string(b))
		}
		sort.Strings(out)
		return out
	}
	a, b := norm(sent), norm(list)
	if a == nil || b == nil {
		return false
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		// Identical multisets: either untouched or merely reordered. Either
		// way there is nothing an agent must act on.
		return true
	}
	return false
}

// skipEchoPath applies §10.2's skip list.
func skipEchoPath(path string, cfg echoConfig) bool {
	switch path {
	case "id", "createdAt", "updatedAt", "_status":
		return true
	}
	// array item ids: layout[0].id
	if strings.HasSuffix(path, ".id") && strings.Contains(path, "[") {
		return true
	}
	for _, ig := range cfg.Ignore {
		ig = strings.TrimSpace(ig)
		if ig == "" {
			continue
		}
		if path == ig || strings.HasPrefix(path, ig+".") || strings.HasPrefix(path, ig+"[") {
			return true
		}
	}
	// §7.9d, narrowed to the shape it was written for. The spec skips "every
	// field whose manifest localized is null" because a locale=all write
	// echoes {code: value} maps. Per-field `localized` is not introspectable
	// at all (§7.6 LOCALIZATION_PER_FIELD_UNKNOWN), so on the typical project
	// *every* field is null and the blanket reading would disable the whole
	// check — including the unknown-blockType case §10.2 exists for (verified
	// live: the check reported nothing on a discarded layout array). The skip
	// is therefore applied to the fields whose echo actually IS a locale map.
	if cfg.localeRoots[rootOf(strings.SplitN(path, "[", 2)[0])] {
		return true
	}
	return false
}

// localeMappedRoots finds the root fields whose echoed value is a locale map:
// `localized` was never learned and the server answered an object where a
// scalar (or an array) was sent.
func localeMappedRoots(shard *discovery.Shard, sent, echoed map[string]any) map[string]bool {
	out := map[string]bool{}
	if shard == nil || len(shard.Fields) == 0 {
		return out
	}
	for root, sentValue := range sent {
		fld, ok := shard.Field(root)
		if !ok || fld.Localized != nil {
			continue
		}
		if _, sentObject := sentValue.(map[string]any); sentObject {
			continue
		}
		if _, gotObject := echoed[root].(map[string]any); gotObject {
			out[root] = true
		}
	}
	return out
}

// lookupPath resolves a leaf path ("layout[0].blockType") in a decoded body.
func lookupPath(root map[string]any, path string) (any, bool) {
	var cur any = root
	for _, step := range splitPath(path) {
		if step.index >= 0 {
			list, ok := cur.([]any)
			if !ok || step.index >= len(list) {
				return nil, false
			}
			cur = list[step.index]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[step.key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// firstMissingPrefix returns the shortest prefix of path that the document
// does not have, so a whole discarded array element is reported once.
func firstMissingPrefix(root map[string]any, path string) string {
	steps := splitPath(path)
	for i := 1; i <= len(steps); i++ {
		if _, ok := lookupPath(root, joinPath(steps[:i])); !ok {
			return joinPath(steps[:i])
		}
	}
	return path
}

type pathStep struct {
	key   string
	index int
}

func splitPath(path string) []pathStep {
	var out []pathStep
	for _, part := range strings.Split(path, ".") {
		name := part
		for {
			open := strings.Index(name, "[")
			if open < 0 {
				break
			}
			close := strings.Index(name[open:], "]")
			if close < 0 {
				break
			}
			idx, err := strconv.Atoi(name[open+1 : open+close])
			if err != nil {
				break
			}
			if head := name[:open]; head != "" {
				out = append(out, pathStep{key: head, index: -1})
			}
			out = append(out, pathStep{index: idx})
			name = name[open+close+1:]
		}
		if name != "" {
			out = append(out, pathStep{key: name, index: -1})
		}
	}
	return out
}

func joinPath(steps []pathStep) string {
	var b strings.Builder
	for _, s := range steps {
		if s.index >= 0 {
			fmt.Fprintf(&b, "[%d]", s.index)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.key)
	}
	return b.String()
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// createdAsDraftWarning is §10.2's second warning: the document exists but is
// invisible, and publishing will re-run exactly the validation --draft skipped.
func createdAsDraftWarning(slug string, doc payload.Doc, shard *discovery.Shard, drafts *bool) *output.Warning {
	if drafts == nil || !*drafts || doc == nil {
		return nil
	}
	if status, _ := doc["_status"].(string); status != "draft" {
		return nil
	}
	missing := stillMissing(shard, doc)
	w := &output.Warning{
		Code: output.WarnCreatedAsDraft,
		Message: fmt.Sprintf("%s has drafts enabled and the new document is _status=draft, so it is not publicly visible. "+
			"--draft skipped required-field validation; publishing re-runs exactly that validation.", slug),
		StillMissing: missing,
	}
	id := doc.IDString()
	if len(missing) > 0 {
		w.Hint = fmt.Sprintf("Still missing: %s. `pay publish %s %s` will fail until they are set.",
			strings.Join(missing, ", "), slug, id)
	} else {
		w.Hint = fmt.Sprintf("`pay publish %s %s` promotes it once every required field is set.", slug, id)
	}
	return w
}

// stillMissing lists the collection's required paths that the document lacks.
func stillMissing(shard *discovery.Shard, doc payload.Doc) []string {
	if shard == nil || len(shard.RequiredPaths) == 0 || doc == nil {
		return nil
	}
	var out []string
	for _, path := range shard.RequiredPaths {
		v, ok := lookupPath(map[string]any(doc), path)
		if !ok || isEmptyValue(v) {
			out = append(out, path)
		}
	}
	return out
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// ---------------------------------------------------------------------------
// write plumbing: confirmation (§12.1) + audit (§12.7)
// ---------------------------------------------------------------------------

// writeOp bundles the confirm/audit lifecycle every L1-L3 command shares.
type writeOp struct {
	d      *Deps
	op     safety.Op
	target string
	global bool
	method string
	path   string
	where  json.RawMessage
	ids    []any
	run    *runStats
}

func (d *Deps) newWriteOp(op safety.Op, target, method, path string) *writeOp {
	return &writeOp{d: d, op: op, target: target, global: op.Global, method: method, path: path, run: d.beginRun()}
}

func (w *writeOp) withWhere(where query.Where) *writeOp {
	if where.IsEmpty() {
		return w
	}
	if s, err := where.JSON(); err == nil {
		w.where = json.RawMessage(s)
	}
	return w
}

func (w *writeOp) withIDs(ids []any) *writeOp {
	w.ids = ids
	return w
}

// confirm applies §12.1. affected is the resolved match count (or 1).
func (w *writeOp) confirm(affected int) error {
	return w.d.confirmer().Confirm(safety.Request{Op: w.op, Target: w.target, Affected: affected})
}

// event builds a record with the fields that are known before the call.
func (w *writeOp) event(phase audit.Phase, affected int) audit.Event {
	cfg := w.d.cfg()
	ev := audit.Event{
		Time:       w.d.now(),
		Phase:      phase,
		Command:    w.op.Command,
		Profile:    cfg.Profile,
		BaseURL:    cfg.BaseURL,
		Action:     w.op.Action(),
		Method:     w.method,
		Path:       w.path,
		Where:      w.where,
		IDs:        idStrings(w.ids),
		Affected:   affected,
		DryRun:     cfg.DryRun,
		DurationMS: 0,
	}
	if w.global {
		ev.Global = w.target
	} else {
		ev.Collection = w.target
	}
	return ev
}

// pre writes the §12.7 pre-record. A failure ABORTS the operation.
func (w *writeOp) pre(affected int) error {
	if w.d == nil || !w.op.Level().Audited() {
		return nil
	}
	return w.d.auditor().Pre(w.event(audit.PhasePre, affected))
}

// post writes the §12.7 post-record. A failure is a warning only.
func (w *writeOp) post(ok bool, status, affected, failed int, errMsg, requestID string) *output.Warning {
	if w.d == nil || !w.op.Level().Audited() {
		return nil
	}
	ev := w.event(audit.PhasePost, affected)
	ev.OK = ok
	ev.Status = status
	ev.Failed = failed
	ev.Err = errMsg
	ev.RequestID = requestID
	if start := w.run.start; !start.IsZero() {
		if end := w.d.now(); !end.IsZero() {
			ev.DurationMS = end.Sub(start).Milliseconds()
		}
	}
	return w.d.auditor().Post(ev)
}

// finish renders the terminal envelope after recording the post-write audit
// entry, so a committed write is never reported as a failure because the log
// was unwritable.
func (w *writeOp) finish(env *output.Envelope, resp *payload.Response, affected, failed int, errMsg string) (*output.Envelope, error) {
	status, requestID := 0, ""
	if resp != nil {
		status, requestID = resp.Status, resp.RequestID
	}
	if warn := w.post(env.OK, status, affected, failed, errMsg, requestID); warn != nil {
		env.AddWarning(*warn)
	}
	return env, nil
}

func idStrings(ids []any) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("%v", id))
	}
	return out
}

// ---------------------------------------------------------------------------
// pay create
// ---------------------------------------------------------------------------

type createFlags struct {
	data        writeData
	draft       bool
	autosave    bool
	depth       int
	selectF     []string
	locale      string
	file        string
	noEchoCheck bool
}

func init() { Register(newCreateCmd) }

func newCreateCmd(rt *Runtime) *cobra.Command {
	f := &createFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "create <collection>",
		Short:   "Create one document (POST /api/{collection})",
		Long: `Create a document.

--data, --data-file, --set and --set-json are COMBINABLE, not alternatives. They
are deep-merged in that order and the later flag wins on a key collision. Arrays
are replaced, never merged element-wise.

--set values are typed (null/true/false/number/string, "quoted" forces a string,
json: forces raw JSON) and then coerced against the collection's schema, because
Payload rejects {"heroImage":"4"} and accepts {"heroImage":4}.

--draft skips required-field validation; it is a deferral, not an escape.
Publishing later re-runs exactly the validation --draft skipped.

After the write PayCLI compares what it sent against what Payload echoed back:
Payload answers 201 "successfully created" while having silently discarded an
unknown field or an entire blocks array with an unrecognised blockType.

`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdCreate, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runCreate(ctx, d, f, args[0])
	})
	f.data.register(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&f.draft, "draft", false, "create as a draft, skipping required-field validation")
	fl.BoolVar(&f.autosave, "autosave", false, "mark the write as an autosave")
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.StringSliceVar(&f.selectF, "select", nil, "return only these fields")
	fl.StringVar(&f.locale, "locale", "", "write into this locale")
	fl.StringVar(&f.file, "file", "", "upload this file (upload collections only — prefer `pay upload`)")
	fl.BoolVar(&f.noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")

	SetHelp(cmd, createHelp())
	return cmd
}

// createHelp is §10.5's model for `pay create`.
func createHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay create <collection> [--set k=v ...] [--set-json k=JSON ...]",
			"                        [--data JSON] [--data-file FILE] [--draft] [--depth N]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
		},
		FlagInfo: map[string]FlagInfo{
			"set":       {Grammar: "key=value  (dotted keys nest: meta.title=…)", Repeatable: true},
			"set-json":  {Grammar: "key=JSON", Repeatable: true},
			"data":      {Grammar: "a JSON object"},
			"data-file": {Grammar: "a path, or - for stdin"},
			"depth":     {Min: intPtr(0), Max: intPtr(10)},
			"draft":     {Note: "skips required-field validation; only on a drafts-enabled collection"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":27,"title":"Pricing","slug":"pricing","_status":"draft","createdAt":"…"}`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "what would be sent, without sending it",
				Cmd: "pay create pages --set title='Pricing' --set slug=pricing --draft --dry-run"},
			{Why: "the required fields only — find them with `pay describe <coll> --required-only`",
				Cmd: "pay create pages --set title='Pricing' --set slug=pricing --draft"},
			{Why: "a relationship: --set-json keeps it a number, --set would send a string",
				Cmd: "pay create posts --set title='Launch' --set-json categories='[3]'"},
			{Why: "a whole document from a file, with two fields overridden",
				Cmd: "pay create pages --data-file ./page.json --set slug=pricing --set-json layout='[]'"},
			{Why: "nested fields without any JSON at all (dotted --set keys nest)",
				Cmd: "pay create posts --set title='Launch' --set meta.title='SEO title'"},
			{Why: "a collection with no drafts — --draft would fail with feature_unavailable",
				Cmd: "pay create crm-tasks --set title='Follow up'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating --data, --data-file, --set and --set-json as alternatives.",
				Right: "They are combinable and deep-merged in that order, later winning. Arrays are replaced whole, never merged element-wise."},
			{Wrong: "Sending a relationship id as a string (--set heroImage=4 reaching Payload as \"4\").",
				Right: "--set values are typed and then coerced against the schema, but for anything nested use --set-json heroImage=4 / --set-json categories='[3]'."},
			{Wrong: "Believing a 201 means everything you sent was stored.",
				Right: "Payload silently discards unknown fields and unrecognised blockType values. Read warnings[] for input_silently_dropped and value_normalized_by_server on every write."},
			{Wrong: "Using --draft to dodge validation permanently.",
				Right: "It is a deferral: publishing re-runs exactly the validation --draft skipped. Fix the fields, or keep the document as a draft deliberately."},
			{Wrong: "`pay create media --file ./hero.png` for an upload collection.",
				Right: "REST needs a multipart body. Use `pay upload media ./hero.png`, which builds it."},
		},
		SeeAlso: []string{
			"pay describe <collection> --required-only   # what must be set",
			"pay update <collection> <id>   # change an existing document",
			"pay upload <collection> <file>   # create an upload document",
			"pay publish <collection> <id>   # promote a draft",
		},
	}
}

func runCreate(ctx context.Context, d *Deps, f *createFlags, slug string) (*output.Envelope, error) {
	cfg := d.cfg()
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if f.draft {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	if f.file != "" {
		if err := payload.CheckUploadCollection(t.Slug, t.flags().Upload); err != nil {
			return nil, err
		}
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"`pay create --file` is not the upload path").
			WithHint("use `pay upload %s %s` — it builds the multipart body Payload requires", t.Slug, f.file)
	}

	body, warnings, err := f.data.build(d, t, t.Shard)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"create needs a document body").
			WithHint("pass --set k=v, --data '{...}' or --data-file body.json")
	}

	// §7.9a / conflict 35, same as `pay update`: POST echoes the created
	// document back, and with `locale=de` sent bare Payload's default
	// fallback:true fills every untranslated field with the DEFAULT locale's
	// text, which the agent then reads as a real translation. resolveLocale is
	// the one place that decides fallback-locale (and validates the code, and
	// honours the profile's `locale`), so the write verbs use it too.
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
	if f.autosave {
		p.Extra = urlValues("autosave", "true")
	}
	if err := query.ValidateSelect(f.selectF, t.schema(cfg)); err != nil {
		return nil, err
	}

	op := safety.Op{Command: safety.CmdCreate, Selector: safety.SelectorNone}
	w := d.newWriteOp(op, t.Slug, "POST", "/"+t.Slug)

	if cfg.DryRun {
		// The previewed URL carries the same query the real POST will: a
		// preview that hides ?locale=de&fallback-locale=none is not a preview.
		q, _ := p.Encode()
		env, err := emitDryRun(d, w, safety.CmdCreate, "POST", client.URLFor(&payload.Request{
			Method: "POST", Path: "/" + t.Slug, Query: q,
		}), body, 1, nil)
		if err != nil {
			return nil, err
		}
		env.Meta.Locale = localeMetaOf(p)
		return env, nil
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}

	res, err := client.Create(ctx, t.Slug, body, p,
		payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body))), payload.WithKnownRoute())
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		return nil, err
	}

	env := output.New(safety.CmdCreate, output.KindDoc, res.Doc).
		WithTarget(t.envTarget(res.Doc.ID())).
		WithChanged(&output.Changed{Created: 1, IDs: []any{res.Doc.ID()}}).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
		})
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	// echoWarnings, not echoDiff: --select is forwarded into the write's query
	// params and Payload honours it, so the echoed document legitimately omits
	// every non-selected field. echoDiff reported each one as
	// input_silently_dropped — the exact false positive §10.2 exists to
	// prevent — on a write that fully succeeded.
	for _, warn := range echoWarnings(t.Slug, body, res.Doc, f.selectF, echoConfig{
		Shard:     t.Shard,
		Ignore:    cfg.EchoCheckIgnore,
		LocaleAll: p.Locale == "all",
		Disabled:  f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	if warn := createdAsDraftWarning(t.Slug, res.Doc, t.Shard, t.flags().Drafts); warn != nil {
		env.AddWarning(*warn)
	}
	return w.withIDs([]any{res.Doc.ID()}).finish(env, res.HTTP, 1, 0, "")
}

func statusOf(res *payload.WriteResult) int {
	if res == nil || res.HTTP == nil {
		return 0
	}
	return res.HTTP.Status
}

// emitDryRun builds §12.2's op_result envelope.
func emitDryRun(d *Deps, w *writeOp, command, method, url string, body map[string]any, total int, ids []any) (*output.Envelope, error) {
	// A negative blast radius is a bug in the caller, not a preview:
	// NewDryRun would quietly turn it into len(ids), i.e. would_affect: 0 for
	// an unbounded write. Refuse rather than print a reassuring zero.
	if total < 0 {
		return nil, apierr.New(apierr.CodeInternal,
			"internal: dry run for `pay %s` computed a negative blast radius (%d)", command, total).
			WithHint("this is a PayCLI bug; re-run without --dry-run only after reporting it")
	}
	var raw json.RawMessage
	if len(body) > 0 {
		if b, err := json.Marshal(body); err == nil {
			raw = b
		}
	}
	// Route the preview through the confirmer as well. It cannot prompt or
	// fail under --dry-run (Confirm short-circuits every question), but it is
	// what prints the §12.4 IRREVERSIBLE note and the §12.1 resolved match
	// count on stderr — the disclosure the dry run exists to show, which no
	// dry-run path reached because they all return before w.confirm().
	if err := w.confirm(total); err != nil {
		return nil, err
	}
	res := safety.NewDryRun(safety.DryRunRequest{Method: method, URL: url, Body: raw}, total, ids, !d.cfg().Redact)
	env := res.Envelope(command, w.run.meta())
	if warn := w.post(true, 0, total, 0, "", ""); warn != nil {
		env.AddWarning(*warn)
	}
	return env, nil
}
