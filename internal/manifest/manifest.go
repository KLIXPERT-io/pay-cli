// Package manifest reads the item files `pay plan` and `pay sync` execute
// (F6) and implements the pure half of the {"$ref": …} grammar every write
// body accepts: parsing a reference, finding every reference in a body,
// replacing them, and ordering plan items so a document is created before
// the items that point at it.
//
// Nothing here talks to Payload. Resolving a reference to an id, matching an
// item against the server and diffing it are the command layer's job
// (internal/cli/plan.go, refs.go); this package only decides what the files
// say and whether that is well formed, so every rule is a table test.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Status is an item's "status": what the write does to the document's
// publication state on a drafts-enabled collection or global.
type Status string

const (
	// StatusKeep (the default) leaves the publication state alone: a
	// document whose newest version is published is saved published (its
	// live content changes), one whose newest version is a draft gets a new
	// draft — including a published document with a pending draft, whose live
	// version stays as it is (warning saved_as_draft) — and a new document is
	// created as a draft.
	StatusKeep Status = "keep"
	// StatusDraft saves the change as a draft (?draft=true). It never
	// unpublishes: a published document keeps its live version and gets a
	// draft on top.
	StatusDraft Status = "draft"
	// StatusPublished writes _status "published": the result is live, and
	// Payload runs full validation.
	StatusPublished Status = "published"
)

// DefaultMatch is the match field an item without "match" uses.
const DefaultMatch = "slug"

// Item is one entry of a manifest: one document or one global, and the data
// it must hold.
type Item struct {
	// Key names the item for "@key" references and in every report. It is
	// the manifest's "key", or derived: "<collection>/<match values…>" for a
	// collection item, "globals/<slug>" for a global.
	Key string `json:"key"`
	// KeyExplicit says the manifest set Key itself.
	KeyExplicit bool `json:"-"`
	// Collection or Global — exactly one is set.
	Collection string `json:"collection,omitempty"`
	Global     string `json:"global,omitempty"`
	// Match are the fields whose values (taken from Data) identify the
	// document. Collections only; default [DefaultMatch].
	Match []string `json:"match,omitempty"`
	// Status is keep, draft or published.
	Status Status `json:"status"`
	// Locale is the locale the item is matched, diffed and written in; ""
	// means the command's --locale / the profile's default.
	Locale string `json:"locale,omitempty"`
	// Data is the document body. It may hold {"$ref": …} objects.
	Data map[string]any `json:"-"`
	// File is the manifest file the item came from ("-" for stdin), and
	// Index its position in that file (0 for a single-object file).
	File  string `json:"file"`
	Index int    `json:"index"`
	// Multi says the file holds an array, so the item's label carries its
	// index.
	Multi bool `json:"-"`
	// FromEnvelope says the file was a PayCLI `pay get` / `pay globals get`
	// envelope rather than a manifest item.
	FromEnvelope bool `json:"-"`
}

// Label is how an item is named where it came from: "dir/a.json" or
// "dir/list.json#2".
func (it *Item) Label() string {
	if it.Multi {
		return fmt.Sprintf("%s#%d", it.File, it.Index)
	}
	return it.File
}

// Target is "pages" or "global footer", for messages.
func (it *Item) Target() string {
	if it.Global != "" {
		return "global " + it.Global
	}
	return it.Collection
}

// MatchValues returns the match fields and their values from Data, in
// Match order. ok is false when a value is missing, null or not a scalar.
func (it *Item) MatchValues() (map[string]any, []string, error) {
	out := make(map[string]any, len(it.Match))
	for _, f := range it.Match {
		v, ok := Lookup(it.Data, f)
		if !ok || v == nil {
			return nil, nil, fmt.Errorf("match field %q has no value in \"data\" (the value to match on comes from the body)", f)
		}
		if !IsScalar(v) {
			return nil, nil, fmt.Errorf("match field %q must be a string, number or boolean in \"data\", not %s (a $ref cannot be matched on; write the id)", f, kindOf(v))
		}
		out[f] = v
	}
	return out, it.Match, nil
}

// Lookup reads a possibly dotted key ("meta.title") from a body.
func Lookup(body map[string]any, key string) (any, bool) {
	var cur any = body
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// IsScalar reports a JSON string, number or boolean.
func IsScalar(v any) bool {
	switch v.(type) {
	case string, json.Number, float64, int, int64, bool:
		return true
	}
	return false
}

// ScalarString renders a scalar for a key or a message.
func ScalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	}
	return "a scalar"
}

