package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
)

// On a project whose schema names no blocks field (introspection disabled),
// the cached census chooses the field — no blocks_field_inferred warning —
// and `ls` reports what the census observed there, labelled as observation.
func TestBlocksLsUsesTheCachedCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	doc := censusPageDocs()[0]

	r := censusRun(t, home, pageEnvelope(t, doc), "blocks", "ls")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if r.hasWarning(t, WarnBlocksFieldInferred) {
		t.Errorf("the census knows layout; nothing was inferred: %v", r.warnings(t))
	}
	d := r.data(t)
	if d["field"] != "layout" {
		t.Errorf("field = %v", d["field"])
	}
	acc := toStrings(d["accepts"])
	sort.Strings(acc)
	if got := strings.Join(acc, ","); got != "group,hero" {
		t.Errorf("accepts = %q", got)
	}
	if d["accepts_source"] != discovery.SourceObservedCensus || d["accepts_authoritative"] != false || d["accepts_pattern"] != "layout" {
		t.Errorf("accepts provenance = %v / %v / %v", d["accepts_source"], d["accepts_authoritative"], d["accepts_pattern"])
	}

	// Nested: what real GROUP rows contain (parent-scoped), not the whole
	// second level.
	r = censusRun(t, home, pageEnvelope(t, doc), "blocks", "ls", "--field", "layout[1].blocks")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d = r.data(t)
	acc = toStrings(d["accepts"])
	sort.Strings(acc)
	if got := strings.Join(acc, ","); got != "grid,text" {
		t.Errorf("nested accepts = %q", got)
	}
	if d["accepts_pattern"] != "group.blocks" || d["accepts_source"] != discovery.SourceObservedCensus {
		t.Errorf("nested provenance = %v / %v", d["accepts_pattern"], d["accepts_source"])
	}
	// Two levels down, inside the grid.
	r = censusRun(t, home, pageEnvelope(t, doc), "blocks", "ls", "--field", "layout[1].blocks[1].blocks")
	d = r.data(t)
	if got := strings.Join(toStrings(d["accepts"]), ","); got != "text" || d["accepts_pattern"] != "grid.blocks" {
		t.Errorf("grid accepts = %q at %v", got, d["accepts_pattern"])
	}

	// An observed list never gates an edit: a slug the census never saw is
	// still added (Payload is the judge).
	r = censusRun(t, home, pageEnvelope(t, doc), "blocks", "add", "statement", "--last")
	if r.Code != 0 {
		t.Errorf("an observed-only list must not refuse a slug: exit %d %s", r.Code, r.Stdout)
	}
}

// Without a census the warning still fires, and its hint names the command
// that makes the next run certain on a site without introspection.
func TestInferredFieldHintNamesTheCensus(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	r := censusRun(t, home, pageEnvelope(t, pageDoc), "blocks", "ls")
	var hint string
	for _, w := range r.warnings(t) {
		if w["code"] == WarnBlocksFieldInferred {
			hint, _ = w["hint"].(string)
		}
	}
	if !strings.Contains(hint, "pay blocks learn pages") {
		t.Errorf("hint = %q", hint)
	}
	// A bare document names no collection, so there is nothing to learn.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls"}, Stdin: pageDoc})
	for _, w := range r.warnings(t) {
		if w["code"] == WarnBlocksFieldInferred && strings.Contains(w["hint"].(string), "pay blocks learn") {
			t.Errorf("no collection, no learn hint: %v", w["hint"])
		}
	}
}

func TestPickRowFieldFromTheCensus(t *testing.T) {
	doc := map[string]any{"layout": []any{}, "hero": map[string]any{"blocks": []any{}}}
	got, inferred, err := pickRowField(doc, nil, []string{"layout"})
	if err != nil || got != "layout" || inferred {
		t.Errorf("one observed field = %q %v %v", got, inferred, err)
	}
	// Observed but absent from this document: named anyway (field_absent
	// downstream), never a guess off the document.
	got, inferred, err = pickRowField(map[string]any{"title": "x"}, nil, []string{"layout"})
	if err != nil || got != "layout" || inferred {
		t.Errorf("absent observed field = %q %v %v", got, inferred, err)
	}
	_, _, err = pickRowField(doc, nil, []string{"hero.blocks", "layout"})
	if err == nil || !strings.Contains(err.Error(), "block census") {
		t.Errorf("two observed fields present must be ambiguous: %v", err)
	}
	// A declared schema still wins over the census.
	shard := &discovery.Shard{BlockFields: map[string]discovery.BlockField{"hero.blocks": {}}}
	got, _, err = pickRowField(doc, shard, []string{"layout"})
	if err != nil || got != "hero.blocks" {
		t.Errorf("declared field = %q %v", got, err)
	}
}

