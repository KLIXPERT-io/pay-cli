package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// ---------------------------------------------------------------------------
// "<redacted>" in write bodies (§9.15.2)
// ---------------------------------------------------------------------------
//
// A document read through PayCLI carries "<redacted>" wherever a key looks
// secret (apiKey, hash, token…). The get → edit → write loop sends that body
// back, and writing the mask would replace the real value with the string
// "<redacted>" — stage page 35 stores exactly that inside its rich text,
// left by an earlier export and write. Every write path therefore handles a
// masked value before anything is sent, and HOW depends on how Payload
// stores the value around it:
//
//   - MERGED values — a top-level field, a group key, a key of an array or
//     block row: the key is left out of the body. Payload merges what a PATCH
//     leaves out (a top-level field, a group key, a row matched by its id), so
//     the stored value stays; a new document or a new row gets the default.
//     Warning redacted_not_sent.
//   - WHOLESALE values — a Lexical editor state (anywhere) or a json field:
//     Payload stores them whole, so a key left out of one is DELETED. The
//     masked value is put back from the document being written (the same
//     place in its stored value) — the write then keeps it exactly. Only when
//     there is nothing to take it from (a create, a bulk write, a node the
//     stored value does not have) is the key removed, with warning
//     redacted_removed saying so.

// Warning codes this file raises.
const (
	// WarnRedactedNotSent says masked values in merged fields were left out
	// of the body.
	WarnRedactedNotSent = "redacted_not_sent"
	// WarnRedactedRemoved says masked keys inside rich text or json had no
	// stored value to be restored from and were removed from the value.
	WarnRedactedRemoved = "redacted_removed"
)

// wholesaleFn reports a value Payload stores whole (it never merges into
// it): a Lexical editor state anywhere, or a json field of the schema. path
// is in docdiff notation.
type wholesaleFn func(path string, v any) bool

// wholesaleValues is the predicate for an entity with the given schema.
func wholesaleValues(shard *discovery.Shard) wholesaleFn {
	jsonPaths := map[string]bool{}
	if shard != nil {
		for _, f := range shard.Fields {
			if f.PayloadType == discovery.TypeJSON && f.Path != "" {
				jsonPaths[f.Path] = true
			}
		}
	}
	return func(path string, v any) bool {
		if lexical.IsEditorState(v) {
			return true
		}
		return len(jsonPaths) > 0 && jsonPaths[schemaPath(path)]
	}
}

// schemaPath drops the indexes of a docdiff path: "hero.links[0].link.url"
// → "hero.links.link.url", the form Shard.Fields uses.
func schemaPath(p string) string {
	var b strings.Builder
	for _, seg := range splitDocPath(p) {
		if seg.index >= 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(seg.key)
	}
	return b.String()
}

func isMask(v any) bool {
	s, ok := v.(string)
	return ok && s == redact.Mask
}

// containsMask reports a "<redacted>" string anywhere in v.
func containsMask(v any) bool {
	switch t := v.(type) {
	case string:
		return t == redact.Mask
	case map[string]any:
		for _, e := range t {
			if containsMask(e) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if containsMask(e) {
				return true
			}
		}
	}
	return false
}

// withoutMasked removes every MERGED key whose value is the mask (see the
// file comment) and does not descend into wholesale values (whole may be
// nil: nothing is wholesale). dropped names the removed keys; inWhole
// reports masked values left inside wholesale values, for restoreMasked.
// body is never modified.
func withoutMasked(body map[string]any, whole wholesaleFn) (out map[string]any, dropped []string, inWhole bool) {
	var walk func(v any, path string) (any, bool)
	walk = func(v any, path string) (any, bool) {
		switch t := v.(type) {
		case map[string]any:
			var out map[string]any
			for _, k := range sortedMapKeys(t) {
				p := docdiff.JoinKey(path, k)
				if isMask(t[k]) {
					if out == nil {
						out = cloneShallow(t)
					}
					delete(out, k)
					dropped = append(dropped, p)
					continue
				}
				if whole != nil && whole(p, t[k]) {
					if containsMask(t[k]) {
						inWhole = true
					}
					continue
				}
				if nv, changed := walk(t[k], p); changed {
					if out == nil {
						out = cloneShallow(t)
					}
					out[k] = nv
				}
			}
			if out == nil {
				return v, false
			}
			return out, true
		case []any:
			var out []any
			for i, e := range t {
				if nv, changed := walk(e, docdiff.JoinIndex(path, i)); changed {
					if out == nil {
						out = append([]any(nil), t...)
					}
					out[i] = nv
				}
			}
			if out == nil {
				return v, false
			}
			return out, true
		}
		return v, false
	}
	res, _ := walk(body, "")
	m, _ := res.(map[string]any)
	return m, dropped, inWhole
}

