// Package redact removes credentials from everything PayCLI emits: stdout,
// stderr, log lines, audit records, cache files and the manifest (§5.3).
//
// Two rules drive the whole package:
//
//  1. Payload returns credentials inside ordinary documents. GET
//     /api/{auth}/me hands back apiKey, hash and salt in plaintext, and a bulk
//     write against the auth collection echoes every touched user, so any
//     response body may contain a secret at any depth.
//  2. error.raw and --output raw must stay byte-faithful *after* redaction
//     (§11.1). JSON is therefore spliced, not re-encoded: when nothing matched,
//     the caller gets the original bytes back with zero normalisation, and when
//     something did match only the matched value ranges are replaced. Key
//     order, indentation, number formatting and duplicate keys all survive.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Mask replaces every redacted value.
const Mask = "<redacted>"

// maskJSON is Mask as a JSON string literal, spliced over redacted values.
const maskJSON = `"` + Mask + `"`

// secretKeys are the Payload fields that carry credentials verbatim (§5.3).
// Matched case-insensitively against the exact key name.
var secretKeys = map[string]struct{}{
	"apikey":                  {},
	"apikeyindex":             {},
	"hash":                    {},
	"salt":                    {},
	"password":                {},
	"sessions":                {},
	"resetpasswordtoken":      {},
	"resetpasswordexpiration": {},
}

// secretKeySubstring is §5.3's catch-all: "any key matching (?i)token|secret".
// It is deliberately a substring match, so refreshToken, token_exp,
// client_secret and payload-secret are all covered.
var secretKeySubstring = regexp.MustCompile(`(?i)token|secret`)

// jwtLike matches a whole string that is shaped like a JWT. Anchored and
// requiring the "eyJ" base64url prefix of `{"` keeps it from eating ordinary
// dotted content such as a slug or a version number.
var jwtLike = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*$`)

// nonSecretKeys are the keys §5.3 matches by substring but also requires
// PayCLI to PRINT. `token_exp` is the only one: §5.3's own bullet says
// `pay auth status` reports it and §4.4 stores it in credentials.json, and an
// expiry timestamp is not a credential — masking it hid the one fact that
// explains why a stored token stopped working.
var nonSecretKeys = map[string]struct{}{
	"token_exp": {}, "tokenexp": {}, "token_expiry": {}, "token_expires_at": {},
}

// Key reports whether a JSON object key (or a query-parameter name) names a
// value that must never be emitted.
func Key(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if _, ok := secretKeys[lower]; ok {
		return true
	}
	if _, ok := nonSecretKeys[lower]; ok {
		return false
	}
	return secretKeySubstring.MatchString(name)
}

// IsJWT reports whether s is shaped like a JSON Web Token.
func IsJWT(s string) bool { return jwtLike.MatchString(s) }

// Result is the outcome of redacting a JSON document.
//
// Data is the original slice when Changed is false — literally the same bytes,
// so `--output raw` and error.raw stay byte-faithful on the overwhelmingly
// common path where there was nothing to hide. Paths lists the redacted
// locations in "docs[0].apiKey" form for the raw_redacted warning (§11.1).
type Result struct {
	Data    []byte
	Changed bool
	Paths   []string
}

// Scrubber adds known literal secret values (the resolved API key, a minted
// JWT) to the structural rules. The zero Scrubber applies the structural rules
// only and is what the package-level functions use.
type Scrubber struct {
	// Literals are exact strings to replace wherever they appear. Empty and
	// very short entries are ignored, because replacing a 1-character literal
	// would shred unrelated output.
	Literals []string
}

// minLiteral is the shortest literal Scrubber will act on. Payload API keys are
// UUIDs and JWTs are far longer; anything shorter is more likely to be a false
// positive than a credential.
const minLiteral = 8

// JSON redacts secret values inside a JSON document, preserving every byte it
// did not have to change. A body that is not valid JSON (an HTML error page,
// a truncated response) falls back to text scrubbing rather than being dropped.
func JSON(b []byte) Result { return Scrubber{}.JSON(b) }

// Text scrubs credentials out of free-form text: log lines, server messages,
// error strings and command echoes.
func Text(s string) string { return Scrubber{}.Text(s) }

// Value redacts an already-decoded JSON-ish value in place-by-copy. It is the
// entry point for cache and audit writers that hold a map rather than bytes.
// The returned paths use the same notation as Result.Paths.
func Value(v any) (any, []string) { return Scrubber{}.Value(v) }

// JSON implements the splice-based redaction described in the package doc.
func (s Scrubber) JSON(b []byte) Result {
	if len(bytes.TrimSpace(b)) == 0 {
		return Result{Data: b}
	}
	spans, paths, err := s.scanJSON(b)
	if err != nil {
		// Not JSON (or not parseable as JSON): fall back to text scrubbing so a
		// credential in an HTML error page or a truncated body is still caught.
		out := s.Text(string(b))
		if out == string(b) {
			return Result{Data: b}
		}
		return Result{Data: []byte(out), Changed: true, Paths: []string{"<body>"}}
	}
	if len(spans) == 0 {
		return Result{Data: b}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out bytes.Buffer
	out.Grow(len(b))
	prev := 0
	for _, sp := range spans {
		if sp.start < prev {
			continue // overlapping span; the outer one already covered it
		}
		out.Write(b[prev:sp.start])
		out.WriteString(maskJSON)
		prev = sp.end
	}
	out.Write(b[prev:])

	// Literal secrets can also appear under innocent key names; sweep the
	// result once more for them.
	final := s.replaceLiterals(out.String())
	sort.Strings(paths)
	return Result{Data: []byte(final), Changed: true, Paths: paths}
}

// Text scrubs credentials out of free-form text.
func (s Scrubber) Text(in string) string {
	out := s.replaceLiterals(in)
	out = textJWT.ReplaceAllString(out, Mask)
	out = textURLUserinfo.ReplaceAllString(out, "${1}"+Mask+"@")
	out = textHeaderLine.ReplaceAllString(out, "${1}${2}"+Mask)
	out = textAssignment.ReplaceAllString(out, "${1}${2}"+Mask)
	return out
}

// Value redacts a decoded value (map[string]any, []any, scalars).
func (s Scrubber) Value(v any) (any, []string) {
	var paths []string
	out := s.redactValue(v, "", &paths)
	sort.Strings(paths)
	return out, paths
}

func (s Scrubber) redactValue(v any, path string, paths *[]string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			p := join(path, k)
			if Key(k) {
				out[k] = Mask
				*paths = append(*paths, p)
				continue
			}
			out[k] = s.redactValue(val, p, paths)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = s.redactValue(val, fmt.Sprintf("%s[%d]", path, i), paths)
		}
		return out
	case string:
		if IsJWT(t) || s.containsLiteral(t) {
			*paths = append(*paths, path)
			return Mask
		}
		return t
	default:
		return v
	}
}

// --- text patterns -------------------------------------------------------

var (
	// A JWT anywhere inside a larger string.
	textJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*`)
	// scheme://userinfo@ — the capture keeps the scheme, the userinfo is masked.
	textURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s@"']+@`)
	// A secret *header* line: the value runs to the end of the line, because
	// "Authorization: users API-Key K" contains spaces.
	textHeaderLine = regexp.MustCompile(
		`(?i)\b(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api[_-]?key)(["']?\s*:\s*)[^\r\n]*`)
	// name=value / "name": value where the name is a secret name. The optional
	// quote in group 2 lets this match JSON-ish text as well as flags.
	textAssignment = regexp.MustCompile(
		`(?i)\b(apikey|api[_-]?key|password|[a-z0-9_-]*token|[a-z0-9_-]*secret)(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&")']+)`)
)

