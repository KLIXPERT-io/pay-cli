package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// ---------------------------------------------------------------------------
// "<redacted>" inside rich text and json (§9.15.2)
// ---------------------------------------------------------------------------

// richWith is a Lexical state whose text node carries extra, and a
// mediaBlock-like block node holding a populated upload (stage page 35's
// shape: fields.media.ogQuelle.hash).
func richWith(extra map[string]any, hash any) map[string]any {
	text := map[string]any{"type": "text", "text": "hello", "version": json.Number("1")}
	for k, v := range extra {
		text[k] = v
	}
	return map[string]any{"root": map[string]any{"type": "root", "version": json.Number("1"), "children": []any{
		map[string]any{"type": "paragraph", "version": json.Number("1"), "children": []any{text}},
		map[string]any{"type": "block", "version": json.Number("2"), "fields": map[string]any{
			"blockType": "mediaBlock", "id": "mb1",
			"media": map[string]any{"id": json.Number("848"), "ogQuelle": map[string]any{"hash": hash, "url": "https://x"}},
		}},
	}}}
}

// An export masks secret-looking keys inside rich text; Payload stores the
// editor state whole, so leaving the key out would DELETE it. The plan puts
// the stored value back: the unmodified export is unchanged (stage page 35,
// whose rich text stores the literal "<redacted>", planned update before).
func TestPlanRestoresMaskedValuesInsideRichText(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 35, map[string]any{"title": "P35", "slug": "p35", "_status": "published", "layout": []any{
		map[string]any{"id": "r1", "blockType": "content", "richText": richWith(map[string]any{"secretNote": "keep-me-123"}, redact.Mask)},
	}})
	home := m.home(t)
	get := m.run(t, home, nil, "get", "pages", "35", "--depth", "0", "--draft")
	if !strings.Contains(get.Stdout, redact.Mask) || strings.Contains(get.Stdout, "keep-me-123") {
		t.Fatalf("secretNote must be masked on read: %s", get.Stdout)
	}
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: get.Stdout})
	it := planItems(t, res)[0]
	if it["action"] != "unchanged" {
		t.Fatalf("plan of the unmodified export = %v (warnings %v)", it, res.warnings(t))
	}
	if res.hasWarning(t, WarnRedactedRemoved) {
		t.Errorf("nothing was removed: %v", res.warnings(t))
	}

	// An edit elsewhere writes the rich text with the REAL value restored.
	get.Env["data"].(map[string]any)["title"] = "P35 v2"
	get.Env["data"].(map[string]any)["layout"].([]any)[0].(map[string]any)["blockName"] = "renamed"
	raw, _ := json.Marshal(get.Env)
	res = cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"sync", "-", "--yes"}, Stdin: string(raw)})
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d bodies %v\n%s", res.Code, m.bodies, res.Stdout)
	}
	sent, _ := json.Marshal(m.bodies[0])
	// The one mask sent is the literal stored at hash (restored as stored).
	if strings.Count(string(sent), "redacted") != 1 || !strings.Contains(string(sent), "keep-me-123") {
		t.Errorf("a mask was written: %s", sent)
	}
	stored := m.colls["pages"]["35"].latest["layout"].([]any)[0].(map[string]any)["richText"]
	b, _ := json.Marshal(stored)
	if !strings.Contains(string(b), `"secretNote":"keep-me-123"`) {
		t.Errorf("the stored secret was lost: %s", b)
	}
}

// Without a stored value to restore from (a create), the masked key is
// removed, and redacted_removed says so instead of promising it stays.
func TestPlanRemovesMaskedRichTextKeysOnCreate(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	doc := map[string]any{"collection": "pages", "data": map[string]any{"title": "New", "slug": "new-one",
		"layout": []any{map[string]any{"blockType": "content", "richText": richWith(map[string]any{"secretNote": redact.Mask}, nil)}}}}
	raw, _ := json.Marshal(doc)
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"sync", "-", "--yes"}, Stdin: string(raw)})
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if !res.hasWarning(t, WarnRedactedRemoved) || res.hasWarning(t, WarnRedactedNotSent) {
		t.Errorf("warnings = %v", res.warnings(t))
	}
	sent, _ := json.Marshal(m.bodies[0])
	if strings.Contains(string(sent), "redacted") || strings.Contains(string(sent), "secretNote") {
		t.Errorf("the mask was sent: %s", sent)
	}
}

