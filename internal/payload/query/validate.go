package query

import (
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Schema is the slice of the manifest the query layer needs. Every slice is
// tri-state: a nil slice means "PayCLI never learned this", and §9.3's rule is
// that an unknown fact is never a local rejection — the request is sent and a
// *_unknown warning is attached by the caller. An empty non-nil slice means
// "known to be empty" and does reject.
type Schema struct {
	// Collection is the slug, for error messages only.
	Collection string
	// Queryable lists field paths that may appear in a where clause.
	Queryable []string
	// Sortable lists field paths that may appear in --sort.
	Sortable []string
	// Selectable lists field paths that may appear in --select.
	Selectable []string
	// DateFields lists fields with payload_type "date".
	DateFields []string
	// DBAdapter / DBAdapterSource gate the adapter-specific operators.
	DBAdapter       string
	DBAdapterSource string
}

// known reports whether a set was learned at all.
func known(set []string) bool { return set != nil }

// ExtractPaths returns every field path a where clause queries, sorted and
// de-duplicated. `and` and `or` are structure, not fields, so they are walked
// through rather than reported.
func ExtractPaths(w Where) []string {
	seen := map[string]bool{}
	walkWhere(w, func(path string, _ string, _ any) {
		seen[path] = true
	})
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ExtractOperators returns every operator used in a where clause, sorted.
func ExtractOperators(w Where) []string {
	seen := map[string]bool{}
	walkWhere(w, func(_ string, op string, _ any) {
		seen[op] = true
	})
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// walkWhere visits every leaf term of a where tree.
func walkWhere(v any, visit func(path, op string, value any)) {
	switch t := v.(type) {
	case Where:
		walkWhere(map[string]any(t), visit)
	case map[string]any:
		for k, child := range t {
			if k == "and" || k == "or" {
				walkWhere(child, visit)
				continue
			}
			ops, ok := child.(map[string]any)
			if !ok {
				// A degenerate {"field": "value"} shape: no operator to report.
				visit(k, "", child)
				continue
			}
			for op, val := range ops {
				visit(k, op, val)
			}
		}
	case []any:
		for _, item := range t {
			walkWhere(item, visit)
		}
	}
}

// ValidateWhere checks every queried path and operator against the schema.
func ValidateWhere(w Where, s Schema) error {
	if w.IsEmpty() {
		return nil
	}
	if known(s.Queryable) {
		for _, p := range ExtractPaths(w) {
			if pathKnown(p, s.Queryable) {
				continue
			}
			return apierr.New(apierr.CodeQueryPathInvalid,
				"%q cannot be queried%s", p, onCollection(s.Collection)).
				WithDidYouMean(apierr.DidYouMean(p, s.Queryable)...).
				WithHint("%s", "run `pay describe "+orSlug(s.Collection)+" --queryable` for the list, "+
					"or pass --no-validate-where to send it anyway")
		}
	}
	for _, op := range ExtractOperators(w) {
		if op == "" {
			continue
		}
		if !IsOperator(op) {
			// A --where-json body may contain anything; catch a typo here
			// rather than letting the server ignore the whole clause.
			return apierr.New(apierr.CodeUnsupportedOperator, "%q is not a Payload operator", op).
				WithDidYouMean(apierr.DidYouMean(op, Operators)...).
				WithHint("%s", "operators: "+strings.Join(Operators, " "))
		}
		if err := CheckOperator(op, s.DBAdapter, s.DBAdapterSource); err != nil {
			return err
		}
	}
	return nil
}

// pathKnown accepts an exact match, and accepts a dotted path when its root
// segment is a known field — `company.name` is a legal 2-hop relational path
// that the manifest lists only as `company` (verified working live).
func pathKnown(path string, set []string) bool {
	for _, k := range set {
		if k == path {
			return true
		}
	}
	root, _, dotted := strings.Cut(path, ".")
	if !dotted {
		return false
	}
	for _, k := range set {
		if k == root {
			return true
		}
	}
	return false
}

// ValidateSort rejects an unknown sort field. The server's behaviour here is
// the worst kind: `sort=bogusField` returns HTTP 200, silently unsorted.
func ValidateSort(fields []string, s Schema) error {
	if !known(s.Sortable) {
		return nil
	}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name := strings.TrimPrefix(f, "-")
		if pathKnown(name, s.Sortable) {
			continue
		}
		return apierr.New(apierr.CodeInvalidSortField,
			"%q is not a sortable field%s", name, onCollection(s.Collection)).
			WithDidYouMean(apierr.DidYouMean(name, s.Sortable)...).
			WithHint("%s", "Payload answers an unknown sort field with HTTP 200 and no sorting at all, so PayCLI "+
				"rejects it here; --no-validate-sort sends it anyway")
	}
	return nil
}

// ValidateSelect rejects an unknown --select key, which the server answers
// with HTTP 200 and a document containing only `id`.
func ValidateSelect(fields []string, s Schema) error {
	if !known(s.Selectable) {
		return nil
	}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || pathKnown(f, s.Selectable) {
			continue
		}
		return apierr.New(apierr.CodeUnknownField,
			"%q is not a field%s", f, onCollection(s.Collection)).
			WithDidYouMean(apierr.DidYouMean(f, s.Selectable)...).
			WithHint("an unknown --select key makes Payload return only `id`, with HTTP 200")
	}
	return nil
}

// ValidateDateField enforces §9.4's --date-field rule.
func ValidateDateField(field string, s Schema) error {
	if field == "" || !known(s.DateFields) {
		return nil
	}
	for _, f := range s.DateFields {
		if f == field {
			return nil
		}
	}
	return apierr.New(apierr.CodeUnknownField,
		"--date-field %q is not a date field%s", field, onCollection(s.Collection)).
		WithDidYouMean(apierr.DidYouMean(field, s.DateFields)...).
		WithHint("%s", "date fields: "+strings.Join(s.DateFields, ", "))
}

// ValidateLimit rejects `--limit 0`, which Payload reads as unlimited — an
// agent that means "no documents" would get every document instead.
func ValidateLimit(limit *int) error {
	if limit == nil {
		return nil
	}
	switch {
	case *limit < 0:
		return apierr.New(apierr.CodeInvalidArgs, "--limit must be positive; got %d", *limit)
	case *limit == 0:
		return apierr.New(apierr.CodeInvalidArgs, "--limit 0 means *unlimited* to Payload, not zero documents").
			WithHint("use --count-only for a count, or --all --max N to page through everything")
	}
	return nil
}

func onCollection(slug string) string {
	if slug == "" {
		return ""
	}
	return " on " + slug
}

func orSlug(slug string) string {
	if slug == "" {
		return "<collection>"
	}
	return slug
}
