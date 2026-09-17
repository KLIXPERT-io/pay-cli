package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// §10.2 — the narrowed echo check must never assert what it did not observe
// ---------------------------------------------------------------------------

// writtenClaims are the phrases a warning about an UNCHECKED field must never
// contain. The narrowed echo check is an absence of evidence: --select kept the
// field out of the echo, so PayCLI saw nothing about it and any statement that
// it was written is fabricated. The first fix for finding 9 ended
// "They were still written." for fields Payload had in fact discarded — the
// confidently-wrong answer §10.2 exists to prevent, and worse than the false
// positive it replaced, because a false alarm makes an agent look while a false
// assurance stops it looking.
var writtenClaims = []string{
	"still written",
	"were written",
	"was written",
	"were stored",
	"was stored",
	"were saved",
	"was saved",
	"they were applied",
}

func assertNoWrittenClaim(t *testing.T, where, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, claim := range writtenClaims {
		if strings.Contains(low, claim) {
			t.Errorf("%s asserts an unobserved fact (%q):\n%s", where, claim, text)
		}
	}
}

// TestNarrowedEchoCheckNeverClaimsTheFieldsWereWritten is the regression for
// the wording AND the semantics: the same write reports the same field as
// silently dropped when the echo is complete, so the narrowed branch cannot
// call it written — it can only say it could not look.
func TestNarrowedEchoCheckNeverClaimsTheFieldsWereWritten(t *testing.T) {
	// `--set bogusfield=1 --set title=abc4`: Payload discards bogusfield.
	sent := map[string]any{"title": "abc4", "bogusfield": float64(1)}
	// What Payload echoes for --select title: the projection, nothing else.
	narrowedEcho := payload.Doc{"id": 33, "title": "abc4"}
	// The identical write without --select: the full document, still no
	// bogusfield, because it never reached the database.
	fullEcho := payload.Doc{"id": 33, "title": "abc4", "slug": "scratch"}

	full := echoWarnings("pages", sent, fullEcho, nil, echoConfig{})
	dropped := warningWithCode(full, output.WarnInputSilentlyDropped)
	if dropped == nil {
		t.Fatalf("precondition: without --select the field is reported dropped: %v", codesOf(full))
	}

	narrowed := warningWithCode(echoWarnings("pages", sent, narrowedEcho, []string{"title"}, echoConfig{}),
		warnEchoCheckNarrowed)
	if narrowed == nil {
		t.Fatalf("the narrowing must still be announced")
	}
	if len(narrowed.Paths) != 1 || narrowed.Paths[0] != "bogusfield" {
		t.Fatalf("paths = %v, want the unchecked field", narrowed.Paths)
	}
	// The two runs describe the SAME write. Whatever the narrowed branch says
	// about bogusfield must stay true in a world where it was discarded.
	assertNoWrittenClaim(t, "echo_check_narrowed message", narrowed.Message)
	assertNoWrittenClaim(t, "echo_check_narrowed hint", narrowed.Hint)

	if !strings.Contains(narrowed.Message, "UNKNOWN") && !strings.Contains(narrowed.Message, "could NOT verify") {
		t.Errorf("the message must say the paths are unverified, not merely unchecked:\n%s", narrowed.Message)
	}
	// §11: a warning an agent cannot act on is noise. Say how to look.
	if !strings.Contains(narrowed.Hint, "--select") || !strings.Contains(narrowed.Hint, "pay get pages") {
		t.Errorf("the hint must tell the agent to re-read without --select:\n%s", narrowed.Hint)
	}
}