// upsert restores the same way.
func TestUpsertRestoresMaskedRichText(t *testing.T) {
	m := newMemCMS(t)
	m.seed("posts", 28, map[string]any{"title": "T", "slug": "t", "_status": "draft",
		"content": richWith(map[string]any{"secretNote": "keep-me-123"}, nil)})
	home := m.home(t)
	body := map[string]any{"title": "T2", "slug": "t", "content": richWith(map[string]any{"secretNote": redact.Mask}, nil)}
	raw, _ := json.Marshal(body)
	res := m.run(t, home, nil, "upsert", "posts", "--data", string(raw), "--draft")
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if _, sent := m.bodies[0]["content"]; sent {
		t.Errorf("the restored rich text equals the stored one and must not be sent: %v", m.bodies[0])
	}
	b, _ := json.Marshal(m.colls["posts"]["28"].latest["content"])
	if !strings.Contains(string(b), "keep-me-123") {
		t.Errorf("stored content lost the secret: %s", b)
	}
}

// The other write verbs used to send the mask verbatim: `pay get | pay update
// --data @-` now leaves merged masks out and restores rich-text ones from the
// document itself.
func TestUpdateDataHandlesMaskedValues(t *testing.T) {
	m := newMemCMS(t)
	m.seed("posts", 28, map[string]any{"title": "T", "slug": "t", "_status": "draft", "apiToken": "tok-real-1234",
		"content": richWith(map[string]any{"secretNote": "keep-me-123"}, nil)})
	home := m.home(t)
	get := m.run(t, home, nil, "get", "posts", "28", "--depth", "0", "--draft")
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL),
		// --no-redact: the envelope would mask the preview's secretNote again.
		Args: []string{"update", "posts", "28", "--data", "@-", "--draft", "--dry-run", "--no-redact"}, Stdin: get.Stdout})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	body, _ := json.Marshal(res.data(t)["request"].(map[string]any)["body"])
	if strings.Contains(string(body), "redacted") {
		t.Errorf("the dry-run body carries the mask: %s", body)
	}
	if !strings.Contains(string(body), "keep-me-123") || strings.Contains(string(body), "apiToken") {
		t.Errorf("rich text must be restored, the merged key left out: %s", body)
	}
	if !res.hasWarning(t, WarnRedactedNotSent) {
		t.Errorf("warnings = %v", res.warnings(t))
	}
	// create has no stored document: the key is removed, and said so.
	res = m.run(t, home, nil, "create", "posts", "--data", `{"title":"N","slug":"n","content":`+mustJSON(t, richWith(map[string]any{"secretNote": redact.Mask}, nil))+`}`, "--dry-run")
	body, _ = json.Marshal(res.data(t)["request"].(map[string]any)["body"])
	if res.Code != 0 || strings.Contains(string(body), "redacted") || !res.hasWarning(t, WarnRedactedRemoved) {
		t.Errorf("create: exit %d body %s warnings %v", res.Code, body, res.warnings(t))
	}
}

func TestWithoutMaskedSkipsWholesaleValues(t *testing.T) {
	body := map[string]any{
		"apiKey": redact.Mask,
		"meta":   map[string]any{"token": redact.Mask, "title": "x"},
		"rt":     richWith(map[string]any{"secretNote": redact.Mask}, nil),
	}
	out, dropped, inWhole := withoutMasked(body, wholesaleValues(nil))
	if fmt.Sprint(dropped) != "[apiKey meta.token]" || !inWhole {
		t.Errorf("dropped %v inWhole %v", dropped, inWhole)
	}
	if !containsMask(out["rt"]) {
		t.Errorf("a rich-text mask must wait for restoreMasked: %v", out["rt"])
	}
	restored, got, removed := restoreMasked(map[string]any{"rt": richWith(map[string]any{"secretNote": "real"}, nil)}, out, wholesaleValues(nil))
	if len(got) != 1 || len(removed) != 0 || containsMask(restored) {
		t.Errorf("restored %v removed %v body %v", got, removed, restored)
	}
	// A node of another type at the same place is not a counterpart.
	other := richWith(nil, nil)
	other["root"].(map[string]any)["children"].([]any)[0].(map[string]any)["type"] = "heading"
	_, got, removed = restoreMasked(map[string]any{"rt": other}, out, wholesaleValues(nil))
	if len(got) != 0 || len(removed) != 1 {
		t.Errorf("restored %v removed %v", got, removed)
	}
}

// ---------------------------------------------------------------------------
// presence-based hooks: unchanged scalars are re-sent
// ---------------------------------------------------------------------------

