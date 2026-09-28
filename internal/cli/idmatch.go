package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// canonicalID validates a document id (checkID) and returns the form every
// later step must use: for a collection with numeric ids, the decimal the
// server itself reports (`0108` and ` 108` become `108`).
//
// Payload casts the id in the URL path and in a `where id in` query, so a
// non-canonical numeric id still reaches the right document. PayCLI's own
// bookkeeping does not: the pre-write backup, the dry-run diff sample and
// the publish pre-validation pair the documents a batched read returned
// with the ids the caller asked for by their string form, so `0108` never
// matched the returned `108` — the backup was skipped while the write
// (delete --permanent included) went through. Canonicalizing once, at
// validation, keeps the path, the envelope, the backup and the audit on the
// same id. String and unknown id types are returned unchanged: `0108` and
// `108` are two different documents there.
func (d *Deps) canonicalID(t *collTarget, slug, id string) (string, *apierr.Error) {
	if e := d.checkID(t, slug, id); e != nil {
		return "", e
	}
	if t.idType() == apierr.IDTypeNumber {
		if n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err == nil {
			return strconv.FormatInt(n, 10), nil
		}
	}
	return id, nil
}

// docsByID indexes the documents a `where id in [...]` read returned so the
// caller can pair them with the ids it asked for, whatever form those ids
// took. Exact string matches win; a numeric request id (`0108`) also
// matches a document whose id is a JSON NUMBER with the same value. A
// string-typed id never matches numerically — `0108` and `108` are two
// documents in a collection with string ids.
type docsByID struct {
	exact   map[string]payload.Doc
	numeric map[string]string // canonical decimal -> exact key, number-typed ids only
}

func newDocsByID() *docsByID {
	return &docsByID{exact: map[string]payload.Doc{}, numeric: map[string]string{}}
}

func (x *docsByID) add(doc payload.Doc) {
	key := doc.IDString()
	x.exact[key] = doc
	if isNumericID(doc.ID()) {
		if n, ok := parseNumericID(key); ok {
			x.numeric[n] = key
		}
	}
}

// lookup returns the document for a requested id and the key it is stored
// under (the server's own rendering of its id).
func (x *docsByID) lookup(id any) (payload.Doc, string, bool) {
	raw := fmt.Sprint(id)
	if doc, ok := x.exact[raw]; ok {
		return doc, raw, true
	}
	if n, ok := parseNumericID(raw); ok {
		if key, ok := x.numeric[n]; ok {
			return x.exact[key], key, true
		}
	}
	return nil, "", false
}

// isNumericID reports whether a decoded id value is a JSON number.
func isNumericID(v any) bool {
	switch v.(type) {
	case json.Number, float64, float32, int, int64, int32:
		return true
	}
	return false
}

// parseNumericID is the canonical decimal of an integer id string, tolerant
// of the leading zeros and surrounding whitespace Payload's cast accepts.
func parseNumericID(s string) (string, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}