// Completion reads block slugs and blocks-field patterns from the cached
// census — the only source on a site with introspection disabled.
func TestCompletionUsesTheCachedCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	env := seededEnv(testBaseURL)

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"describe", "pages", "--block", ""}, []string{"grid", "group", "hero", "text"}},
		{[]string{"describe", "pages", "--block", "g"}, []string{"grid", "group"}},
		{[]string{"blocks", "schema", ""}, []string{"grid", "group", "hero", "text"}},
		{[]string{"blocks", "new", "h"}, []string{"hero"}},
		{[]string{"blocks", "example", "--collection", "pages", "t"}, []string{"text"}},
		{[]string{"blocks", "add", "gr"}, []string{"grid", "group"}},
		{[]string{"blocks", "types", "pages", "--field", "layout["}, []string{"layout[].blocks", "layout[].blocks[].blocks"}},
		{[]string{"blocks", "schema", "text", "--field", "g"}, []string{"grid.blocks", "group.blocks"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			res := complete(t, home, env, c.args...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stderr)
			}
			if got := strings.Join(suggestions(res.Stdout), ","); got != strings.Join(c.want, ",") {
				t.Errorf("got %q, want %q\n%s", got, strings.Join(c.want, ","), res.Stdout)
			}
		})
	}
	// A census-only slug says it was observed, and where.
	res := complete(t, home, env, "blocks", "schema", "her")
	if !strings.Contains(res.Stdout, "hero\tobserved: 4 rows in pages") {
		t.Errorf("description missing:\n%s", res.Stdout)
	}
	// describe --field keeps the ordinary field paths and adds the census's
	// nested patterns.
	res = complete(t, home, env, "describe", "pages", "--field", "layout")
	got := suggestions(res.Stdout)
	if !containsString(got, "layout") || !containsString(got, "layout[].blocks") {
		t.Errorf("describe --field = %v", got)
	}
	// --collection completes entities even after the positional argument.
	res = complete(t, home, env, "blocks", "schema", "hero", "--collection", "pag")
	if got := suggestions(res.Stdout); !containsString(got, "pages") {
		t.Errorf("--collection = %v", got)
	}
}

// doctor runs the checks that need no server even when there is no
// base_url to reach one.
func TestDoctorWithNoBaseURLRunsTheLocalChecks(t *testing.T) {
	dir := t.TempDir()
	res := cliRun(t, invocation{Args: []string{"doctor"}, Env: []string{"PAY_BACKUP_DIR=" + dir}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	checks := doctorChecks(t, res)
	if checks["base_url"]["status"] != CheckFail {
		t.Errorf("base_url = %v", checks["base_url"])
	}
	for _, name := range []string{"audit_log", "backups", "skill", "update"} {
		if _, ok := checks[name]; !ok {
			t.Errorf("doctor without base_url has no %q check", name)
		}
	}
	if checks["backups"]["status"] != CheckOK {
		t.Errorf("backups = %v", checks["backups"])
	}
	// Nothing that needs the server ran.
	for _, name := range []string{"credential", "reachability", "discovery", "cache", "block_census"} {
		if _, ok := checks[name]; ok {
			t.Errorf("doctor without base_url ran %q", name)
		}
	}
	if b, _ := res.data(t)["backups"].(map[string]any); b["dir"] != dir {
		t.Errorf("data.backups = %v", b)
	}
}

// Completion never offers a slug inferred from a GraphQL interfaceName once
// the census has matched it to the slug documents really carry: the inferred
// one is exactly what Payload silently drops.
func TestCompletionPrefersCensusMatchedSlugs(t *testing.T) {
	shard := &discovery.Shard{}
	shard.SetBlockField(discovery.BlockField{
		Path: "layout", Slugs: []string{"callToActionBlock", "mediaBlock"},
		Source:         discovery.SourceUnionInferred,
		SlugSources:    map[string]string{"callToActionBlock": discovery.SourceUnionInferred, "mediaBlock": discovery.SourceUnionInferred},
		SlugInterfaces: map[string]string{"callToActionBlock": "CallToActionBlock", "mediaBlock": "MediaBlock"},
		InterfaceNames: []string{"CallToActionBlock", "MediaBlock"},
	})
	census := buildPagesCensus(t, "s", []string{
		`{"id":1,"layout":[{"id":"a","blockType":"cta","links":[]},{"id":"b","blockType":"mediaBlock","media":1}]}`,
	})
	k := discovery.NewBlockKnowledge(discovery.EntityKindCollection, "pages", census, shard)
	got := suggestions(strings.Join(censusSlugCandidates("", k, nil), "\n"))
	if strings.Join(got, ",") != "cta,mediaBlock" {
		t.Errorf("candidates = %v", got)
	}
	// Without a census the declared list is offered, as before.
	k = discovery.NewBlockKnowledge(discovery.EntityKindCollection, "pages", nil, shard)
	got = suggestions(strings.Join(censusSlugCandidates("", k, nil), "\n"))
	if strings.Join(got, ",") != "callToActionBlock,mediaBlock" {
		t.Errorf("declared candidates = %v", got)
	}
}