// The website template's populatePublishedAt sets publishedAt to now when
// the request has none, so an update that sent only the changed field moved
// the date and needed a second sync. Declared, unchanged scalars are re-sent.
func TestUpsertResendsUnchangedScalars(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 50, map[string]any{"title": "A", "slug": "pa", "_status": "published",
		"publishedAt": "2026-02-03T10:00:00.000Z"})
	home := m.home(t)
	res := m.run(t, home, nil, "upsert", "pages", "--data", `{"title":"B","slug":"pa","publishedAt":"2026-02-03T10:00:00Z"}`)
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if m.bodies[0]["publishedAt"] != "2026-02-03T10:00:00.000Z" {
		t.Errorf("publishedAt must be re-sent in its stored form: %v", m.bodies[0])
	}
	if got := m.colls["pages"]["50"].main["publishedAt"]; got != "2026-02-03T10:00:00.000Z" {
		t.Errorf("the hook moved publishedAt: %v", got)
	}
	res = m.run(t, home, nil, "upsert", "pages", "--data", `{"title":"B","slug":"pa","publishedAt":"2026-02-03T10:00:00Z"}`, "--dry-run")
	if upsertMeta(t, res)["action"] != "unchanged" {
		t.Errorf("the next upsert must be unchanged: %v", upsertMeta(t, res))
	}
	// --ignore keeps a differing value out of the write.
	m.bodies = nil
	res = m.run(t, home, nil, "upsert", "pages", "--data", `{"title":"C","slug":"pa","publishedAt":"2020-01-01"}`, "--ignore", "publishedAt")
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if _, sent := m.bodies[0]["publishedAt"]; sent {
		t.Errorf("an ignored, differing field must not be sent: %v", m.bodies[0])
	}
}

func TestHookRewroteWarning(t *testing.T) {
	dc := decision{action: actionUpdate, fields: []string{"title"}, resent: []string{"slug"},
		body: map[string]any{"title": "B", "slug": "s", "meta": map[string]any{"title": "M"}}}
	echo := map[string]any{"title": "B", "slug": "s", "meta": map[string]any{"title": "reset by a hook"}}
	w := hookRewroteWarning(echo, dc, docdiff.Options{}, "run it again")
	if w == nil || w.Code != WarnHookRewroteUnsent || fmt.Sprint(w.Paths) != "[meta]" {
		t.Fatalf("warning = %+v", w)
	}
	echo["meta"] = map[string]any{"title": "M"}
	if w := hookRewroteWarning(echo, dc, docdiff.Options{}, "x"); w != nil {
		t.Errorf("an intact echo warned: %+v", w)
	}
}

// ---------------------------------------------------------------------------
// $ref against relationTo
// ---------------------------------------------------------------------------

// A reference naming a collection its field does not accept used to resolve
// to that collection's id and link an unrelated document (Postgres ids
// collide). pages.hero.media accepts media only (graphql).
func TestRefChecksRelationTo(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	m.seed("media", 4, map[string]any{"alt": "a", "filename": "a.png"})
	home := m.home(t)
	bad := m.run(t, home, nil, "update", "pages", "16", "--set-json", `hero={"type":"lowImpact","media":{"$ref":"pages","slug":"home"}}`, "--dry-run")
	if bad.Code == 0 || bad.code(t) != string(apierr.CodeBadRequestBody) || !strings.Contains(bad.Stdout, "accepts only media") {
		t.Errorf("wrong collection: exit %d\n%s", bad.Code, bad.Stdout)
	}
	good := m.run(t, home, nil, "update", "pages", "16", "--set-json", `hero={"type":"lowImpact","media":{"$ref":"media","filename":"a.png"}}`, "--dry-run")
	if good.Code != 0 || good.hasWarning(t, WarnRefTargetUnchecked) {
		t.Errorf("right collection: exit %d warnings %v\n%s", good.Code, good.warnings(t), good.Stdout)
	}
	// A polymorphic relationship: the collection must be one of relationTo.
	poly := m.run(t, home, nil, "update", "pages", "16", "--set-json",
		`hero={"type":"lowImpact","links":[{"link":{"type":"reference","reference":{"$ref":"categories","slug":"news","$as":"polymorphic"}}}]}`, "--dry-run")
	if poly.Code == 0 || !strings.Contains(poly.Stdout, "pages, posts") {
		t.Errorf("polymorphic: exit %d\n%s", poly.Code, poly.Stdout)
	}
	// A {relationTo, value} pair (a Lexical link's fields.doc) must agree.
	link := map[string]any{"type": "link", "fields": map[string]any{"linkType": "internal",
		"doc": map[string]any{"relationTo": "pages", "value": map[string]any{"$ref": "categories", "slug": "news"}}}}
	rt := map[string]any{"root": map[string]any{"type": "root", "children": []any{map[string]any{"type": "paragraph", "children": []any{link}}}}}
	sib := m.run(t, home, nil, "update", "pages", "16", "--set-json", "layout="+mustJSON(t, []any{map[string]any{"blockType": "content", "richText": rt}}), "--dry-run")
	if sib.Code == 0 || !strings.Contains(sib.Stdout, "relationTo next to it") {
		t.Errorf("sibling relationTo: exit %d\n%s", sib.Code, sib.Stdout)
	}
}

