package cache

import "github.com/KLIXPERT-io/pay-cli/internal/redact"

// censusJSON is canonicalJSON for the block census (§7.10c), with a
// census-shaped redaction instead of the document-shaped one.
//
// canonicalJSON's redact.JSON masks the whole VALUE of any secret-looking KEY,
// which is right for a document and wrong for the census: the census keys its
// maps by schema NAMES — block field names (`hash`, `csrfToken`,
// `hubspotFormToken`), blockType slugs, path patterns, entity slugs — and the
// values under them are structure (a field profile, a count). Masking them
// turned a CensusField object into the string "<redacted>", the file no
// longer decoded, and the census was silently rebuilt by every command that
// needed it.
//
// What the census must never persist is a secret VALUE, and the only values it
// stores are the observed scalar samples (`values`) and short strings. So:
//   - under a secret-looking key an object keeps its structure but loses its
//     `values` (that field's observed samples), and a string is masked;
//   - numbers and booleans (counts, flags) are kept everywhere;
//   - a JWT-shaped string anywhere is masked.
//
// internal/discovery already skips value sampling for secret-looking field
// names; this is the defence in depth at the single write choke point.
func censusJSON(v any) ([]byte, error) {
	decoded, err := decodeCanonical(v)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(scrubCensus(decoded, false))
}

func scrubCensus(v any, secretKey bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKey && k == "values" {
				continue
			}
			out[k] = scrubCensus(val, redact.Key(k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrubCensus(val, false)
		}
		return out
	case string:
		if secretKey || redact.IsJWT(t) {
			return redact.Mask
		}
		return t
	default:
		return v
	}
}