// itemKeys are the keys a manifest item may carry. Any key starting with "$"
// ("$schema", "$comment") is ignored, so a manifest can document itself.
var itemKeys = map[string]bool{
	"key": true, "collection": true, "global": true, "match": true,
	"status": true, "locale": true, "data": true,
}

// Problem is one thing wrong with a manifest, named by the item it concerns.
type Problem struct {
	// Where is the item label ("dir/a.json#1") or file.
	Where string
	// Code is the apierr code this problem would raise on its own.
	Code apierr.Code
	// Message says what is wrong.
	Message string
	// Hint says what to do about it ("" = the code's default).
	Hint string
}

// Problems is a manifest that cannot be planned. It converts to one
// manifest_invalid error naming every problem.
type Problems []Problem

// Err renders the problems as one error (nil when there are none). The code
// is the problems' common code when they share one, else manifest_invalid.
func (ps Problems) Err() error {
	if len(ps) == 0 {
		return nil
	}
	code := ps[0].Code
	for _, p := range ps[1:] {
		if p.Code != code {
			code = apierr.CodeManifestInvalid
			break
		}
	}
	failures := make([]apierr.Failure, 0, len(ps))
	for _, p := range ps {
		failures = append(failures, apierr.Failure{ID: p.Where, Code: p.Code, Message: p.Message})
	}
	msg := ps[0].Where + ": " + ps[0].Message
	if len(ps) > 1 {
		msg = fmt.Sprintf("%d manifest problems; the first: %s", len(ps), msg)
	}
	e := apierr.New(code, "%s", msg).WithFailures(failures...)
	// One hint for all of them when they agree; otherwise the list is the
	// answer, and the hint says so.
	hint := ps[0].Hint
	for _, p := range ps[1:] {
		if p.Hint != hint {
			hint = ""
			break
		}
	}
	switch {
	case hint != "":
		e = e.WithHint("%s", hint)
	case len(ps) > 1 || code != apierr.CodeManifestInvalid:
		e = e.WithHint("%s", "error.failures names every item and what is wrong with it; fix them all, then re-run the same `pay plan`")
	}
	return e
}

// fileHint is the hint for a manifest argument that cannot be read.
const fileHint = "name manifest files or directories that exist; a directory contributes every *.json file below it, - reads stdin"

// Load reads every item from the given paths. A directory contributes every
// *.json file below it (recursively, in lexical order, skipping names that
// start with "."); "-" reads one manifest from stdin. The result is in load
// order with keys derived and uniqueness checked. Every problem is collected
// before failing, so one run names all of them.
func Load(paths []string, stdin io.Reader) ([]*Item, error) {
	if len(paths) == 0 {
		return nil, apierr.New(apierr.CodeInvalidArgs, "name at least one manifest file or directory").
			WithHint("e.g. `pay plan ./content` (every *.json below it) or `pay plan page.json footer.json`")
	}
	var (
		items    []*Item
		problems Problems
		stdinUse bool
	)
	for _, p := range paths {
		if p == "-" {
			if stdinUse {
				problems = append(problems, Problem{Where: "-", Code: apierr.CodeInvalidArgs, Message: "stdin (-) can be read only once"})
				continue
			}
			stdinUse = true
			b, err := io.ReadAll(stdin)
			if err != nil {
				problems = append(problems, Problem{Where: "-", Code: apierr.CodeManifestInvalid, Message: "stdin could not be read: " + err.Error()})
				continue
			}
			got, probs := parseFile("-", b)
			items = append(items, got...)
			problems = append(problems, probs...)
			continue
		}
		files, err := expand(p)
		if err != nil {
			problems = append(problems, Problem{Where: p, Code: apierr.CodeFileMissing, Message: err.Error(), Hint: fileHint})
			continue
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				problems = append(problems, Problem{Where: f, Code: apierr.CodeFileMissing, Message: "could not be read: " + err.Error(), Hint: fileHint})
				continue
			}
			got, probs := parseFile(f, b)
			items = append(items, got...)
			problems = append(problems, probs...)
		}
	}
	// Keys and duplicates are checked over the items that did parse, so one
	// run reports those problems together with the parse errors.
	problems = append(problems, finish(items)...)
	if err := problems.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// expand turns one argument into the files it names.
func expand(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("no such file or directory")
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != path && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(name), ".json") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not be listed: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("the directory holds no *.json manifest file")
	}
	sort.Strings(out)
	return out, nil
}