// Plan-local references are checked too: a page's hero.media pointing at a
// categories item of the same plan.
func TestPlanLocalRefChecksRelationTo(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "m.json", `[{"key":"cat","collection":"categories","data":{"title":"C","slug":"c"}},
		{"key":"page","collection":"pages","data":{"title":"P","slug":"p","hero":{"type":"lowImpact","media":{"$ref":"@cat"}}}}]`)
	res := m.run(t, home, nil, "plan", dir)
	if res.Code == 0 || !strings.Contains(res.Stdout, "accepts only media") {
		t.Errorf("exit %d\n%s", res.Code, res.Stdout)
	}
	if page := itemByKey(t, planItems(t, res), "page"); page["action"] != actionError {
		t.Errorf("page = %v", page)
	}
}

// A field whose relationTo is unknown cannot be checked: said once.
func TestRefUncheckedWarning(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	res := m.run(t, home, nil, "update", "pages", "16", "--set-json", `parent={"$ref":"pages","slug":"home"}`, "--dry-run")
	if res.Code != 0 || !res.hasWarning(t, WarnRefTargetUnchecked) {
		t.Errorf("exit %d warnings %v", res.Code, res.warnings(t))
	}
}

func TestSplitDocPath(t *testing.T) {
	got := fmt.Sprint(splitDocPath("layout[2].blocks[0].link.doc"))
	if got != "[{layout -1} { 2} {blocks -1} { 0} {link -1} {doc -1}]" {
		t.Errorf("segments = %s", got)
	}
	if schemaPath("hero.links[0].link.reference") != "hero.links.link.reference" {
		t.Error(schemaPath("hero.links[0].link.reference"))
	}
}

// ---------------------------------------------------------------------------
// smaller fixes
// ---------------------------------------------------------------------------

// A relationship id written as a string ("4", ["66"]) where Payload stores
// numbers used to plan "update" forever.
func TestAlignToStoredNumericStringIDs(t *testing.T) {
	stored := map[string]any{"heroImage": json.Number("4"), "authors": []any{json.Number("66"), float64(67)}, "title": "4"}
	body := map[string]any{"heroImage": "4", "authors": []any{"66", "67"}, "title": "4"}
	got := alignToStored(stored, body, nil)
	if res := docdiff.DiffPatch(stored, got, docdiff.Options{}); !res.Identical {
		t.Errorf("aligned body still differs: %v", res.Changes)
	}
	if !sameNumber(json.Number("4"), "4") || sameNumber("4", "4") || sameNumber(json.Number("4"), "4a") {
		t.Error("sameNumber")
	}
}

// A failed sync item's validation hint speaks manifest: no --draft flag.
func TestSyncValidationHintSpeaksManifest(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "m.json", `{"collection":"pages","data":{"title":"FAIL","slug":"f"}}`)
	res := m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code == 0 {
		t.Fatalf("the item must fail:\n%s", res.Stdout)
	}
	it := planItems(t, res)[0]
	hint, _ := it["error"].(map[string]any)["hint"].(string)
	if strings.Contains(hint, "--draft") || !strings.Contains(hint, `"status": "draft"`) || !strings.Contains(hint, "pay describe pages --required-only") {
		t.Errorf("hint = %q", hint)
	}
}

// An unchanged upsert sends nothing, so it has no block advice to give.
func TestUnchangedUpsertHasNoBlockWarnings(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 60, map[string]any{"title": "A", "slug": "b", "_status": "draft",
		"layout": []any{map[string]any{"id": "r1", "blockType": "content"}}})
	home := m.home(t)
	res := m.run(t, home, nil, "upsert", "pages", "--data", `{"title":"A","slug":"b","layout":[{"blockType":"content"}]}`)
	if upsertMeta(t, res)["action"] != "unchanged" {
		t.Fatalf("meta = %v", upsertMeta(t, res))
	}
	for _, w := range res.warnings(t) {
		if blockCheckCodes[fmt.Sprint(w["code"])] {
			t.Errorf("block warning on an unchanged upsert: %v", w)
		}
	}
}

// [t](pay:pages/about) names a page by a string where ids are numbers.
func TestRelationIDNotNumericWarning(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	res := m.run(t, home, nil, "update", "pages", "16", "--set-md", "hero.richText=See [about](pay:pages/about).", "--dry-run")
	if res.Code != 0 || !res.hasWarning(t, WarnRelationIDNotNumeric) {
		t.Errorf("exit %d warnings %v\n%s", res.Code, res.warnings(t), res.Stdout)
	}
	// pay:pages/slug=home is a reference, resolved to the id.
	res = m.run(t, home, nil, "update", "pages", "16", "--set-md", "hero.richText=See [home](pay:pages/slug=home).", "--dry-run")
	body, _ := json.Marshal(res.data(t)["request"].(map[string]any)["body"])
	if res.Code != 0 || res.hasWarning(t, WarnRelationIDNotNumeric) || !strings.Contains(string(body), `"value":16`) {
		t.Errorf("exit %d warnings %v body %s", res.Code, res.warnings(t), body)
	}
}