// TestUpdateWithSelectDoesNotVouchForDroppedInput drives the whole command, the
// way the reported reproduction did: `pay update pages 33 --set bogusfield=1
// --set title=abc4 --select title` used to end its warning with "They were
// still written." for a field Payload had thrown away.
func TestUpdateWithSelectDoesNotVouchForDroppedInput(t *testing.T) {
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("PATCH", "/api/pages/33", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			// Payload honours ?select[title]=true: title and the id only.
			_, _ = io.WriteString(w, `{"doc":{"id":33,"title":"abc4"},"message":"Updated successfully."}`)
		}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"update", "pages", "33", "--set", "bogusfield=1", "--set", "title=abc4",
			"--select", "title", "--draft", "--yes"},
		Env: seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	var found bool
	for _, w := range res.warnings(t) {
		if w["code"] != warnEchoCheckNarrowed {
			continue
		}
		found = true
		assertNoWrittenClaim(t, "envelope warning message", str(w["message"]))
		assertNoWrittenClaim(t, "envelope warning hint", str(w["hint"]))
	}
	if !found {
		t.Fatalf("the narrowed echo check disappeared from the envelope: %v", res.warnings(t))
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// §7.9a / conflict 35 — the write verbs resolve --locale like the read verbs
// ---------------------------------------------------------------------------

// seedLocalizedDiscovery is seedDiscovery with the project's localisation
// capability replaced, so a test can describe a localised project without
// re-recording the fixtures.
func seedLocalizedDiscovery(t *testing.T, home, baseURL string, loc discovery.Localization) {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL:        baseURL,
		APIPath:        "/api",
		GraphQLPath:    "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	m.Capabilities.Localization = loc

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
}

func localisedProject(enabled *bool) discovery.Localization {
	return discovery.Localization{
		Enabled:       enabled,
		EnabledSource: discovery.SourceConfigured,
		Locales:       []string{"en", "de"},
		LocalesSource: discovery.SourceConfigured,
	}
}

// TestWriteVerbsSendFallbackLocale is finding 19's write half. update.go and
// create.go hand-built query.Params and never set FallbackLocale, so
// `--locale de` went out bare; Payload's default is fallback:true, so every
// untranslated field came back in the DEFAULT locale and the echoed document
// presented English as a German translation. The write verbs must resolve the
// locale through the same helper `find` uses.
func TestWriteVerbsSendFallbackLocale(t *testing.T) {
	yes := true
	ok := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, body)
		}
	}
	tests := []struct {
		name    string
		args    []string
		method  string
		path    string
		enabled *bool
		handle  http.HandlerFunc
	}{
		{
			// §7.9a: UNKNOWN IS NOT OFF. Localization.Enabled is fed only by
			// the GraphQL schema, so on a graphQL:{disable:true} project it
			// stays nil however localised the project really is — and a write
			// into `de` must still say "do not fall back".
			name:    "update when localisation is unknown",
			args:    []string{"update", "pages", "33", "--set", "title=Hallo", "--locale", "de", "--draft", "--yes"},
			method:  "PATCH",
			path:    "/api/pages/33",
			enabled: nil,
			handle:  ok(`{"doc":{"id":33,"title":"Hallo"},"message":"Updated successfully."}`),
		},
		{
			name:   "update",
			args:   []string{"update", "pages", "33", "--set", "title=Hallo", "--locale", "de", "--draft", "--yes"},
			method: "PATCH", path: "/api/pages/33", enabled: &yes,
			handle: ok(`{"doc":{"id":33,"title":"Hallo"},"message":"Updated successfully."}`),
		},
		{
			name:   "create",
			args:   []string{"create", "pages", "--set", "title=Hallo", "--locale", "de", "--draft", "--yes"},
			method: "POST", path: "/api/pages", enabled: &yes,
			handle: ok(`{"doc":{"id":34,"title":"Hallo"},"message":"created"}`),
		},
		// The verbs below all echo a document back too, and every one of them
		// still hand-built query.Params{Locale: locale} with no
		// FallbackLocale after update/create were routed. `pay publish pages 33
		// --locale de` answered with the English title as if it were the German
		// one, exactly like `pay update` did.
		{
			name:   "publish",
			args:   []string{"publish", "pages", "33", "--locale", "de", "--yes", "--no-pre-validate"},
			method: "PATCH", path: "/api/pages/33", enabled: &yes,
			handle: ok(`{"doc":{"id":33,"title":"Hallo","_status":"published"},"message":"Updated successfully."}`),
		},
		{
			name:   "unpublish",
			args:   []string{"unpublish", "pages", "33", "--locale", "de", "--yes"},
			method: "PATCH", path: "/api/pages/33", enabled: &yes,
			handle: ok(`{"doc":{"id":33,"title":"Hallo","_status":"draft"},"message":"Updated successfully."}`),
		},
		{
			name:   "duplicate",
			args:   []string{"duplicate", "pages", "33", "--locale", "de", "--yes"},
			method: "POST", path: "/api/pages/33/duplicate", enabled: &yes,
			handle: ok(`{"doc":{"id":34,"title":"Hallo"},"message":"created"}`),
		},
		{
			name:   "delete",
			args:   []string{"delete", "pages", "33", "--locale", "de", "--yes"},
			method: "DELETE", path: "/api/pages/33", enabled: &yes,
			handle: ok(`{"doc":{"id":33,"title":"Hallo"},"message":"Deleted successfully."}`),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := payloadtest.NewServer(t, payloadtest.WithHandler(tc.method, tc.path, tc.handle))
			home := t.TempDir()
			seedLocalizedDiscovery(t, home, srv.URL, localisedProject(tc.enabled))

			res := cliRun(t, invocation{Home: home, Args: tc.args, Env: seededEnv(srv.URL)})
			if res.Code != 0 {
				t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
			}

			var query string
			for _, r := range srv.Requests() {
				if r.Method == tc.method && r.Path == tc.path {
					query = r.Query
				}
			}
			if query == "" {
				t.Fatalf("the %s never reached the server: %+v", tc.method, srv.Requests())
			}
			t.Logf("%s %s?%s", tc.method, tc.path, query)
			if !strings.Contains(query, "locale=de") {
				t.Fatalf("%s %s?%s does not carry the requested locale", tc.method, tc.path, query)
			}
			if !strings.Contains(query, "fallback-locale="+discovery.FallbackNone) {
				t.Fatalf("%s %s?%s has no fallback-locale: Payload falls back to the DEFAULT locale, "+
					"so the echoed document shows default-locale text as a %q translation",
					tc.method, tc.path, query, "de")
			}
			// §10.1: the envelope must disclose what was actually sent, or the
			// agent cannot tell a null from an untranslated field.
			meta, _ := res.Env["meta"].(map[string]any)
			loc, _ := meta["locale"].(map[string]any)
			if str(loc["requested"]) != "de" || str(loc["fallback"]) != discovery.FallbackNone {
				enc, _ := json.Marshal(loc)
				t.Errorf("meta.locale = %s, want {requested: de, fallback: none}", enc)
			}
		})
	}
}

