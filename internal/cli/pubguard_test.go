package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// §10.2.3 a non-draft write never unpublishes by accident (wave-B review)
// ---------------------------------------------------------------------------

// addPendingDraft saves a draft on top of the newest version, as
// `pay update … --draft` does.
func addPendingDraft(s *pgSim, title string) {
	d := s.latest()
	d["title"] = title
	d["_status"] = "draft"
	s.versions = append(s.versions, d)
}

// A plain update of a published page with a pending draft used to write the
// draft into the live row with _status "draft" — the page went offline and
// neither the dry run nor the write said so (verified on localhost:3900).
func TestUpdateOverAPendingDraftIsRefusedWithoutAFlag(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	addPendingDraft(s, "pending")

	for _, extra := range [][]string{nil, {"--dry-run"}} {
		r := simRun(t, s, home, append([]string{"update", "pages", "71", "--set", "slug=b", "--yes"}, extra...)...)
		if r.code(t) != "invalid_args" || len(s.patches()) != 0 {
			t.Fatalf("%v: code %q, %d PATCHes\n%s", extra, r.code(t), len(s.patches()), r.Stdout)
		}
		if msg := fmt.Sprint(r.Env["error"]); !strings.Contains(msg, "pending draft") || !strings.Contains(msg, "--unpublish") {
			t.Errorf("%v: error = %s", extra, msg)
		}
	}
	if st := s.readMain()["_status"]; st != "published" {
		t.Fatalf("status = %v", st)
	}

	// --publish: deliberate; it publishes the draft too, and says so. The
	// dry run shows what visitors will see change (data.diff.published).
	dry := simRun(t, s, home, "update", "pages", "71", "--set", "slug=b", "--publish", "--dry-run")
	if dry.Code != 0 || !dry.hasWarning(t, WarnPublishesPendingDraft) {
		t.Fatalf("publish dry run: exit %d %v\n%s", dry.Code, dry.warnings(t), dry.Stdout)
	}
	pub, _ := dry.data(t)["diff"].(map[string]any)["published"].(map[string]any)
	if pub == nil || pub["base"] != diffBasePublished || !strings.Contains(fmt.Sprint(pub["changes"]), "pending") {
		t.Errorf("data.diff.published = %v", pub)
	}
	r := simRun(t, s, home, "update", "pages", "71", "--set", "slug=b", "--publish", "--yes")
	if r.Code != 0 || !r.hasWarning(t, WarnPublishesPendingDraft) {
		t.Fatalf("--publish: exit %d %v", r.Code, r.warnings(t))
	}
	if m := s.readMain(); m["_status"] != "published" || m["title"] != "pending" || m["slug"] != "b" {
		t.Errorf("stored after --publish = %v %v %v", m["_status"], m["title"], m["slug"])
	}

	// --draft edits the draft; --unpublish takes it offline on purpose.
	addPendingDraft(s, "pending 2")
	if r := simRun(t, s, home, "update", "pages", "71", "--set", "slug=c", "--draft", "--yes"); r.Code != 0 {
		t.Fatalf("--draft: %s", r.Stdout)
	}
	if m := s.readMain(); m["_status"] != "published" || m["slug"] != "b" {
		t.Errorf("--draft touched the live row: %v", m)
	}
	if r := simRun(t, s, home, "update", "pages", "71", "--set", "slug=d", "--unpublish", "--yes"); r.Code != 0 {
		t.Fatalf("--unpublish: %s", r.Stdout)
	}
	if st := s.readMain()["_status"]; st != "draft" {
		t.Errorf("--unpublish left %v", st)
	}
}

// A published page without a pending draft is written as before.
func TestUpdateOfAPublishedPageWithoutADraftIsUnchanged(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	r := simRun(t, s, home, "update", "pages", "71", "--set", "title=x", "--yes")
	if r.Code != 0 || r.hasWarning(t, WarnPublishesPendingDraft) {
		t.Fatalf("exit %d %v\n%s", r.Code, r.warnings(t), r.Stdout)
	}
	if m := s.readMain(); m["_status"] != "published" || m["title"] != "x" {
		t.Errorf("stored = %v %v", m["_status"], m["title"])
	}
	dry := simRun(t, s, home, "update", "pages", "71", "--set", "title=y", "--dry-run")
	if _, ok := dry.data(t)["diff"].(map[string]any)["published"]; ok {
		t.Errorf("data.diff.published without a pending draft: %v", dry.data(t)["diff"])
	}
}