// restoreMasked puts every masked value inside a wholesale value back from
// stored, the document the write changes (nil for a create): the value at
// the same place of the stored rich text or json. Rows are paired by id
// (else by index, same block type) on the way down; inside a wholesale
// value, by index, and only between nodes of the same Lexical type. A
// masked key with no stored counterpart is removed. Neither argument is
// modified.
func restoreMasked(stored, body map[string]any, whole wholesaleFn) (out map[string]any, restored, removed []string) {
	r := &maskRestorer{whole: whole}
	res, _ := r.value(stored, body, "", false)
	m, _ := res.(map[string]any)
	return m, r.restored, r.removed
}

type maskRestorer struct {
	whole             wholesaleFn
	restored, removed []string
}

// value walks body next to its stored counterpart (nil when there is none);
// inside marks a wholesale value.
func (r *maskRestorer) value(stored, body any, path string, inside bool) (any, bool) {
	switch b := body.(type) {
	case map[string]any:
		s, _ := stored.(map[string]any)
		if s != nil && !sameNode(s, b) {
			s = nil
		}
		var out map[string]any
		set := func(k string, v any) {
			if out == nil {
				out = cloneShallow(b)
			}
			out[k] = v
		}
		for _, k := range sortedMapKeys(b) {
			v := b[k]
			p := docdiff.JoinKey(path, k)
			in := inside || (r.whole != nil && r.whole(p, v))
			if !in && !containsMask(v) {
				continue
			}
			if in && isMask(v) {
				if sv, ok := s[k]; ok && s != nil {
					set(k, sv)
					r.restored = append(r.restored, p)
					continue
				}
				if out == nil {
					out = cloneShallow(b)
				}
				delete(out, k)
				r.removed = append(r.removed, p)
				continue
			}
			var sv any
			if s != nil {
				sv = s[k]
			}
			if nv, changed := r.value(sv, v, p, in); changed {
				set(k, nv)
			}
		}
		if out == nil {
			return body, false
		}
		return out, true
	case []any:
		s, _ := stored.([]any)
		var out []any
		for i, e := range b {
			if !containsMask(e) {
				continue
			}
			nv, changed := r.value(pairStored(s, e, i, inside), e, docdiff.JoinIndex(path, i), inside)
			if !changed {
				continue
			}
			if out == nil {
				out = append([]any(nil), b...)
			}
			out[i] = nv
		}
		if out == nil {
			return body, false
		}
		return out, true
	}
	return body, false
}

// pairStored is the stored element a body element corresponds to: the row
// with the same id, else the element at the same index.
func pairStored(stored []any, e any, i int, inside bool) any {
	if !inside {
		if id, ok := rowIDOf(e); ok {
			for _, sr := range stored {
				if sid, ok := rowIDOf(sr); ok && sid == id {
					return sr
				}
			}
			return nil
		}
	}
	if i < len(stored) {
		return stored[i]
	}
	return nil
}

// sameNode pairs two objects only when they are the same kind of thing: the
// same Lexical node type, the same block type.
func sameNode(stored, body map[string]any) bool {
	for _, k := range []string{"type", "blockType"} {
		bv, bok := body[k].(string)
		sv, sok := stored[k].(string)
		if bok && sok && bv != sv {
			return false
		}
	}
	return true
}

// maskedWarning reports masked values left out of merged fields: paths for
// one body, or the item keys and the distinct paths for a plan.
func maskedWarning(paths, keys []string) output.Warning {
	const effect = "left out of the write: a merged field (a top-level field, a group key, a row matched by its id) keeps " +
		"its stored value, and a new document or row gets the field's default"
	w := output.Warning{
		Code: WarnRedactedNotSent,
		Message: fmt.Sprintf("%d value(s) are %q — masked by the PayCLI read the body was exported from — and are %s",
			len(paths), redact.Mask, effect),
		Paths: capPaths(paths),
		Hint:  "to write such a field, put the real value in the body (read it with --no-redact)",
	}
	if len(keys) > 0 {
		w.Message = fmt.Sprintf("%d item(s) hold %q values (%s) — masked by the PayCLI read they were exported from — which are %s",
			len(keys), redact.Mask, strings.Join(capPaths(paths), ", "), effect)
		w.Paths = capPaths(keys)
		w.Hint = "to write such a field, put the real value in the item's data (read it with --no-redact)"
	}
	return w
}

