package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// testPreviewSecret is what the fake site's /next/preview route accepts.
const testPreviewSecret = "fake-preview-secret-0123456789"

// fakeSite is a tiny stateful Payload + website-template stand-in for the F9
// commands: `pages` with the four states a URL/preview/publish command must
// tell apart, and the template's /next/preview route.
//
//	1  about         published, nested (breadcrumbs), newest version published
//	2  draft-only    never published: stored _status draft
//	3  changed       published as "changed", newest draft renamed "changed-new"
//	4,5 dup          two documents share a slug
//	6  home          published, slug home
//	7  broken        publishing it fails validation
type fakeSite struct {
	*payloadtest.Server
	mu        sync.Mutex
	stored    map[string]map[string]any
	newest    map[string]map[string]any
	patches   []string
	handshake []string
}

func page(id int, slug, status string, extra map[string]any) map[string]any {
	d := map[string]any{"id": id, "slug": slug, "title": strings.ToUpper(slug), "_status": status}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	fs := &fakeSite{
		stored: map[string]map[string]any{
			"1": page(1, "about", "published", map[string]any{"breadcrumbs": []any{
				map[string]any{"url": "/company", "label": "Company"},
				map[string]any{"url": "/company/about", "label": "About"}}}),
			"2": page(2, "draft-only", "draft", nil),
			"3": page(3, "changed", "published", nil),
			"4": page(4, "dup", "published", nil),
			"5": page(5, "dup", "published", nil),
			"6": page(6, "home", "published", nil),
			"7": page(7, "broken", "draft", nil),
		},
	}
	fs.newest = map[string]map[string]any{}
	for id, d := range fs.stored {
		cp := map[string]any{}
		for k, v := range d {
			cp[k] = v
		}
		fs.newest[id] = cp
	}
	fs.newest["3"]["slug"] = "changed-new"
	fs.newest["3"]["_status"] = "draft"

	opts := []payloadtest.Option{
		// A document that does not exist: Payload's own 404 body.
		payloadtest.WithHandler("PATCH", "/api/pages/99", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 404, map[string]any{"errors": []any{map[string]any{"message": "Not Found"}}})
		}),
		payloadtest.WithHandler("GET", "/api/pages", fs.find),
		payloadtest.WithHandler("GET", "/next/preview", fs.preview),
	}
	for id := range fs.stored {
		opts = append(opts,
			payloadtest.WithHandler("GET", "/api/pages/"+id, fs.get(id)),
			payloadtest.WithHandler("PATCH", "/api/pages/"+id, fs.patch(id)))
	}
	fs.Server = payloadtest.NewServer(t, opts...)
	return fs
}

func (fs *fakeSite) get(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		doc := fs.stored[id]
		if r.URL.Query().Get("draft") == "true" {
			doc = fs.newest[id]
		}
		writeJSON(w, 200, doc)
	}
}

func (fs *fakeSite) find(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var where map[string]map[string]any
	_ = json.Unmarshal([]byte(r.URL.Query().Get("where")), &where)
	want, _ := where["slug"]["equals"].(string)
	pool := fs.stored
	if r.URL.Query().Get("draft") == "true" {
		pool = fs.newest
	}
	docs := []any{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7"} {
		if d := pool[id]; d["slug"] == want {
			docs = append(docs, map[string]any{"id": d["id"], "slug": d["slug"]})
		}
	}
	writeJSON(w, 200, map[string]any{"docs": docs, "totalDocs": len(docs), "limit": 2, "page": 1, "totalPages": 1})
}

func (fs *fakeSite) patch(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.patches = append(fs.patches, id)
		if id == "7" {
			writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{
				"name": "ValidationError", "message": "The following field is invalid: Title",
				"data": map[string]any{"errors": []any{map[string]any{"path": "title", "message": "required"}}}}}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		doc := fs.stored[id]
		doc["_status"] = in["_status"]
		writeJSON(w, 200, map[string]any{"doc": doc, "message": "Updated successfully."})
	}
}

func (fs *fakeSite) preview(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	fs.handshake = append(fs.handshake, r.URL.RawQuery)
	fs.mu.Unlock()
	if r.URL.Query().Get("previewSecret") != testPreviewSecret {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "You are not allowed to preview this page")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__prerender_bypass", Value: "bypass-cookie-value", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode})
	w.Header().Set("Location", r.URL.Query().Get("path"))
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func urlRun(t *testing.T, fs *fakeSite, home string, env []string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Home: home, Args: args, Env: append(seededEnv(fs.URL), env...)})
}

