package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// seeded runs a command against the recorded manifest with no network at all:
// the cache is warm, the credential is anonymous and http.DefaultTransport
// panics (see TestMain).
func seeded(t *testing.T, args ...string) cliResult {
	t.Helper()
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	return cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(testBaseURL)})
}

func TestExplainServesTheWarmCacheOffline(t *testing.T) {
	res := seeded(t, "explain")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	for _, key := range []string{
		"connection", "collection_slugs", "global_slugs", "collections",
		"globals", "commands", "query_syntax", "exit_codes", "gotchas",
	} {
		if _, ok := d[key]; !ok {
			t.Errorf("data.%s missing from the default tier", key)
		}
	}
	if res.Env["data_kind"] != "capabilities" {
		t.Errorf("data_kind = %v", res.Env["data_kind"])
	}
}

// TestExplainNeverTruncatesTheSlugList is §18's hard rule.
func TestExplainNeverTruncatesTheSlugList(t *testing.T) {
	for _, tier := range [][]string{
		{"explain"},
		{"explain", "--slim"},
		{"explain", "--section", "collections"},
		{"explain", "--section", "collections", "--offset", "40"},
		{"explain", "--section", "exit_codes"},
		{"explain", "--section", "gotchas"},
	} {
		t.Run(strings.Join(tier, " "), func(t *testing.T) {
			res := seeded(t, tier...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			slugs, ok := res.data(t)["collection_slugs"].([]any)
			if !ok {
				t.Fatalf("collection_slugs is %T", res.data(t)["collection_slugs"])
			}
			if len(slugs) < 40 {
				t.Fatalf("collection_slugs has %d entries; the recorded project has 50", len(slugs))
			}
		})
	}
}

func TestExplainSlimIsSmallAndComplete(t *testing.T) {
	res := seeded(t, "explain", "--slim")
	d := res.data(t)
	if _, ok := d["collections"]; ok {
		t.Errorf("--slim must not carry the per-collection detail array")
	}
	if _, ok := d["exit_codes"]; !ok {
		t.Errorf("--slim must carry the exit-code table")
	}
	meta := res.Env["meta"].(map[string]any)
	bytesN, _ := meta["bytes"].(json.Number)
	if bytesN.String() == "" || bytesN.String() == "0" {
		t.Errorf("meta.bytes must always be set so an agent can budget: %v", meta["bytes"])
	}
}

func TestExplainDefaultTierTruncatesDetailWithARunnableRecovery(t *testing.T) {
	res := seeded(t, "explain")
	colls := res.data(t)["collections"].([]any)
	if len(colls) > DetailCap {
		t.Fatalf("detail array has %d entries, cap is %d", len(colls), DetailCap)
	}
	if len(colls) < DetailCap {
		t.Skipf("the recorded project has only %d collections; nothing to truncate", len(colls))
	}
	var warn map[string]any
	for _, w := range res.warnings(t) {
		if w["code"] == "collections_truncated" {
			warn = w
		}
	}
	if warn == nil {
		t.Fatalf("no collections_truncated warning: %v", res.warnings(t))
	}
	hint, _ := warn["hint"].(string)
	if !strings.HasPrefix(hint, "pay explain --section collections --offset ") {
		t.Errorf("truncation hint is not runnable: %q", hint)
	}
	// The recovery must actually work.
	next := seeded(t, strings.Fields(hint)[1:]...)
	if next.Code != 0 {
		t.Fatalf("the documented recovery failed with exit %d:\n%s", next.Code, next.Stdout)
	}
}

func TestExplainFullRefusesAboveMaxBytes(t *testing.T) {
	res := seeded(t, "explain", "--full", "--max-bytes", "2048")
	if res.Code != 5 {
		t.Fatalf("exit = %d, want 5\n%s", res.Code, res.Stdout)
	}
	if got := res.code(t); got != "request_too_large" {
		t.Errorf("code = %q, want request_too_large", got)
	}
}

func TestExplainSectionAndCollectionTiers(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
		deny []string
	}{
		{"connection", []string{"explain", "--section", "connection"},
			[]string{"connection", "collection_slugs"}, []string{"collections", "gotchas"}},
		{"query_syntax", []string{"explain", "--section", "query_syntax"},
			[]string{"query_syntax"}, []string{"collections"}},
		{"exit_codes", []string{"explain", "--section", "exit_codes"},
			[]string{"exit_codes"}, []string{"query_syntax"}},
		{"gotchas", []string{"explain", "--section", "gotchas"},
			[]string{"gotchas"}, []string{"commands"}},
		{"commands", []string{"explain", "--section", "commands"},
			[]string{"commands"}, []string{"gotchas"}},
		{"globals", []string{"explain", "--section", "globals"},
			[]string{"globals"}, []string{"collections"}},
		{"one collection", []string{"explain", "--collection", "pages"},
			[]string{"collection", "entity", "fields"}, []string{"collections"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := seeded(t, tc.args...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			d := res.data(t)
			for _, key := range tc.want {
				if _, ok := d[key]; !ok {
					t.Errorf("data.%s missing", key)
				}
			}
			for _, key := range tc.deny {
				if _, ok := d[key]; ok {
					t.Errorf("data.%s should not be in this slice", key)
				}
			}
		})
	}
}

func TestExplainRejectsBadArguments(t *testing.T) {
	cases := []struct {
		args []string
		code string
	}{
		{[]string{"explain", "--section", "nope"}, "invalid_option"},
		{[]string{"explain", "--slim", "--full"}, "invalid_args"},
		{[]string{"explain", "--offset", "-1"}, "invalid_args"},
		{[]string{"explain", "--collection", "no-such-collection"}, "collection_unknown"},
		{[]string{"explain", "--output", "csv"}, "format_unsupported"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			res := seeded(t, tc.args...)
			if got := res.code(t); got != tc.code {
				t.Errorf("code = %q, want %q\n%s", got, tc.code, res.Stdout)
			}
		})
	}
}