// maskRemovedWarning reports masked keys inside rich text or json that had
// no stored value to be restored from.
func maskRemovedWarning(paths, keys []string) output.Warning {
	const effect = "Payload stores rich text and json whole, and there is no stored value at that place to restore " +
		"(a new document, a bulk write, or a node the stored value does not have), so the key is REMOVED from the value " +
		"and is gone after the write"
	w := output.Warning{
		Code: WarnRedactedRemoved,
		Message: fmt.Sprintf("%d key(s) inside rich text or json hold %q (masked by the PayCLI read the body was exported from); %s",
			len(paths), redact.Mask, effect),
		Paths: capPaths(paths),
		Hint:  "to keep the real value, export with --no-redact (pay get … --no-redact) or write it into the body",
	}
	if len(keys) > 0 {
		w.Message = fmt.Sprintf("%d item(s) hold %q inside rich text or json (%s); %s",
			len(keys), redact.Mask, strings.Join(capPaths(paths), ", "), effect)
		w.Paths = capPaths(keys)
		w.Hint = "to keep the real value, export with --no-redact (pay get … --no-redact) or write it into the item's data"
	}
	return w
}

// ---------------------------------------------------------------------------
// the write verbs' body preparation
// ---------------------------------------------------------------------------

// writeScope is what a write verb knows about the body's target.
type writeScope struct {
	shard *discovery.Shard
	// preimage reads the document the write changes (the newest version,
	// depth 0). nil for a create or a bulk write: nothing to restore from.
	// It is only called when a masked value sits inside rich text or json.
	preimage func(ctx context.Context) (map[string]any, error)
}

// prepareWriteBody is the one call create, update, apply, globals update,
// upload and the body form of `pay diff` make on a body before it is
// previewed or sent: masked values (the file comment), references
// (resolveBodyRefs, checked against relationTo) and relationship values whose
// id cannot belong to a numeric-id collection.
func (d *Deps) prepareWriteBody(ctx context.Context, sc writeScope, body map[string]any, locale string) (map[string]any, []output.Warning, error) {
	var warnings []output.Warning
	whole := wholesaleValues(sc.shard)
	body, dropped, inWhole := withoutMasked(body, whole)
	if len(dropped) > 0 {
		warnings = append(warnings, maskedWarning(dropped, nil))
	}
	body, refWarn, err := d.resolveBodyRefs(ctx, sc.shard, body, locale)
	if err != nil {
		return nil, nil, err
	}
	if refWarn != nil {
		warnings = append(warnings, *refWarn)
	}
	if inWhole {
		var current map[string]any
		if sc.preimage != nil {
			// A failed read restores nothing: the keys are removed and the
			// warning says so, instead of writing the mask.
			if doc, err := sc.preimage(ctx); err == nil {
				current = doc
			}
		}
		var removed []string
		body, _, removed = restoreMasked(current, body, whole)
		if len(removed) > 0 {
			warnings = append(warnings, maskRemovedWarning(removed, nil))
		}
	}
	if w := d.relationIDWarning(body); w != nil {
		warnings = append(warnings, *w)
	}
	return body, warnings, nil
}

// WarnRelationIDNotNumeric says a {relationTo, value} pair names a document
// of a numeric-id collection by a value that is not a number: a Markdown link
// written as [text](pay:pages/about), where "about" is a slug.
const WarnRelationIDNotNumeric = "relation_id_not_numeric"

// relationIDWarning finds {relationTo, value} objects — a Lexical link's
// fields.doc, an upload or relationship node, a polymorphic relationship —
// whose value is a non-numeric string while the collection's ids are numbers
// (Postgres, SQLite). Payload stores such a link without complaint, and it
// never resolves. No request: the id types come from the discovery manifest.
func (d *Deps) relationIDWarning(body map[string]any) *output.Warning {
	if d == nil || d.Manifest == nil {
		return nil
	}
	var paths []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			if rel, ok := t["relationTo"].(string); ok {
				if s, ok := t["value"].(string); ok && s != "" && !isAllDigitsString(s) {
					if c, ok := d.Manifest.Collection(rel); ok && c.IDType == discovery.IDTypeNumber {
						paths = append(paths, docdiff.JoinKey(path, "value"))
					}
				}
			}
			for _, k := range sortedMapKeys(t) {
				walk(t[k], docdiff.JoinKey(path, k))
			}
		case []any:
			for i, e := range t {
				walk(e, docdiff.JoinIndex(path, i))
			}
		}
	}
	walk(body, "")
	if len(paths) == 0 {
		return nil
	}
	return &output.Warning{
		Code: WarnRelationIDNotNumeric,
		Message: fmt.Sprintf("%d link(s) or relationship(s) name a document by a value that is not a number (%s), and that collection's ids are "+
			"numbers: Payload stores the value, and the link never resolves (a Markdown [text](pay:pages/about) link names the page by id, not slug)",
			len(paths), strings.Join(capPaths(paths), ", ")),
		Paths: capPaths(paths),
		Hint:  "write the id, or name the document by slug: [text](pay:pages/slug=about) in Markdown, {\"$ref\":\"pages\",\"slug\":\"about\"} in JSON",
	}
}