func dataString(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

// F9's path ladder, one row per source, plus the status pair.
func TestURLResolvesThePathAndTheStatus(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	cases := []struct {
		name, want, source, status string
		changed                    bool
		args                       []string
	}{
		{"breadcrumbs win", "/company/about", PathSourceBreadcrumbs, "published", false, []string{"url", "pages", "1"}},
		{"slug fallback", "/draft-only", PathSourceSlug, "draft", true, []string{"url", "pages", "2"}},
		{"home is served at /", "/", PathSourceHome, "published", false, []string{"url", "pages", "6"}},
		{"stored slug by default", "/changed", PathSourceSlug, "published", true, []string{"url", "pages", "3"}},
		{"--draft reads the newest draft", "/changed-new", PathSourceSlug, "published", true, []string{"url", "pages", "3", "--draft"}},
		{"--slug resolves first", "/company/about", PathSourceBreadcrumbs, "published", false, []string{"url", "pages", "--slug", "about"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := urlRun(t, fs, home, nil, tc.args...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			if res.Env["data_kind"] != "op_result" {
				t.Fatalf("data_kind = %v", res.Env["data_kind"])
			}
			d := res.data(t)
			if dataString(d, "path") != tc.want || dataString(d, "path_source") != tc.source {
				t.Fatalf("path = %v (%v), want %s (%s)", d["path"], d["path_source"], tc.want, tc.source)
			}
			if dataString(d, "url") != fs.URL+tc.want {
				t.Errorf("url = %v, want site_url + path", d["url"])
			}
			if dataString(d, "site_url_source") != "derived:base_url" {
				t.Errorf("site_url_source = %v", d["site_url_source"])
			}
			if dataString(d, "status") != tc.status || d["has_unpublished_changes"] != tc.changed {
				t.Errorf("status = %v, changes = %v; want %s, %v", d["status"], d["has_unpublished_changes"], tc.status, tc.changed)
			}
		})
	}

	t.Run("a never-published page warns that the URL is not live", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "url", "pages", "2")
		found := false
		for _, w := range res.warnings(t) {
			if w["code"] == WarnURLNotLive {
				found = true
				if !strings.Contains(w["hint"].(string), "pay url pages 2 --preview") {
					t.Errorf("hint = %v", w["hint"])
				}
			}
		}
		if !found {
			t.Fatalf("no %s warning: %s", WarnURLNotLive, res.Stdout)
		}
	})
	t.Run("--draft reports where the published page still is", func(t *testing.T) {
		d := urlRun(t, fs, home, nil, "url", "pages", "3", "--draft").data(t)
		if dataString(d, "published_path") != "/changed" {
			t.Fatalf("published_path = %v", d["published_path"])
		}
	})
}

func TestURLSlugErrors(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	amb := urlRun(t, fs, home, nil, "url", "pages", "--slug", "dup")
	if amb.Code != apierr.ExitValidation || amb.code(t) != string(apierr.CodeSlugAmbiguous) {
		t.Fatalf("ambiguous slug: exit %d code %s\n%s", amb.Code, amb.code(t), amb.Stdout)
	}
	e := amb.Env["error"].(map[string]any)
	if dym, _ := e["did_you_mean"].([]any); len(dym) != 2 {
		t.Errorf("did_you_mean = %v, want both ids", e["did_you_mean"])
	}

	none := urlRun(t, fs, home, nil, "url", "pages", "--slug", "changed-new")
	if none.Code != apierr.ExitNotFound || none.code(t) != string(apierr.CodeDocNotFound) {
		t.Fatalf("missing slug: exit %d code %s", none.Code, none.code(t))
	}
	if hint := none.Env["error"].(map[string]any)["hint"].(string); !strings.Contains(hint, "--draft") {
		t.Errorf("a slug that exists only in a draft must point at --draft: %q", hint)
	}
	// ...and with --draft it is found.
	if res := urlRun(t, fs, home, nil, "url", "pages", "--slug", "changed-new", "--draft"); res.Code != 0 {
		t.Fatalf("--draft --slug: exit %d\n%s", res.Code, res.Stdout)
	}

	both := urlRun(t, fs, home, nil, "url", "pages", "1", "--slug", "about")
	if both.code(t) != string(apierr.CodeInvalidArgs) {
		t.Errorf("id and --slug together: %s", both.code(t))
	}
}

