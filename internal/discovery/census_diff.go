package discovery

import (
	"fmt"
	"sort"
)

// CensusChange is one difference between two censuses of the same entity —
// what `pay blocks learn` reports so an agent sees "grid gained splitDesktop"
// instead of diffing two JSON dumps.
type CensusChange struct {
	Change    string `json:"change"` // added | removed | not_seen
	What      string `json:"what"`   // block_type | field | path | path_type
	Entity    string `json:"entity"`
	BlockType string `json:"block_type,omitempty"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
}

// DiffCensusEntity compares a previous and a new census of one entity. A nil
// previous census reports nothing: a first census has no history to diff.
//
// Absence only means "no longer used" when the new read looked everywhere. A
// read that was truncated (a smaller --census-max-docs; a collection larger
// than the cap, whose newest-first window moved) or failed did not, so what it
// lacks is reported as change "not_seen", never "removed".
func DiffCensusEntity(prev, next *CensusEntity) []CensusChange {
	if prev == nil || next == nil {
		return []CensusChange{}
	}
	name := next.Slug
	if next.Kind == EntityKindGlobal {
		name = "global:" + next.Slug
	}
	out := []CensusChange{}

	for _, s := range setDiff(typeKeys(next), typeKeys(prev)) {
		out = append(out, CensusChange{Change: "added", What: "block_type", Entity: name, BlockType: s,
			Message: fmt.Sprintf("%s: new blockType %q (%d row(s))", name, s, next.Types[s].Count)})
	}
	for _, s := range setDiff(typeKeys(prev), typeKeys(next)) {
		out = append(out, CensusChange{Change: "removed", What: "block_type", Entity: name, BlockType: s,
			Message: fmt.Sprintf("%s: blockType %q is no longer used", name, s)})
	}
	for _, p := range setDiff(pathKeys(next), pathKeys(prev)) {
		out = append(out, CensusChange{Change: "added", What: "path", Entity: name, Path: p,
			Message: fmt.Sprintf("%s: blocks now appear at %s", name, p)})
	}
	for _, p := range setDiff(pathKeys(prev), pathKeys(next)) {
		out = append(out, CensusChange{Change: "removed", What: "path", Entity: name, Path: p,
			Message: fmt.Sprintf("%s: no blocks remain at %s", name, p)})
	}
	for _, p := range sortedIntersect(pathKeys(prev), pathKeys(next)) {
		a, b := prev.Paths[p], next.Paths[p]
		for _, s := range setDiff(countKeys(b.Types), countKeys(a.Types)) {
			out = append(out, CensusChange{Change: "added", What: "path_type", Entity: name, Path: p, BlockType: s,
				Message: fmt.Sprintf("%s: %s now also holds %q", name, p, s)})
		}
		for _, s := range setDiff(countKeys(a.Types), countKeys(b.Types)) {
			out = append(out, CensusChange{Change: "removed", What: "path_type", Entity: name, Path: p, BlockType: s,
				Message: fmt.Sprintf("%s: %s no longer holds %q", name, p, s)})
		}
	}
	for _, s := range sortedIntersect(typeKeys(prev), typeKeys(next)) {
		was, now := fieldPaths(prev.Types[s].Fields, ""), fieldPaths(next.Types[s].Fields, "")
		for _, f := range setDiff(now, was) {
			out = append(out, CensusChange{Change: "added", What: "field", Entity: name, BlockType: s, Path: f,
				Message: fmt.Sprintf("%s gained %s", s, f)})
		}
		for _, f := range setDiff(was, now) {
			out = append(out, CensusChange{Change: "removed", What: "field", Entity: name, BlockType: s, Path: f,
				Message: fmt.Sprintf("%s lost %s", s, f)})
		}
	}
	if partialRead(next) {
		why := fmt.Sprintf("this read covered %d document(s)", next.DocsScanned)
		switch {
		case next.Error != "":
			why += " and failed"
		case next.Truncated && next.TotalDocs != nil:
			why += fmt.Sprintf(" of %d (truncated)", *next.TotalDocs)
		}
		why += fmt.Sprintf(", the previous one %d", prev.DocsScanned)
		for i := range out {
			if out[i].Change == "removed" {
				out[i].Change = "not_seen"
				out[i].Message = fmt.Sprintf("%s: %s was not seen in this partial read (%s); it may still be in use",
					name, changeSubject(out[i]), why)
			}
		}
	}
	if partialRead(prev) {
		for i := range out {
			if out[i].Change == "added" {
				out[i].Message += fmt.Sprintf(" (the previous read covered only %d document(s), so it may not be new)", prev.DocsScanned)
			}
		}
	}
	return out
}

// changeSubject names what a change is about, for a rephrased message.
func changeSubject(ch CensusChange) string {
	switch ch.What {
	case "block_type":
		return fmt.Sprintf("blockType %q", ch.BlockType)
	case "path":
		return "the blocks position " + ch.Path
	case "path_type":
		return fmt.Sprintf("blockType %q at %s", ch.BlockType, ch.Path)
	case "field":
		return fmt.Sprintf("field %s of %s", ch.Path, ch.BlockType)
	}
	return ch.What
}

func typeKeys(e *CensusEntity) map[string]bool {
	out := map[string]bool{}
	for k := range e.Types {
		out[k] = true
	}
	return out
}

func pathKeys(e *CensusEntity) map[string]bool {
	out := map[string]bool{}
	for k := range e.Paths {
		out[k] = true
	}
	return out
}

func countKeys(m map[string]int) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// fieldPaths flattens a field tree to `a`, `a.b`, `a[].c`.
func fieldPaths(fields map[string]*CensusField, prefix string) map[string]bool {
	out := map[string]bool{}
	for name, f := range fields {
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		out[p] = true
		sub := p
		if f.Kind == CensusKindArray {
			sub = p + "[]"
		}
		for k := range fieldPaths(f.Fields, sub) {
			out[k] = true
		}
	}
	return out
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sortedIntersect(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// partialRead reports a census that did not read every document: a truncated
// or failed read. A complete read with fewer documents than the last one is
// NOT partial — documents were deleted — and its absences are real.
func partialRead(e *CensusEntity) bool {
	return e.Truncated || e.Error != ""
}
