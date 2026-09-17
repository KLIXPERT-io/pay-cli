package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// indent is the envelope's pretty-print indent. Two spaces keeps a large
// doc_list readable without inflating an agent's token bill.
const indent = "  "

// marshalIndent renders v with HTML escaping DISABLED.
//
// encoding/json escapes <, > and & into <, > and & by default.
// That would mangle every "<redacted>" sentinel, every "<UNKNOWN>" provenance
// marker and every where-clause URL in meta, turning readable output into
// escape soup for an audience that is not a browser.
func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalCompact is marshalIndent on one line.
func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// generic converts a typed payload into the plain map/slice/scalar shapes every
// renderer and --path walks.
//
// A command is free to put a payload.Doc, a []payload.Doc or a struct in .data,
// and a NAMED map type does not satisfy a `map[string]any` type assertion.
// Without this conversion --output csv prints a header and no rows, --output id
// prints Go syntax, and --path fails with invalid_path_expr on data the
// envelope plainly rendered as an object. Numbers decode through json.Number,
// so a 19-digit document id survives the trip.
func generic(v any) any {
	// The fast path is a DEEP check, not a top-level type assertion. A
	// map[string]any whose values are typed ([]string, a struct, a named map)
	// is the shape every capability/schema command builds by hand, and
	// returning it unconverted is what made `--path '.a[]'` and `.a[0]` fail
	// on explain/collections/describe/access/auth list/config list while the
	// same forms worked on wire-decoded documents. Wire-decoded data is
	// already generic all the way down, so it still skips the round trip.
	if isGeneric(v) {
		return v
	}
	b, err := marshalCompact(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return out
}

// isGeneric reports whether v is ALREADY built only from the shapes --path and
// the csv/table/id renderers walk, recursively. It allocates nothing, so it is
// cheaper than the marshal round trip it lets a large doc_list skip.
func isGeneric(v any) bool {
	switch t := v.(type) {
	case nil, string, bool, json.Number:
		return true
	case map[string]any:
		for _, e := range t {
			if !isGeneric(e) {
				return false
			}
		}
		return true
	case []any:
		for _, e := range t {
			if !isGeneric(e) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// renderJSON writes one pretty envelope object, the default format for agents.
func renderJSON(w io.Writer, env *Envelope) error {
	b, err := env.MarshalIndentTo()
	if err != nil {
		return fmt.Errorf("output: encode envelope: %w", err)
	}
	return writeLine(w, b)
}

func writeLine(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