func TestExplainUnknownCollectionSuggests(t *testing.T) {
	res := seeded(t, "explain", "--collection", "page")
	e, _ := res.Env["error"].(map[string]any)
	dym, _ := e["did_you_mean"].([]any)
	found := false
	for _, s := range dym {
		if s == "pages" {
			found = true
		}
	}
	if !found {
		t.Errorf("did_you_mean = %v, want it to contain \"pages\"", dym)
	}
}

func TestExplainQuerySyntaxTeachesEveryOperator(t *testing.T) {
	res := seeded(t, "explain", "--section", "query_syntax")
	qs := res.data(t)["query_syntax"].(map[string]any)
	ops, _ := qs["operators"].([]any)
	if len(ops) != len(whereOperators) {
		t.Fatalf("operator table has %d rows, want %d", len(ops), len(whereOperators))
	}
	// The two verified footguns must be stated in the payload, not only in help.
	dev, _ := qs["deviations"].([]any)
	joined := ""
	for _, d := range dev {
		joined += d.(string)
	}
	for _, want := range []string{"contains escapes", "nlike auto-wraps"} {
		if !strings.Contains(joined, want) {
			t.Errorf("deviations do not mention %q: %v", want, dev)
		}
	}
}

func TestExplainExitCodeTableIsTotal(t *testing.T) {
	res := seeded(t, "explain", "--section", "exit_codes")
	rows, _ := res.data(t)["exit_codes"].([]any)
	seen := map[string]bool{}
	for _, r := range rows {
		row := r.(map[string]any)
		codes, _ := row["codes"].([]any)
		for _, c := range codes {
			entry := c.(map[string]any)
			seen[entry["code"].(string)] = true
			if entry["hint"] == "" {
				t.Errorf("code %v has no hint", entry["code"])
			}
		}
	}
	if len(seen) < 40 {
		t.Errorf("only %d error codes reported; the table should be total", len(seen))
	}
}

func TestCollectionsFiltersAndOrdering(t *testing.T) {
	res := seeded(t, "collections")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	rows, _ := d["collections"].([]any)
	if len(rows) == 0 {
		t.Fatal("no collections returned")
	}
	for _, r := range rows {
		if r.(map[string]any)["internal"] == true {
			t.Errorf("internal collections must be hidden by default")
		}
	}

	withInternal := seeded(t, "collections", "--include-internal")
	if len(withInternal.data(t)["collections"].([]any)) <= len(rows) {
		t.Errorf("--include-internal did not widen the list")
	}

	grepped := seeded(t, "collections", "--grep", "page")
	for _, r := range grepped.data(t)["collections"].([]any) {
		slug := r.(map[string]any)["slug"].(string)
		if !strings.Contains(strings.ToLower(slug), "page") {
			t.Errorf("--grep page matched %q", slug)
		}
	}

	if got := seeded(t, "collections", "--kind", "nope").code(t); got != "invalid_option" {
		t.Errorf("--kind nope = %q, want invalid_option", got)
	}
	if got := seeded(t, "collections", "--capability", "nope").code(t); got != "invalid_option" {
		t.Errorf("--capability nope = %q, want invalid_option", got)
	}
}

func TestDescribeReturnsTheFullFieldContract(t *testing.T) {
	res := seeded(t, "describe", "pages")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	fields, _ := d["fields"].([]any)
	if len(fields) == 0 {
		t.Fatal("no fields returned for pages")
	}
	// §7.8.2: every field entry carries the same 26 keys, nulls included.
	first := fields[0].(map[string]any)
	if len(first) != 26 {
		t.Errorf("field entry has %d keys, want exactly 26: %v", len(first), fieldKeyNames(first))
	}
	for _, key := range []string{"path", "payload_type", "required", "queryable", "operators", "write_shape"} {
		if _, ok := first[key]; !ok {
			t.Errorf("field entry is missing %q", key)
		}
	}
}

func TestDescribeUnknownEntityAndField(t *testing.T) {
	if got := seeded(t, "describe", "page").code(t); got != "collection_unknown" {
		t.Errorf("unknown entity = %q, want collection_unknown", got)
	}
	if got := seeded(t, "describe", "pages", "--field", "no-such-field").code(t); got != "unknown_field" {
		t.Errorf("unknown field = %q, want unknown_field", got)
	}
}

func TestDescribeFiltersAndExamples(t *testing.T) {
	all := seeded(t, "describe", "pages")
	required := seeded(t, "describe", "pages", "--required-only")
	if len(required.data(t)["fields"].([]any)) > len(all.data(t)["fields"].([]any)) {
		t.Errorf("--required-only widened the field list")
	}
	queryable := seeded(t, "describe", "pages", "--queryable")
	for _, f := range queryable.data(t)["fields"].([]any) {
		if f.(map[string]any)["queryable"] != true {
			t.Errorf("--queryable returned a non-queryable field")
		}
	}
	ex := seeded(t, "describe", "pages", "--examples")
	examples, _ := ex.data(t)["examples"].([]any)
	if len(examples) < 3 {
		t.Errorf("--examples produced %d commands, want at least 3", len(examples))
	}
	for _, e := range examples {
		if !strings.HasPrefix(e.(string), "pay ") {
			t.Errorf("example is not a pay command: %q", e)
		}
	}
}

func fieldKeyNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
