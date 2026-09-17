package config

import (
	"sort"
	"strings"
)

// Interpolate expands ${NAME} references in a config value against env
// (§4.2's `"X-Vercel-Protection-Bypass" = "${VERCEL_BYPASS}"`).
//
// Deliberately minimal, because this string can end up in an HTTP header and
// in §8.1's headersFP:
//
//   - ${NAME} only. A bare $NAME is left alone, so a literal dollar in a token
//     survives untouched.
//   - $${NAME} escapes to the literal ${NAME}.
//   - An unset (or set-but-empty, §4.5) variable expands to "" and its name is
//     returned in missing, so the caller can warn instead of silently sending
//     an empty header.
//
// The returned missing slice is sorted and de-duplicated.
func Interpolate(s string, env Env) (string, []string) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	missingSet := map[string]bool{}

	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '$' && i+2 < len(s) && s[i+2] == '{' {
			// $${…} -> literal ${…}
			b.WriteByte('$')
			i += 2
			continue
		}
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				// Unterminated: emit verbatim rather than eating the rest.
				b.WriteString(s[i:])
				break
			}
			name := s[i+2 : i+2+end]
			value, ok := env.Lookup(name)
			if !ok {
				missingSet[name] = true
			}
			b.WriteString(value)
			i += 2 + end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}

	if len(missingSet) == 0 {
		return b.String(), nil
	}
	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return b.String(), missing
}

// InterpolateMap expands every value of a header map. Keys are never
// interpolated: a header *name* built from the environment would make §8.1's
// headersFP and the redaction allow-list unpredictable.
func InterpolateMap(in map[string]string, env Env) (map[string]string, []string) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	missingSet := map[string]bool{}
	for k, v := range in {
		expanded, missing := Interpolate(v, env)
		out[k] = expanded
		for _, name := range missing {
			missingSet[name] = true
		}
	}
	if len(missingSet) == 0 {
		return out, nil
	}
	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return out, missing
}