// parseFile decodes one file: an item object, an array of items, or a
// PayCLI envelope of one document or global.
func parseFile(file string, b []byte) ([]*Item, Problems) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid, Message: "the file is empty"}}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid, Message: "not valid JSON: " + err.Error()}}
	}
	if dec.More() {
		return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid, Message: "holds more than one JSON value; use one object or one array of items"}}
	}
	switch t := v.(type) {
	case []any:
		var (
			items    []*Item
			problems Problems
		)
		if len(t) == 0 {
			return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid, Message: "the array holds no items"}}
		}
		for i, raw := range t {
			obj, ok := raw.(map[string]any)
			where := fmt.Sprintf("%s#%d", file, i)
			if !ok {
				problems = append(problems, Problem{Where: where, Code: apierr.CodeManifestInvalid,
					Message: "an item must be a JSON object, not " + kindOf(raw)})
				continue
			}
			it, probs := parseItem(obj, file, i, true)
			if it != nil {
				items = append(items, it)
			}
			problems = append(problems, probs...)
		}
		return items, problems
	case map[string]any:
		if isEnvelope(t) {
			return fromEnvelope(t, file)
		}
		it, probs := parseItem(t, file, 0, false)
		if it == nil {
			return nil, probs
		}
		return []*Item{it}, probs
	}
	return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid,
		Message: "a manifest file holds one item object or an array of them, not " + kindOf(v)}}
}

