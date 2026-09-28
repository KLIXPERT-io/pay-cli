package cli

import (
	"fmt"
	"strings"
	"testing"
)

// seedPendingDraft stores a published page 40 whose newest version is a
// draft with another title, as `pay update pages 40 --set title=… --draft`
// leaves it.
func seedPendingDraft(m *memCMS) {
	m.seed("pages", 40, map[string]any{"title": "Live", "slug": "s", "summary": "old", "_status": "published"})
	d := m.colls["pages"]["40"]
	d.latest["title"] = "Pending"
	d.latest["_status"] = "draft"
}

// warningMessages joins the messages of every warning with code.
func warningMessages(t *testing.T, r cliResult, code string) string {
	t.Helper()
	var out []string
	for _, w := range r.warnings(t) {
		if w["code"] == code {
			out = append(out, fmt.Sprint(w["message"]))
		}
	}
	return strings.Join(out, "\n")
}

// `pay plan` and `pay sync --dry-run` are the review step before
// `sync --yes`. With status "published" over a pending draft the real sync
// warned publishes_pending_draft and took the draft's other changes live,
// but the preview said nothing and called the live page "draft".
func TestPlanWarnsPublishOverAPendingDraft(t *testing.T) {
	m := newMemCMS(t)
	seedPendingDraft(m)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "a.json", `{"key":"a","collection":"pages","status":"published","data":{"slug":"s","summary":"new"}}`)

	for _, args := range [][]string{{"plan", dir}, {"sync", dir, "--dry-run"}} {
		r := m.run(t, home, nil, args...)
		if r.Code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, r.Code, r.Stdout)
		}
		if !r.hasWarning(t, WarnPublishesPendingDraft) {
			t.Errorf("%v: no publishes_pending_draft: %v", args, r.warnings(t))
		}
		if msg := warningMessages(t, r, WarnPublishesPendingDraft); !strings.HasPrefix(msg, "[a] pages 40") {
			t.Errorf("%v: message = %q", args, msg)
		}
		it := planItems(t, r)[0]
		if it["current_status"] != "draft" || it["stored_status"] != "published" || it["write_mode"] != "publish" {
			t.Errorf("%v: item = %v", args, it)
		}
	}
	if len(m.writes) != 0 {
		t.Fatalf("a preview wrote: %v", m.writes)
	}

	// The real sync warns through the update path — once, not twice.
	r := m.run(t, home, nil, "sync", dir, "--yes")
	if r.Code != 0 {
		t.Fatalf("sync: exit %d\n%s", r.Code, r.Stdout)
	}
	if n := strings.Count(warningMessages(t, r, WarnPublishesPendingDraft), "\n") + 1; n != 1 {
		t.Errorf("sync warned publishes_pending_draft %d times: %v", n, r.warnings(t))
	}
}

// status keep over a published document with a pending draft saves into the
// draft (the safe choice) — and now says so, on the preview and on the real
// write, instead of an unexplained ok / "updated".
func TestKeepOverAPendingDraftWarnsSavedAsDraft(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		m := newMemCMS(t)
		seedPendingDraft(m)
		home := m.home(t)
		dir := t.TempDir()
		writeManifest(t, dir, "a.json", `{"key":"a","collection":"pages","data":{"slug":"s","summary":"new"}}`)
		for _, args := range [][]string{{"plan", dir}, {"sync", dir, "--dry-run"}, {"sync", dir, "--yes"}} {
			r := m.run(t, home, nil, args...)
			if r.Code != 0 || !r.hasWarning(t, WarnSavedAsDraft) {
				t.Fatalf("%v: exit %d, warnings %v\n%s", args, r.Code, r.warnings(t), r.Stdout)
			}
			if r.hasWarning(t, WarnPublishesPendingDraft) {
				t.Errorf("%v: a draft save warned publishes_pending_draft", args)
			}
		}
		if len(m.writes) != 1 || !strings.Contains(m.writes[0], "draft=true") {
			t.Fatalf("writes = %v", m.writes)
		}
		if main := m.colls["pages"]["40"].main; main["summary"] != "old" || main["_status"] != "published" {
			t.Errorf("the live row changed: %v", main)
		}
	})

	t.Run("upsert", func(t *testing.T) {
		m := newMemCMS(t)
		seedPendingDraft(m)
		home := m.home(t)
		for _, extra := range [][]string{{"--dry-run"}, nil} {
			r := m.run(t, home, nil, append([]string{"upsert", "pages", "--set", "slug=s", "--set", "summary=new", "--yes"}, extra...)...)
			if r.Code != 0 || !r.hasWarning(t, WarnSavedAsDraft) {
				t.Fatalf("%v: exit %d, warnings %v\n%s", extra, r.Code, r.warnings(t), r.Stdout)
			}
		}
		if main := m.colls["pages"]["40"].main; main["summary"] != "old" {
			t.Errorf("the live row changed: %v", main)
		}
	})

	t.Run("no warning without a pending draft or with an explicit status", func(t *testing.T) {
		for _, tc := range []struct{ status, current string }{
			{"keep", "draft"},     // never published: a draft is what it is
			{"keep", "published"}, // no pending draft: saved live
			{"draft", "pending"},  // --draft said so
		} {
			m := newMemCMS(t)
			if tc.current == "pending" {
				seedPendingDraft(m)
			} else {
				m.seed("pages", 40, map[string]any{"title": "T", "slug": "s", "summary": "old", "_status": tc.current})
			}
			home := m.home(t)
			dir := t.TempDir()
			writeManifest(t, dir, "a.json", fmt.Sprintf(`{"collection":"pages","status":%q,"data":{"slug":"s","summary":"new"}}`, tc.status))
			r := m.run(t, home, nil, "plan", dir)
			if r.Code != 0 || r.hasWarning(t, WarnSavedAsDraft) || r.hasWarning(t, WarnPublishesPendingDraft) {
				t.Errorf("%+v: exit %d, warnings %v", tc, r.Code, r.warnings(t))
			}
			if it := planItems(t, r)[0]; tc.current == "published" && it["stored_status"] != nil {
				t.Errorf("%+v: stored_status read without need: %v", tc, it)
			}
		}
	})
}