// The handshake: no redirect followed, the cookie saved 0600, its value and
// the secret nowhere in the envelope.
func TestURLPreviewHandshake(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	res := urlRun(t, fs, home, []string{"PAY_PREVIEW_SECRET=" + testPreviewSecret}, "url", "pages", "3", "--preview", "-v")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	for where, text := range map[string]string{"stdout": res.Stdout, "stderr (-v)": res.Stderr} {
		if strings.Contains(text, testPreviewSecret) {
			t.Fatalf("%s contains the preview secret:\n%s", where, text)
		}
		if strings.Contains(text, "bypass-cookie-value") {
			t.Fatalf("%s contains the cookie value:\n%s", where, text)
		}
	}
	if !strings.Contains(res.Stderr, "/next/preview") || !strings.Contains(res.Stderr, "previewSecret=") {
		t.Errorf("-v did not log the handshake request, so the scrub above proved nothing:\n%s", res.Stderr)
	}
	d := res.data(t)
	if dataString(d, "path") != "/changed-new" || d["draft"] != true {
		t.Fatalf("--preview must resolve the draft path: %v draft=%v", d["path"], d["draft"])
	}
	p := d["preview"].(map[string]any)
	if dataString(p, "key_source") != "env:PAY_PREVIEW_SECRET" || dataString(p, "key_fingerprint") == "" {
		t.Errorf("key_source/key_fingerprint = %v / %v", p["key_source"], p["key_fingerprint"])
	}
	hs := p["handshake"].(map[string]any)
	if !strings.Contains(dataString(hs, "url"), "previewSecret=<redacted>") || !strings.Contains(dataString(hs, "url"), "path=%2Fchanged-new") {
		t.Errorf("handshake url = %v", hs["url"])
	}
	if hs["status"].(json.Number).String() != "307" || dataString(hs, "location") != "/changed-new" {
		t.Errorf("handshake = %v", hs)
	}
	cookies := p["cookies"].([]any)
	if len(cookies) != 1 || cookies[0].(map[string]any)["name"] != "__prerender_bypass" {
		t.Fatalf("cookies = %v", cookies)
	}
	if _, has := cookies[0].(map[string]any)["value"]; has {
		t.Fatal("the envelope carries a cookie value")
	}

	file := dataString(p, "cookie_file")
	if !strings.HasPrefix(file, filepath.Join(home, "state")) {
		t.Errorf("default cookie file %q is not under the state dir", file)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != CookieFilePerm {
		t.Fatalf("cookie file mode = %v, %v", info.Mode().Perm(), err)
	}
	var recs []cookieRecord
	raw, _ := os.ReadFile(file)
	if err := json.Unmarshal(raw, &recs); err != nil || len(recs) != 1 {
		t.Fatalf("cookie file = %s (%v)", raw, err)
	}
	host := strings.TrimPrefix(strings.Split(fs.URL, ":")[1], "//")
	if r := recs[0]; r.Value != "bypass-cookie-value" || r.Domain != host || r.Path != "/" ||
		!r.HTTPOnly || !r.Secure || r.SameSite != "None" || r.Expires != -1 {
		t.Errorf("cookie record = %+v", r)
	}
	if ab := p["load"].(map[string]any)["agent_browser"].(string); !strings.Contains(ab, "agent-browser cookies set --curl") ||
		!strings.Contains(ab, "--domain "+host) {
		t.Errorf("load hints = %v", p["load"])
	}

	t.Run("--cookie-file relative to the working directory", func(t *testing.T) {
		res := cliRun(t, invocation{Home: home, WorkDir: home, Args: []string{"url", "pages", "1", "--preview", "--cookie-file", "c.json"},
			Env: append(seededEnv(fs.URL), "PAY_PREVIEW_SECRET="+testPreviewSecret)})
		if res.Code != 0 {
			t.Fatalf("exit %d: %s", res.Code, res.Stdout)
		}
		if _, err := os.Stat(filepath.Join(home, "c.json")); err != nil {
			t.Fatalf("cookie file not written where asked: %v", err)
		}
	})
}

func TestURLPreviewFailures(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	t.Run("no secret configured: exit 9 before any request", func(t *testing.T) {
		fs.Reset()
		res := urlRun(t, fs, home, nil, "url", "pages", "1", "--preview")
		if res.Code != apierr.ExitConfig || res.code(t) != string(apierr.CodePreviewSecretMissing) {
			t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
		}
		e := res.Env["error"].(map[string]any)
		for _, want := range []string{"PAY_PREVIEW_SECRET", "preview_secret_env", "pay auth preview-secret --stdin"} {
			if !strings.Contains(e["message"].(string)+" "+e["hint"].(string), want) {
				t.Errorf("the error does not name %q:\n%v\n%v", want, e["message"], e["hint"])
			}
		}
		if n := fs.Count("GET", "/next/preview"); n != 0 {
			t.Errorf("the handshake was attempted %d time(s) without a secret", n)
		}
	})
	t.Run("a wrong secret is preview_denied (exit 8)", func(t *testing.T) {
		res := urlRun(t, fs, home, []string{"PAY_PREVIEW_SECRET=wrong-secret-wrong-secret"}, "url", "pages", "1", "--preview")
		if res.Code != apierr.ExitAccessDenied || res.code(t) != string(apierr.CodePreviewDenied) {
			t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
		}
		if strings.Contains(res.Stdout, "wrong-secret-wrong-secret") {
			t.Fatal("the error echoes the secret")
		}
	})
	t.Run("no preview route is preview_failed (exit 10)", func(t *testing.T) {
		res := urlRun(t, fs, home, []string{"PAY_PREVIEW_SECRET=" + testPreviewSecret}, "url", "pages", "1", "--preview",
			"--config", writeConfig(t, home, "[profiles.default]\npreview_path = \"/nope?p={path}&s={secret}\"\n"))
		if res.Code != apierr.ExitCapability || res.code(t) != string(apierr.CodePreviewFailed) {
			t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
		}
		if strings.Contains(res.Stdout, testPreviewSecret) {
			t.Fatal("the error echoes the secret")
		}
	})
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "extra-config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveFrontendPath(t *testing.T) {
	doc := payload.Doc{"id": 9, "slug": "hello world", "category": map[string]any{"slug": "news"}}
	cases := []struct {
		name, tmpl, locale, want, source string
		doc                              payload.Doc
		wantErr                          bool
	}{
		{"slug is path-escaped", "", "", "/hello%20world", PathSourceSlug, doc, false},
		{"template with dotted and id", "/{locale}/blog/{category.slug}/{id}-{slug}", "de", "/de/blog/news/9-hello%20world", PathSourceTemplate, doc, false},
		{"template value in a query is query-escaped", "/search?q={slug}", "", "/search?q=hello+world", PathSourceTemplate, doc, false},
		{"a hole in the template is an error", "/blog/{author.slug}", "", "", "", doc, true},
		{"breadcrumbs beat a template", "/x/{slug}", "", "/a/b", PathSourceBreadcrumbs,
			payload.Doc{"id": 1, "slug": "b", "breadcrumbs": []any{map[string]any{"url": "/a/b"}}}, false},
		{"breadcrumb /home is served at /", "", "", "/", PathSourceHome,
			payload.Doc{"id": 1, "slug": "home", "breadcrumbs": []any{map[string]any{"url": "/home"}}}, false},
		{"nothing to go on", "", "", "", "", payload.Doc{"id": 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source, err := resolveFrontendPath("posts", tc.doc, tc.tmpl, tc.locale)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want || source != tc.source {
				t.Fatalf("got %q (%s), %v; want %q (%s)", got, source, err, tc.want, tc.source)
			}
		})
	}
}

// url_paths from a config file, and the url_path_guessed warning it silences.
func TestURLTemplateFromConfigAndTheGuessWarning(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)
	cfg := writeConfig(t, home, "[profiles.default]\nsite_url = \"https://www.example.com/\"\n[profiles.default.url_paths]\npages = \"/p/{slug}\"\n")
	res := urlRun(t, fs, home, nil, "url", "pages", "2", "--config", cfg)
	d := res.data(t)
	if dataString(d, "url") != "https://www.example.com/p/draft-only" || dataString(d, "path_source") != PathSourceTemplate {
		t.Fatalf("url = %v (%v)", d["url"], d["path_source"])
	}
	for _, w := range res.warnings(t) {
		if w["code"] == WarnURLPathGuessed {
			t.Fatalf("a configured template must not be reported as a guess: %v", w)
		}
	}
}

// `pay url --output id|table` used to render the typed result struct: id
// printed a Go struct dump with pointer addresses, table an empty "ID" column.
// The envelope carries plain JSON data now, so every renderer can read it.
func TestURLOutputIDAndTable(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	res := urlRun(t, fs, home, nil, "url", "pages", "1", "--output", "id")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	if got := strings.TrimSpace(res.Stdout); got != "1" {
		t.Fatalf("--output id = %q, want the document id", got)
	}

	res = urlRun(t, fs, home, nil, "url", "pages", "1", "--output", "table")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	for _, want := range []string{"PATH", "STATUS", "published", "about"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("--output table lacks %q:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "0x") || strings.Contains(res.Stdout, "&{") {
		t.Errorf("--output table renders a Go value:\n%s", res.Stdout)
	}

	res = urlRun(t, fs, home, nil, "url", "pages", "1", "--path", ".url", "--output", "id")
	if got := strings.TrimSpace(res.Stdout); got != fs.URL+"/company/about" {
		t.Fatalf("--path .url --output id = %q", got)
	}
}
