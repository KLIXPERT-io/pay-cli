package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// renderCSV writes RFC 4180 (§10.3).
//
// Default columns are the discovered top-level SCALAR fields only — never a
// richText blob, which would produce a multi-kilobyte cell of Lexical JSON.
// Explicit columns may be dotted paths, in which case nested objects are
// flattened to dotted columns, and any array value is JSON-encoded in-cell.
func renderCSV(w io.Writer, env *Envelope, columns []string) error {
	docs := docsOf(env.Data)
	rows := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		m, ok := d.(map[string]any)
		if !ok {
			// A bare scalar row (--select id) still deserves a column.
			m = map[string]any{"value": d}
		}
		rows = append(rows, m)
	}

	cols := columns
	if len(cols) == 0 {
		cols = defaultColumns(rows)
	}
	cw := csv.NewWriter(w)
	// RFC 4180 mandates CRLF line endings; Go's default is LF.
	cw.UseCRLF = true
	if err := cw.Write(cols); err != nil {
		return fmt.Errorf("output: write csv header: %w", err)
	}
	for _, row := range rows {
		flat := flatten(row, "")
		record := make([]string, len(cols))
		for i, c := range cols {
			record[i] = cellString(lookup(row, flat, c))
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("output: write csv row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// lookup prefers an exact top-level key, then the flattened dotted path, so a
// document that literally contains a key with a dot in it still works.
func lookup(row, flat map[string]any, col string) any {
	if v, ok := row[col]; ok {
		return v
	}
	return flat[col]
}

// defaultColumns picks the top-level scalar fields, in first-seen order with
// later documents' extra fields appended. Order is stable across runs, which
// matters because these files get diffed.
func defaultColumns(rows []map[string]any) []string {
	seen := map[string]bool{}
	var cols []string
	for _, row := range rows {
		for _, k := range SortedKeys(row) {
			if seen[k] || !isScalar(row[k]) {
				continue
			}
			seen[k] = true
			cols = append(cols, k)
		}
	}
	// "id" first when present: it is the column every consumer joins on.
	sort.SliceStable(cols, func(i, j int) bool { return cols[i] == "id" && cols[j] != "id" })
	if len(cols) == 0 {
		cols = []string{"id"}
	}
	return cols
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64, float32, int, int64, json.Number:
		return true
	default:
		return false
	}
}

// flatten turns nested objects into dotted keys. Arrays are NOT descended into:
// §10.3 says arrays are JSON-encoded in-cell.
func flatten(m map[string]any, prefix string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			for nk, nv := range flatten(nested, key) {
				out[nk] = nv
			}
			continue
		}
		out[key] = v
	}
	return out
}

// cellString renders one cell. Scalars are printed plainly; everything else is
// compact JSON so the cell stays machine-readable.
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := marshalCompact(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return strings.TrimSpace(string(b))
	}
}