// TestWriteVerbsValidateTheLocaleBeforeWriting is the other half of routing the
// write verbs through resolveLocale: a locale the project does not have is
// rejected before the request is sent, instead of silently writing into a
// locale that does not exist.
func TestWriteVerbsValidateTheLocaleBeforeWriting(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name   string
		args   []string
		method string
	}{
		{"update", []string{"update", "pages", "33", "--set", "title=x", "--locale", "dee", "--yes"}, "PATCH"},
		{"create", []string{"create", "pages", "--set", "title=x", "--locale", "dee", "--yes"}, "POST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := payloadtest.NewServer(t)
			home := t.TempDir()
			seedLocalizedDiscovery(t, home, srv.URL, localisedProject(&yes))

			res := cliRun(t, invocation{Home: home, Args: tc.args, Env: seededEnv(srv.URL)})
			if res.Code == 0 {
				t.Fatalf("exit = 0; a write into an unknown locale must fail\n%s", res.Stdout)
			}
			if got := res.code(t); got != "invalid_option" {
				t.Errorf("error.code = %q, want invalid_option", got)
			}
			for _, r := range srv.Requests() {
				if r.Method == tc.method {
					t.Fatalf("the write was sent anyway: %s %s?%s", r.Method, r.Path, r.Query)
				}
			}
		})
	}
}

