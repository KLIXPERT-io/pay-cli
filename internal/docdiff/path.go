package docdiff

import (
	"regexp"
	"strconv"
	"strings"
)

// Paths use the notation PayCLI's echo-diff (§10.2) and `pay blocks` already
// print: dotted keys, bracketed indices — `layout[2].blocks[1].title`.

// JoinKey appends an object key to a path.
func JoinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// JoinIndex appends an array index to a path.
func JoinIndex(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}

// RootField is the top-level key a path starts with ("layout" for
// "layout[2].title"), or "" for the empty path.
func RootField(path string) string {
	end := len(path)
	if i := strings.IndexAny(path, ".["); i >= 0 {
		end = i
	}
	return path[:end]
}

// pattern is one compiled IgnorePaths/LeafPaths entry.
type pattern struct {
	re *regexp.Regexp
}

var indexRE = regexp.MustCompile(`\[(\d+|\*|)\]`)

// compilePatterns turns path patterns into anchored regular expressions. `[]`
// and `[*]` match any index; `[3]` matches exactly index 3; a pattern also
// matches everything below the path it names.
func compilePatterns(raw []string) []pattern {
	out := make([]pattern, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "."))
		if p == "" {
			continue
		}
		var b strings.Builder
		b.WriteString("^")
		last := 0
		for _, m := range indexRE.FindAllStringSubmatchIndex(p, -1) {
			b.WriteString(regexp.QuoteMeta(p[last:m[0]]))
			idx := p[m[2]:m[3]]
			if idx == "" || idx == "*" {
				b.WriteString(`\[\d+\]`)
			} else {
				b.WriteString(`\[` + idx + `\]`)
			}
			last = m[1]
		}
		b.WriteString(regexp.QuoteMeta(p[last:]))
		b.WriteString(`(?:$|[.\[])`)
		out = append(out, pattern{re: regexp.MustCompile(b.String())})
	}
	return out
}

func matchAny(ps []pattern, path string) bool {
	for _, p := range ps {
		if p.re.MatchString(path) {
			return true
		}
	}
	return false
}
