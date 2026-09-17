package output

import (
	"fmt"
	"io"
)

// renderJSONL writes one BARE document per line on stdout and the envelope —
// page, next, meta, warnings, and error when there is one — as a single JSON
// line on stderr (§10.3). That split is what makes
//
//	pay find pages --all --output jsonl > out.jsonl 2> summary.json
//
// yield a clean data file and a clean summary at the same time.
func renderJSONL(stdout, stderr io.Writer, env *Envelope) error {
	for _, doc := range docsOf(env.Data) {
		b, err := marshalCompact(doc)
		if err != nil {
			return fmt.Errorf("output: encode document: %w", err)
		}
		if err := writeLine(stdout, b); err != nil {
			return err
		}
	}
	summary := env.summary()
	b, err := marshalCompact(summary)
	if err != nil {
		return fmt.Errorf("output: encode summary: %w", err)
	}
	return writeLine(stderr, b)
}

// summary is the envelope minus data: the same object with Data cleared, so
// the stderr line has exactly the keys an agent already knows.
func (e *Envelope) summary() *Envelope {
	cp := *e
	cp.Data = nil
	cp.RawBody = nil
	return &cp
}

// docsOf normalises Data into a slice of documents. A bare object (data_kind
// "doc" or "global") is one line; a scalar is one line; nil is no lines.
func docsOf(data any) []any {
	switch t := generic(data).(type) {
	case nil:
		return nil
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, d := range t {
			out[i] = d
		}
		return out
	default:
		return []any{data}
	}
}