// seedLocalizedDiscoveryWithFlags is seedLocalizedDiscovery for the verbs whose
// route is gated on a capability the recorded fixture does not have (`restore`
// needs trash). It mutates only the flags asked for, so the rest of the project
// is still the real recorded one.
func seedLocalizedDiscoveryWithFlags(t *testing.T, home, baseURL string, loc discovery.Localization,
	slug string, mutate func(*discovery.Flags)) {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL: baseURL, APIPath: "/api", GraphQLPath: "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	m.Capabilities.Localization = loc
	found := false
	for i := range m.Collections {
		if m.Collections[i].Slug == slug {
			mutate(&m.Collections[i].Flags)
			found = true
		}
	}
	if !found {
		t.Fatalf("the recorded manifest has no %q to adjust", slug)
	}

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation, m.Meta.Scope, m.GeneratedAt = generation, sc.Key, now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	if ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now); !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
}

// TestRestoreSendsFallbackLocale covers the last collection verb that built its
// own query.Params. `restore` un-deletes a document and echoes it back, so the
// same fabricated-translation hazard applies.
func TestRestoreSendsFallbackLocale(t *testing.T) {
	yes := true
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		"PATCH", "/api/pages/33", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, `{"doc":{"id":33,"title":"Hallo"},"message":"Updated successfully."}`)
		}))
	home := t.TempDir()
	seedLocalizedDiscoveryWithFlags(t, home, srv.URL, localisedProject(&yes), "pages",
		func(f *discovery.Flags) { f.Trash = &yes })

	res := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"restore", "pages", "33", "--locale", "de", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	assertSentLocale(t, srv, "PATCH", "/api/pages/33")
	assertMetaLocale(t, res)
}

// TestGlobalsUpdateSendsFallbackLocale: `globals get` was routed through
// resolveLocale and `globals update` — the half that WRITES — was not, so the two
// halves of the same command disagreed about what `--locale de` means.
func TestGlobalsUpdateSendsFallbackLocale(t *testing.T) {
	yes := true
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		"POST", "/api/globals/header", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, `{"result":{"id":"1","navTitle":"Hallo"},"message":"Updated successfully."}`)
		}))
	home := t.TempDir()
	seedLocalizedDiscovery(t, home, srv.URL, localisedProject(&yes))

	res := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"globals", "update", "header", "--set", "navTitle=Hallo", "--locale", "de", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	assertSentLocale(t, srv, "POST", "/api/globals/header")
}

// assertSentLocale is the §7.9a wire assertion: locale=de AND fallback-locale.
func assertSentLocale(t *testing.T, srv *payloadtest.Server, method, path string) {
	t.Helper()
	var q string
	for _, r := range srv.Requests() {
		if r.Method == method && r.Path == path {
			q = r.Query
		}
	}
	if q == "" {
		t.Fatalf("the %s never reached the server: %+v", method, srv.Requests())
	}
	if !strings.Contains(q, "locale=de") {
		t.Fatalf("%s %s?%s does not carry the requested locale", method, path, q)
	}
	if !strings.Contains(q, "fallback-locale="+discovery.FallbackNone) {
		t.Fatalf("%s %s?%s has no fallback-locale: the echoed document shows DEFAULT-locale text "+
			"as a %q translation", method, path, q, "de")
	}
}

// assertMetaLocale is §10.1's half: what went out has to be visible in meta, or
// the agent cannot tell an untranslated field from a null one.
func assertMetaLocale(t *testing.T, res cliResult) {
	t.Helper()
	meta, _ := res.Env["meta"].(map[string]any)
	loc, _ := meta["locale"].(map[string]any)
	if str(loc["requested"]) != "de" || str(loc["fallback"]) != discovery.FallbackNone {
		enc, _ := json.Marshal(loc)
		t.Errorf("meta.locale = %s, want {requested: de, fallback: none}", enc)
	}
}
