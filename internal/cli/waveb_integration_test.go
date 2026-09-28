package cli

import (
	"fmt"
	"strings"
	"testing"
)

// Wave B integration: the features meet in the same write paths, so the
// shapes and refusals one of them defines must hold on the others' verbs.

// Every dry run that previews a write carries data.diff in one shape
// (docDiffData): base, id, status, updated_at, identical, summary, changes.
// plan items and the upsert dry run are previews too.
func TestPlanAndUpsertDiffsShareTheDryRunShape(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)

	res := m.run(t, home, nil, "plan", siteManifest(t))
	if res.Code != 0 {
		t.Fatalf("plan exit %d\n%s", res.Code, res.Stdout)
	}
	d := itemByKey(t, planItems(t, res), "pages/home")["diff"].(map[string]any)
	if d["base"] != "latest" || fmt.Sprint(d["id"]) != "16" || d["status"] != "published" || d["summary"] == nil {
		t.Errorf("plan item diff = %v", d)
	}
	hdr := itemByKey(t, planItems(t, res), "globals/header")["diff"].(map[string]any)
	if _, has := hdr["id"]; has || hdr["base"] != "latest" {
		t.Errorf("a global's diff has no id: %v", hdr)
	}

	dry := m.run(t, home, nil, "upsert", "pages", "--set", "slug=home", "--set", "title=Home v3", "--dry-run")
	if dry.Code != 0 {
		t.Fatalf("upsert dry exit %d\n%s", dry.Code, dry.Stdout)
	}
	ud := dry.data(t)["diff"].(map[string]any)
	if ud["base"] != "latest" || fmt.Sprint(ud["id"]) != "16" || ud["status"] != "published" {
		t.Errorf("upsert dry-run diff = %v", ud)
	}
	if ch, _ := ud["changes"].([]any); len(ch) != 1 || ch[0].(map[string]any)["path"] != "title" {
		t.Errorf("upsert dry-run changes = %v", ud["changes"])
	}
	if len(m.writes) != 0 {
		t.Errorf("dry runs wrote: %v", m.writes)
	}
}

// A document read with --richtext md|text must not be synced back: its
// rich text is strings. The manifest loader refuses the envelope, and the
// plan refuses a bare item whose declared richText field holds a string.
func TestPlanRefusesRenderedRichText(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)

	env := `{"ok":true,"v":1,"data_kind":"doc","meta":{"richtext":{"format":"md","rendered":1,"paths":["hero.richText"]}},
		"target":{"kind":"collection","slug":"pages"},"data":{"slug":"home","title":"Home","hero":{"richText":"# Hi"}}}`
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: env})
	if res.Code != 5 || !strings.Contains(res.Stdout, `"richtext_rendered"`) {
		t.Errorf("rendered envelope: exit %d\n%s", res.Code, res.Stdout)
	}

	dir := t.TempDir()
	writeManifest(t, dir, "home.json", `{"collection":"pages","data":{"slug":"home","hero":{"richText":"# Hi"}}}`)
	for _, verb := range []string{"plan", "sync"} {
		args := []string{verb, dir}
		if verb == "sync" {
			args = append(args, "--yes")
		}
		res = m.run(t, home, nil, args...)
		if res.Code != 5 || !strings.Contains(res.Stdout, `"richtext_rendered"`) {
			t.Errorf("%s of a bare rendered item: exit %d\n%s", verb, res.Code, res.Stdout)
		}
	}
	if len(m.writes) != 0 {
		t.Errorf("refused items were written: %v", m.writes)
	}
}