func (s Scrubber) replaceLiterals(in string) string {
	out := in
	for _, lit := range s.Literals {
		if len(lit) < minLiteral {
			continue
		}
		out = strings.ReplaceAll(out, lit, Mask)
	}
	return out
}

func (s Scrubber) containsLiteral(in string) bool {
	for _, lit := range s.Literals {
		if len(lit) >= minLiteral && strings.Contains(in, lit) {
			return true
		}
	}
	return false
}

// --- JSON scanning -------------------------------------------------------

type span struct{ start, end int }

// scanJSON walks b with a streaming decoder and records the byte range of every
// value that must be replaced. Nothing is re-encoded, so the caller's bytes are
// preserved outside the recorded ranges.
func (s Scrubber) scanJSON(b []byte) ([]span, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var (
		spans []span
		paths []string
	)
	if err := s.walk(dec, b, "", &spans, &paths, 0); err != nil {
		return nil, nil, err
	}
	// Reject trailing garbage so "{}<html>" is treated as non-JSON.
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("redact: trailing data after JSON value")
	}
	return spans, paths, nil
}

// maxDepth guards against a hostile or pathological body blowing the stack.
const maxDepth = 200

func (s Scrubber) walk(dec *json.Decoder, b []byte, path string, spans *[]span, paths *[]string, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("redact: JSON nested deeper than %d levels", maxDepth)
	}
	before := dec.InputOffset()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			for dec.More() {
				keyTok, kerr := dec.Token()
				if kerr != nil {
					return kerr
				}
				key, ok := keyTok.(string)
				if !ok {
					return errors.New("redact: non-string object key")
				}
				afterKey := dec.InputOffset()
				p := join(path, key)
				if Key(key) {
					var raw json.RawMessage
					if derr := dec.Decode(&raw); derr != nil {
						return derr
					}
					start := valueStart(b, int(afterKey))
					end := int(dec.InputOffset())
					// Redaction must be idempotent: a value that is already the
					// mask has nothing left to hide, and re-recording it would
					// report Changed (and a warning) on a body no pass will
					// alter again.
					if !alreadyMasked(b, start, end) {
						*spans = append(*spans, span{start: start, end: end})
						*paths = append(*paths, p)
					}
					continue
				}
				if werr := s.walk(dec, b, p, spans, paths, depth+1); werr != nil {
					return werr
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return err
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if werr := s.walk(dec, b, fmt.Sprintf("%s[%d]", path, i), spans, paths, depth+1); werr != nil {
					return werr
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return err
			}
		default:
			return fmt.Errorf("redact: unexpected delimiter %q", t)
		}
	case string:
		if IsJWT(t) || s.containsLiteral(t) {
			start := valueStart(b, int(before))
			*spans = append(*spans, span{start: start, end: int(dec.InputOffset())})
			*paths = append(*paths, path)
		}
	}
	return nil
}

// alreadyMasked reports that the bytes in [start,end) are exactly the mask
// literal, i.e. a previous redaction pass already replaced this value.
func alreadyMasked(b []byte, start, end int) bool {
	if start < 0 || end > len(b) || start >= end {
		return false
	}
	return string(bytes.TrimSpace(b[start:end])) == maskJSON
}

// valueStart advances past the structural bytes (whitespace, a ':' after a key,
// a ',' after a previous element) that sit between the recorded offset and the
// first byte of the value itself.
func valueStart(b []byte, from int) int {
	for from < len(b) {
		switch b[from] {
		case ' ', '\t', '\r', '\n', ':', ',':
			from++
		default:
			return from
		}
	}
	return from
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