// A body _status "draft" (a `pay get --draft` export) written without --draft
// used to unpublish a live page silently.
func TestBodyStatusDraftDoesNotUnpublishSilently(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	file := writeBody(t, home, `{"title":"edited","_status":"draft"}`)
	for _, extra := range [][]string{{"--dry-run"}, {"--yes"}} {
		r := simRun(t, s, home, append([]string{"update", "pages", "71", "--data-file", file}, extra...)...)
		if r.code(t) != "invalid_args" || len(s.patches()) != 0 {
			t.Fatalf("%v: code %q\n%s", extra, r.code(t), r.Stdout)
		}
	}
	if r := simRun(t, s, home, "update", "pages", "71", "--data-file", file, "--draft", "--yes"); r.Code != 0 {
		t.Fatalf("--draft: %s", r.Stdout)
	}
	if st := s.readMain()["_status"]; st != "published" {
		t.Fatalf("--draft unpublished: %v", st)
	}
	if r := simRun(t, s, home, "update", "pages", "71", "--data-file", file, "--unpublish", "--yes"); r.Code != 0 {
		t.Fatalf("--unpublish: %s", r.Stdout)
	}
	if st := s.readMain()["_status"]; st != "draft" {
		t.Errorf("--unpublish left %v", st)
	}
}

func TestPublicationVerdict(t *testing.T) {
	o := orphanTarget{t: &collTarget{Slug: "pages"}, id: "7"}
	cases := []struct {
		body          string
		stored, lates string
		wantErr       bool
		wantWarn      bool
	}{
		{"", "published", "published", false, false},
		{"", "published", "draft", true, false},
		{"", "draft", "draft", false, false},
		{"draft", "published", "published", true, false},
		{"draft", "draft", "draft", false, false},
		{"published", "published", "draft", false, true},
		{"published", "published", "published", false, false},
		{"", "", "", false, false},
	}
	for _, c := range cases {
		w, err := publicationVerdict(o, c.body, pubState{stored: c.stored, latest: c.lates})
		if (err != nil) != c.wantErr || (w != nil) != c.wantWarn {
			t.Errorf("%+v: warn %v err %v", c, w, err)
		}
	}
}

// ---------------------------------------------------------------------------
// --reset-nested repair of a PUBLISHED document (wave-B review, finding 1)
// ---------------------------------------------------------------------------

// orphanPublished seeds a published bento page and orphans it with a plain
// grid write, as a publish of a restructure does on a live page.
func orphanPublished(t *testing.T) (*pgSim, string) {
	t.Helper()
	s, home := setupSim(t, simBento, "published")
	r := simRun(t, s, home, "update", "pages", "71", "--data-file", writeBody(t, home, simGridBody), "--yes")
	if r.Code != 0 || !r.hasWarning(t, WarnOrphanRows) {
		t.Fatalf("setup: exit %d %v\n%s", r.Code, r.warnings(t), r.Stdout)
	}
	if emptyCount(s.readMain()) != 3 || s.readMain()["_status"] != "published" {
		t.Fatalf("setup: stored %v", s.readMain())
	}
	return s, home
}

// The {} rows are only in the stored row, never in a version, so they used
// to make every published orphaned document look like it had a pending
// draft: the repair every orphan warning points at was refused.
func TestResetNestedRepairsAPublishedOrphanedDocument(t *testing.T) {
	s, home := orphanPublished(t)
	before := len(s.patches())
	dry := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--dry-run")
	if dry.Code != 0 || len(s.patches()) != before {
		t.Fatalf("dry run: exit %d\n%s", dry.Code, dry.Stdout)
	}
	if plan := dry.data(t)["reset_nested"].(map[string]any); plan["mode"] != "repair" {
		t.Fatalf("plan = %v", plan)
	}
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--yes")
	if r.Code != 0 {
		t.Fatalf("repair: exit %d\n%s", r.Code, r.Stdout)
	}
	if m := s.readMain(); emptyCount(m) != 0 || m["_status"] != "published" {
		t.Errorf("after the repair: %d {} rows, status %v", emptyCount(m), m["_status"])
	}
	if rn := r.Env["meta"].(map[string]any)["reset_nested"].(map[string]any); rn["verified"] != true {
		t.Errorf("report = %v", rn)
	}
}

// A REAL pending draft is still refused, and the hint is a publish that
// works.
func TestResetNestedRepairStillRefusesARealPendingDraft(t *testing.T) {
	s, home := orphanPublished(t)
	addPendingDraft(s, "pending")
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--dry-run")
	if r.code(t) != "invalid_args" {
		t.Fatalf("code %q\n%s", r.code(t), r.Stdout)
	}
	if hint := fmt.Sprint(r.Env["error"].(map[string]any)["hint"]); !strings.Contains(hint, "pay publish pages 71 --reset-nested") {
		t.Errorf("hint = %s", hint)
	}
}