// isEnvelope is the §9.11.6 four-key detection.
func isEnvelope(m map[string]any) bool {
	for _, k := range []string{"ok", "v", "data_kind", "meta"} {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// fromEnvelope turns a `pay get <collection> <id>`, `pay globals get <slug>`
// or `pay find <collection>` envelope into items that say "these documents
// should look like this": the collection (or global) comes from target, the
// match is the default, the status is keep.
func fromEnvelope(env map[string]any, file string) ([]*Item, Problems) {
	bad := func(msg string) ([]*Item, Problems) {
		return nil, Problems{{Where: file, Code: apierr.CodeManifestInvalid, Message: msg}}
	}
	if ok, _ := env["ok"].(bool); !ok {
		return bad("is a PayCLI error envelope, not a document")
	}
	// §9.12.8: a read with --richtext md|text replaced every editor state by
	// a string; syncing it back would overwrite each richText field with
	// text the admin editor cannot open.
	if meta, _ := env["meta"].(map[string]any); meta != nil {
		if rt, _ := meta["richtext"].(map[string]any); rt != nil {
			if format, _ := rt["format"].(string); format == "md" || format == "text" {
				return nil, Problems{{Where: file, Code: apierr.CodeRichTextRendered,
					Message: fmt.Sprintf("is a PayCLI envelope read with --richtext %s: its rich text is strings, not Lexical editor states", format),
					Hint:    "re-export it without --richtext (the default is json): pay get <collection> <id> --depth 0 --draft"}}
			}
		}
	}
	target, _ := env["target"].(map[string]any)
	slug, _ := target["slug"].(string)
	kind, _ := env["data_kind"].(string)
	if slug == "" {
		return bad(fmt.Sprintf("is a PayCLI %q envelope without target.slug", kind))
	}
	switch kind {
	case "doc":
		data, ok := env["data"].(map[string]any)
		if !ok {
			return bad("is a PayCLI doc envelope whose data is not one document")
		}
		if p := populatedProblem(data, file); p != nil {
			return nil, Problems{*p}
		}
		return []*Item{{File: file, Status: StatusKeep, FromEnvelope: true, Collection: slug,
			Match: envelopeMatch(data), Data: cloneMap(data)}}, nil
	case "global":
		data, ok := env["data"].(map[string]any)
		if !ok {
			return bad("is a PayCLI global envelope whose data is not one document")
		}
		if p := populatedProblem(data, file); p != nil {
			return nil, Problems{*p}
		}
		return []*Item{{File: file, Status: StatusKeep, FromEnvelope: true, Global: slug, Data: cloneMap(data)}}, nil
	case "doc_list":
		list, _ := env["data"].([]any)
		if len(list) == 0 {
			return bad("is a PayCLI doc_list envelope with no documents")
		}
		items := make([]*Item, 0, len(list))
		var problems Problems
		for i, raw := range list {
			doc, ok := raw.(map[string]any)
			if !ok {
				problems = append(problems, Problem{Where: fmt.Sprintf("%s#%d", file, i), Code: apierr.CodeManifestInvalid,
					Message: "a listed document is not an object"})
				continue
			}
			if p := populatedProblem(doc, fmt.Sprintf("%s#%d", file, i)); p != nil {
				problems = append(problems, *p)
				continue
			}
			items = append(items, &Item{File: file, Index: i, Multi: true, Status: StatusKeep, FromEnvelope: true,
				Collection: slug, Match: envelopeMatch(doc), Data: cloneMap(doc)})
		}
		return items, problems
	}
	return bad(fmt.Sprintf("is a PayCLI %q envelope; only `pay get` (doc), `pay find` (doc_list) and `pay globals get` (global) envelopes hold documents", kind))
}

// envelopeMatch is how an exported document is matched: on its slug — the
// natural key, the same on every server, so an export from one server syncs
// into another — and on its id when it has no slug (a collection without
// one: the id is all an export carries). A match on id only ever updates:
// the plan refuses the create it would otherwise lead to.
func envelopeMatch(doc map[string]any) []string {
	if v, ok := doc[DefaultMatch]; ok && v != nil {
		return []string{DefaultMatch}
	}
	if v, ok := doc["id"]; ok && IsScalar(v) {
		return []string{"id"}
	}
	return []string{DefaultMatch}
}

// populatedProblem refuses an exported document read at --depth 1 or more:
// its relationships are whole documents (an object with an id and Payload's
// timestamps) where a write needs the id, so syncing it would plan every
// relationship as a change and write the expansion back.
func populatedProblem(doc map[string]any, where string) *Problem {
	var paths []string
	var walk func(v any, path string, depth int)
	walk = func(v any, path string, depth int) {
		if len(paths) >= 5 || depth > 32 {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			if path != "" && looksPopulated(t) {
				paths = append(paths, path)
				return
			}
			if isEditorState(t) {
				// Rich text is stored as written: a block node's upload
				// may hold the whole document even at depth 0 (seen on
				// klixpert stage), and it round-trips unchanged.
				return
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(t[k], p, depth+1)
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s[%d]", path, i), depth+1)
			}
		}
	}
	walk(doc, "", 0)
	if len(paths) == 0 {
		return nil
	}
	return &Problem{Where: where, Code: apierr.CodeManifestInvalid,
		Message: fmt.Sprintf("is a PayCLI envelope read above --depth 0: %s hold(s) whole related documents where a write needs their ids",
			strings.Join(paths, ", ")),
		Hint: "re-export it at depth 0: pay get <collection> <id> --depth 0 --draft (pay find … --depth 0 --draft for a list)"}
}

// isEditorState is a Lexical editor state: {"root": {"type": "root", …}}.
func isEditorState(obj map[string]any) bool {
	root, ok := obj["root"].(map[string]any)
	return ok && root["type"] == "root"
}

// looksPopulated is an expanded relationship: an object with an id and one of
// the timestamps Payload puts on every document (a row has an id but no
// timestamps; a rich-text node has neither).
func looksPopulated(obj map[string]any) bool {
	if _, ok := obj["id"]; !ok {
		return false
	}
	_, created := obj["createdAt"]
	_, updated := obj["updatedAt"]
	return created || updated
}

// parseItem validates one item object.
func parseItem(obj map[string]any, file string, index int, multi bool) (*Item, Problems) {
	it := &Item{File: file, Index: index, Multi: multi, Status: StatusKeep}
	where := it.Label()
	var problems Problems
	bad := func(format string, args ...any) {
		problems = append(problems, Problem{Where: where, Code: apierr.CodeManifestInvalid, Message: fmt.Sprintf(format, args...)})
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "$") || itemKeys[k] {
			continue
		}
		bad("unknown key %q (an item has key, collection|global, match, status, locale and data; keys starting with $ are comments)%s",
			k, suggest(k))
	}
	str := func(key string) string {
		v, ok := obj[key]
		if !ok || v == nil {
			return ""
		}
		s, ok := v.(string)
		if !ok {
			bad("%q must be a string, not %s", key, kindOf(v))
			return ""
		}
		return strings.TrimSpace(s)
	}
	it.Collection = str("collection")
	it.Global = str("global")
	it.Locale = str("locale")
	if k := str("key"); k != "" {
		switch {
		case strings.HasPrefix(k, "@"):
			bad("\"key\" %q must not start with @ (the @ belongs to the reference: {\"$ref\":\"@%s\"})", k, strings.TrimPrefix(k, "@"))
		case strings.ContainsAny(k, " \t\n"):
			bad("\"key\" %q must not contain whitespace", k)
		default:
			it.Key, it.KeyExplicit = k, true
		}
	}
	switch {
	case it.Collection == "" && it.Global == "":
		bad("names no target: set \"collection\" (a document) or \"global\"")
	case it.Collection != "" && it.Global != "":
		bad("sets both \"collection\" and \"global\"; an item is one or the other")
	}
	switch st := str("status"); st {
	case "", string(StatusKeep):
		it.Status = StatusKeep
	case string(StatusDraft), string(StatusPublished):
		it.Status = Status(st)
	default:
		bad("\"status\" %q is not draft, published or keep", st)
	}
	if raw, ok := obj["match"]; ok && raw != nil {
		if it.Global != "" {
			bad("\"match\" is for collection items; a global is always exactly one document")
		}
		switch m := raw.(type) {
		case string:
			it.Match = splitMatch(m)
		case []any:
			for _, f := range m {
				s, ok := f.(string)
				if !ok || strings.TrimSpace(s) == "" {
					bad("\"match\" must be a field name or a list of field names")
					break
				}
				it.Match = append(it.Match, strings.TrimSpace(s))
			}
		default:
			bad("\"match\" must be a field name or a list of field names, not %s", kindOf(raw))
		}
		if len(it.Match) == 0 && it.Global == "" {
			bad("\"match\" is empty")
		}
	} else if it.Collection != "" {
		it.Match = []string{DefaultMatch}
	}
	seen := map[string]bool{}
	for _, f := range it.Match {
		if seen[f] {
			bad("\"match\" names %q twice", f)
		}
		seen[f] = true
	}
	switch d := obj["data"].(type) {
	case map[string]any:
		it.Data = d
	case nil:
		if _, ok := obj["data"]; ok {
			bad("\"data\" is null; it must be an object holding the document's fields")
		} else {
			bad("has no \"data\" (the document's fields)")
		}
	default:
		bad("\"data\" must be an object, not %s", kindOf(obj["data"]))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return it, nil
}

func splitMatch(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// suggest names the item key a typo was probably meant to be.
func suggest(k string) string {
	best, bestD := "", 3
	for cand := range itemKeys {
		if d := levenshtein(strings.ToLower(k), cand); d < bestD {
			best, bestD = cand, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; did you mean %q?", best)
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// finish derives missing keys and rejects duplicates: two items with the
// same key, or two items that name the same document (same collection, same
// match fields and values, same locale) or the same global.
func finish(items []*Item) Problems {
	var problems Problems
	for _, it := range items {
		if it.KeyExplicit {
			continue
		}
		if it.Global != "" {
			it.Key = "globals/" + it.Global
			continue
		}
		vals, fields, err := it.MatchValues()
		if err != nil {
			problems = append(problems, Problem{Where: it.Label(), Code: apierr.CodeManifestInvalid, Message: err.Error()})
			continue
		}
		parts := []string{it.Collection}
		for _, f := range fields {
			parts = append(parts, ScalarString(vals[f]))
		}
		it.Key = strings.Join(parts, "/")
		if it.Locale != "" {
			it.Key += "@" + it.Locale
		}
	}
	byKey := map[string]*Item{}
	byTarget := map[string]*Item{}
	for _, it := range items {
		if it.Key == "" {
			continue // its key could not be derived; already reported
		}
		if prev, ok := byKey[it.Key]; ok {
			problems = append(problems, Problem{Where: it.Label(), Code: apierr.CodeManifestInvalid,
				Message: fmt.Sprintf("key %q is already used by %s; keys must be unique (set \"key\" explicitly)", it.Key, prev.Label())})
			continue
		}
		byKey[it.Key] = it
		target := "global\x00" + it.Global + "\x00" + it.Locale
		if it.Collection != "" {
			vals, fields, err := it.MatchValues()
			if err != nil {
				problems = append(problems, Problem{Where: it.Label(), Code: apierr.CodeManifestInvalid, Message: err.Error()})
				continue
			}
			var b strings.Builder
			b.WriteString("coll\x00" + it.Collection + "\x00" + it.Locale)
			for _, f := range fields {
				b.WriteString("\x00" + f + "=" + ScalarString(vals[f]))
			}
			target = b.String()
		}
		if prev, ok := byTarget[target]; ok {
			problems = append(problems, Problem{Where: it.Label(), Code: apierr.CodeManifestInvalid,
				Message: fmt.Sprintf("writes the same %s as %s; one document is described by one item", documentWord(it), prev.Label())})
			continue
		}
		byTarget[target] = it
	}
	return problems
}

func documentWord(it *Item) string {
	if it.Global != "" {
		return "global"
	}
	return "document"
}

// cloneMap is a shallow copy; values are never mutated in place by this
// package.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
