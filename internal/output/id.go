package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// renderID writes one id per line and nothing else (§10.3) — the format shell
// pipelines consume. On a write it prints changed.ids, because that is the set
// the command actually affected; otherwise it prints each document's id.
func renderID(w io.Writer, env *Envelope) error {
	ids := envelopeIDs(env)
	for _, id := range ids {
		if _, err := io.WriteString(w, idString(id)+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func envelopeIDs(env *Envelope) []any {
	if env.Changed != nil && len(env.Changed.IDs) > 0 {
		return env.Changed.IDs
	}
	var out []any
	for _, doc := range docsOf(env.Data) {
		if m, ok := doc.(map[string]any); ok {
			if id, ok := m["id"]; ok {
				out = append(out, id)
			}
			continue
		}
		// A bare scalar in data (an id list produced by --select id) is already
		// an id.
		if doc != nil {
			out = append(out, doc)
		}
	}
	if len(out) == 0 && env.Target != nil && env.Target.ID != nil {
		out = append(out, env.Target.ID)
	}
	return out
}

// idString prints an id without JSON quoting and without scientific notation.
// Payload ids are integers on a relational adapter and strings on Mongo, and a
// float-formatted "1.1e+01" in a shell pipeline is a silent wrong answer.
func idString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
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
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