// A global's refusal used to point at `pay globals update <slug>
// --reset-nested --dry-run` — the refused repair itself.
func TestGlobalRepairHintIsAPublish(t *testing.T) {
	o := orphanTarget{g: &globalTarget{Slug: "footer"}}
	cmd := publishResetCmd(o)
	if !strings.Contains(cmd, "--publish") || cmd == o.resetCmd(" --dry-run") {
		t.Errorf("global publish hint = %q", cmd)
	}
	root := newRootCommand(&Runtime{})
	g, _, err := root.Find([]string{"globals", "update"})
	if err != nil || g.Flags().Lookup("publish") == nil || g.Flags().Lookup("unpublish") == nil {
		t.Errorf("globals update lacks --publish/--unpublish")
	}
	if !strings.HasPrefix(o.draftVsPublishedCmd(), "pay diff --global footer --draft-vs-published") {
		t.Errorf("global draft-vs-published = %q", o.draftVsPublishedCmd())
	}
}

// ---------------------------------------------------------------------------
// a failed restatement (wave-B review, finding 3)
// ---------------------------------------------------------------------------

func noBackoff(t *testing.T) {
	t.Helper()
	saved := resetRestateBackoff
	resetRestateBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { resetRestateBackoff = saved })
}

// A transient failure of the restatement is retried: it is idempotent.
func TestResetNestedRetriesATransientRestatementFailure(t *testing.T) {
	noBackoff(t)
	s, home := orphanPublished(t)
	base := s.npatch
	s.fail = func(n int) int {
		if n == base+2 { // the restatement, once
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--yes")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	if s.npatch != base+3 || emptyCount(s.readMain()) != 0 || len(s.readMain()["layout"].([]any)) != 1 {
		t.Errorf("patches %d, stored %v", s.npatch-base, s.readMain()["layout"])
	}
}

// When the restatement keeps failing, the carriers are stored (and in the
// newest version). A re-run of the repair cannot see them, so the error
// saves the restatement and names the command that sends it.
func TestResetNestedRestatementFailureNamesAWorkingRecovery(t *testing.T) {
	noBackoff(t)
	s, home := orphanPublished(t)
	base := s.npatch
	s.fail = func(n int) int {
		if n > base+1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--yes")
	if r.Code == 0 {
		t.Fatalf("the failure was not reported\n%s", r.Stdout)
	}
	e := r.Env["error"].(map[string]any)
	hint := fmt.Sprint(e["hint"])
	if strings.Contains(hint, "Re-run the same command") || strings.Contains(hint, "backups restore") {
		t.Errorf("hint = %s", hint)
	}
	if !strings.HasPrefix(hint, "pay update pages 71 --data-file ") {
		t.Fatalf("hint = %s", hint)
	}
	file := strings.Fields(strings.TrimPrefix(hint, "pay update pages 71 --data-file "))[0]
	file = strings.Trim(file, "'")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("saved restatement: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil || len(saved["layout"].([]any)) != 1 {
		t.Fatalf("saved restatement = %s", raw)
	}
	// The carriers are stored: an ordinary write is rejected.
	if l := s.readMain()["layout"].([]any); len(l) != 2 {
		t.Fatalf("carriers not stored: %v", l)
	}
	s.fail = nil
	if r := simRun(t, s, home, "update", "pages", "71", "--set", "title=x", "--yes"); r.code(t) != "validation_failed" {
		t.Fatalf("an ordinary write passed over carriers: %s", r.Stdout)
	}
	// The named recovery works.
	if r := simRun(t, s, home, "update", "pages", "71", "--data-file", file, "--yes"); r.Code != 0 {
		t.Fatalf("recovery: %s", r.Stdout)
	}
	m := s.readMain()
	if l := m["layout"].([]any); len(l) != 1 || emptyCount(m) != 0 || m["_status"] != "published" {
		t.Errorf("after the recovery: %v", m)
	}
	// The newest version is clean. (The version the phase updated in place
	// keeps the carrier as history — only the restatement could undo that.)
	if l := s.latest()["layout"].([]any); len(l) != 1 {
		t.Errorf("the newest version holds %d layout rows", len(l))
	}
}

// With backups on, the hint names the real backup file.
func TestResetNestedFailureNamesTheBackupOnlyWhenOneWasWritten(t *testing.T) {
	noBackoff(t)
	s, home := orphanPublished(t)
	base := s.npatch
	s.fail = func(n int) int {
		if n > base+1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	dir := t.TempDir()
	r := cliRun(t, invocation{Home: home, Env: append(seededEnv(s.srv.URL), "PAY_BACKUP_DIR="+dir),
		Args: []string{"update", "pages", "71", "--reset-nested", "--yes"}})
	hint := fmt.Sprint(r.Env["error"].(map[string]any)["hint"])
	files := backupFiles(t, dir)
	if len(files) != 1 || !strings.Contains(hint, files[0]) {
		t.Errorf("backups %v, hint %s", files, hint)
	}
}

// ---------------------------------------------------------------------------
// per-locale _status (wave-B review, finding 4)
// ---------------------------------------------------------------------------

// seedLocalized re-seeds the discovery cache with localization not known to
// be off (the fixture says it is off, which skips the locale=all read).
func seedLocalized(t *testing.T, home, baseURL string) {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{BaseURL: baseURL, APIPath: "/api", GraphQLPath: "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)
	m.Capabilities.Localization.Enabled = nil
	now := payloadtest.Epoch
	gen := cache.NewGeneration(now)
	m.Generation, m.Meta.Scope, m.GeneratedAt, m.ExpiresAt = gen, sc.Key, now, now.Add(24*time.Hour)
	shard.Generation = gen
	if ok, warns := cache.New(filepath.Join(home, "cache")).WriteSet(sc, cache.Set{Generation: gen, Manifest: &m,
		Shards: map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard}}, now); !ok {
		t.Fatalf("seed: %v", warns)
	}
}

func TestResetNestedRefusesAPerLocaleStatus(t *testing.T) {
	s, home := orphanPublished(t)
	seedLocalized(t, home, s.srv.URL)
	s.perLocaleStatus = true
	before := len(s.patches())
	for _, args := range [][]string{
		{"update", "pages", "71", "--reset-nested", "--dry-run"},
		{"update", "pages", "71", "--reset-nested", "--yes"},
		{"update", "pages", "71", "--data-file", writeBody(t, home, simGridBody), "--reset-nested", "--yes"},
	} {
		r := simRun(t, s, home, args...)
		if r.code(t) != "feature_unavailable" {
			t.Errorf("%v: code %q\n%s", args, r.code(t), r.Stdout)
		}
	}
	if len(s.patches()) != before {
		t.Errorf("a per-locale document was written")
	}
	// The read that proves it asks for every locale.
	found := false
	for _, r := range s.srv.Requests() {
		if r.Method == http.MethodGet && strings.Contains(r.Query, "locale=all") {
			found = true
		}
	}
	if !found {
		t.Error("no locale=all read")
	}
}

// ---------------------------------------------------------------------------
// previews (KNOWN issue, LOW findings)
// ---------------------------------------------------------------------------

// A repair dry run with nothing to do says so like the real run, and does
// not claim a write.
func TestRepairDryRunWithNothingToDoWarns(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--dry-run")
	if r.Code != 0 || !r.hasWarning(t, WarnResetNestedNotNeeded) {
		t.Fatalf("exit %d %v", r.Code, r.warnings(t))
	}
	if fmt.Sprint(r.data(t)["would_affect"]) != "0" {
		t.Errorf("would_affect = %v", r.data(t)["would_affect"])
	}
}

// `pay versions restore --dry-run` previews what the restore changes and the
// rows it would leave behind.
func TestVersionsRestoreDryRunDiffsAndPredicts(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	// Now a grid page; version 700 (the first) is the bento one.
	grid := simDoc(t, simGridBody)
	merged := s.latest()
	merged["layout"] = grid["layout"]
	s.write(merged)
	s.versions = append(s.versions, orphans.WithoutEmptyRows(orphans.CloneDoc(s.readMain())))
	// Repair so only the restore itself matters.
	if r := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--yes"); r.Code != 0 {
		t.Fatalf("repair: %s", r.Stdout)
	}
	r := simRun(t, s, home, "versions", "restore", "pages", "700", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	diff, _ := r.data(t)["diff"].(map[string]any)
	if diff == nil || diff["base"] != diffBasePublished || diff["identical"] != false {
		t.Errorf("data.diff = %v", diff)
	}
}

// The post-write read-back no longer depends on the projected echo, and a
// rich-text editor state does not count as a row holding a list.
func TestOrphanReadBackWithSelect(t *testing.T) {
	s, home := setupSim(t, simBento, "draft")
	n := len(s.srv.Requests())
	r := simRun(t, s, home, "publish", "pages", "71", "--select", "title", "--no-pre-validate", "--yes")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	reqs := s.srv.Requests()[n:]
	patchAt, readBack := -1, false
	for i, q := range reqs {
		if q.Method == http.MethodPatch {
			patchAt = i
		}
		if patchAt >= 0 && i > patchAt && q.Method == http.MethodGet && q.Path == "/api/pages/71" {
			readBack = true
		}
	}
	if !readBack {
		t.Errorf("no stored-row read-back after a --select publish: %v", reqs)
	}
	rich := map[string]any{"text": map[string]any{"root": map[string]any{"type": "root", "children": []any{}}}}
	if holdsList(rich) {
		t.Error("an editor state counted as a list of rows")
	}
	if !holdsList(map[string]any{"blocks": []any{}}) {
		t.Error("a blocks list was not seen")
	}
}
