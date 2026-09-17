package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// §9.10.2 — the data flags are combinable and deep-merged, later winning
// ---------------------------------------------------------------------------

func TestWriteDataMergeOrder(t *testing.T) {
	tests := []struct {
		name string
		in   writeData
		want map[string]string // path -> JSON of the expected value
	}{
		{
			name: "set beats data",
			in: writeData{
				data: `{"title":"from-data","slug":"s"}`,
				set:  []string{"title=from-set"},
			},
			want: map[string]string{"title": `"from-set"`, "slug": `"s"`},
		},
		{
			name: "set-json beats set",
			in: writeData{
				set:     []string{"title=from-set"},
				setJSON: []string{`title="from-set-json"`},
			},
			want: map[string]string{"title": `"from-set-json"`},
		},
		{
			name: "alt is the lowest-precedence contributor",
			in: writeData{
				alt: "A",
				set: []string{"alt=B"},
			},
			want: map[string]string{"alt": `"B"`},
		},
		{
			name: "objects deep-merge",
			in: writeData{
				data:    `{"meta":{"a":1,"b":2}}`,
				setJSON: []string{`meta={"b":3}`},
			},
			want: map[string]string{"meta": `{"a":1,"b":3}`},
		},
		{
			name: "arrays are replaced, never merged",
			in: writeData{
				data:    `{"layout":[{"blockType":"cta"},{"blockType":"content"}]}`,
				setJSON: []string{`layout=[{"blockType":"mediaBlock"}]`},
			},
			want: map[string]string{"layout": `[{"blockType":"mediaBlock"}]`},
		},
		{
			name: "dotted set keys reach a group",
			in:   writeData{set: []string{"meta.title=x"}},
			want: map[string]string{"meta": `{"title":"x"}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := tc.in.build(testDeps(), &collTarget{Slug: "pages"}, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for path, want := range tc.want {
				got, err := json.Marshal(body[path])
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Fatalf("%s = %s, want %s", path, got, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §9.10.1 — --set typing and coercion
// ---------------------------------------------------------------------------

func TestSetCoercion(t *testing.T) {
	d := testDeps()
	tgt := testTarget()
	shard := tgt.Shard

	tests := []struct {
		name     string
		pair     string
		wantJSON string
		wantErr  apierr.Code
		wantWarn string
	}{
		{name: "relationship id is coerced to a number", pair: "heroImage=4", wantJSON: `4`},
		{
			name:    "an explicit string id is refused",
			pair:    `heroImage="4"`,
			wantErr: apierr.CodeInvalidArgs,
		},
		{name: "hasMany relationship splits on commas", pair: "tags=a,b", wantJSON: `["a","b"]`},
		{name: "number field", pair: "views=12", wantJSON: `12`},
		{name: "number field rejects a word", pair: "views=many", wantErr: apierr.CodeInvalidArgs},
		{name: "checkbox", pair: "featured=true", wantJSON: `true`},
		{name: "checkbox rejects a string", pair: "featured=yes", wantErr: apierr.CodeInvalidArgs},
		{name: "date accepts YYYY-MM-DD", pair: "publishedAt=2026-08-01", wantJSON: `"2026-08-01T00:00:00Z"`},
		{name: "date accepts RFC 3339", pair: "publishedAt=2026-08-01T09:30:00Z", wantJSON: `"2026-08-01T09:30:00Z"`},
		{name: "date rejects a relative form", pair: "publishedAt=7d", wantErr: apierr.CodeInvalidArgs},
		{name: "richText refuses --set", pair: "content=hello", wantErr: apierr.CodeInvalidArgs},
		{name: "blocks refuses --set", pair: "layout=cta", wantErr: apierr.CodeInvalidArgs},
		{name: "select rejects a value outside the enum", pair: "_status=archived", wantErr: apierr.CodeInvalidOption},
		{name: "select accepts a known value", pair: "_status=published", wantJSON: `"published"`},
		{name: "text keeps a numeric literal as a string", pair: "title=123", wantJSON: `"123"`},
		{
			name:     "an unknown field is sent with a warning, never rejected",
			pair:     "brandNew=hello",
			wantJSON: `"hello"`,
			wantWarn: warnWriteShapeUnknown,
		},
		{name: "null is passed through", pair: "slug=null", wantJSON: `null`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := writeData{set: []string{tc.pair}}
			body, warnings, err := w.build(d, tgt, shard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected %s, got body %v", tc.wantErr, body)
				}
				if got := apierr.CodeOf(err); got != tc.wantErr {
					t.Fatalf("code = %s, want %s (%v)", got, tc.wantErr, err)
				}
				if apierr.ExitCode(err) != apierr.ExitValidation {
					t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			key, _, _ := strings.Cut(tc.pair, "=")
			got, _ := json.Marshal(body[key])
			if string(got) != tc.wantJSON {
				t.Fatalf("%s = %s, want %s", key, got, tc.wantJSON)
			}
			if tc.wantWarn != "" {
				found := false
				for _, warn := range warnings {
					if warn.Code == tc.wantWarn {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing %s warning in %v", tc.wantWarn, warnings)
				}
			}
		})
	}
}

// TestRelationMismatchMessage pins §9.10.1's worked example.
func TestRelationMismatchMessage(t *testing.T) {
	w := writeData{set: []string{`heroImage="4"`}}
	_, _, err := w.build(testDeps(), testTarget(), pagesShard())
	if err == nil {
		t.Fatal("expected invalid_args")
	}
	msg := err.Error()
	for _, want := range []string{"heroImage expects a number id", "media", "id_type=number", `the string "4"`, "invalid relationships"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q is missing %q", msg, want)
		}
	}
}

// TestSetJSONBypassesCoercion covers §9.10.1's documented escape hatch.
func TestSetJSONBypassesCoercion(t *testing.T) {
	w := writeData{setJSON: []string{`content={"root":{"children":[]}}`, `heroImage="4"`}}
	body, _, err := w.build(testDeps(), testTarget(), pagesShard())
	if err != nil {
		t.Fatalf("--set-json must bypass coercion: %v", err)
	}
	if _, ok := body["content"].(map[string]any); !ok {
		t.Fatalf("content = %#v", body["content"])
	}
	if body["heroImage"] != "4" {
		t.Fatalf("heroImage = %#v, want the raw string", body["heroImage"])
	}
}

// TestUnknownIDTypeNeverCoerces is the tri-state rule for relationships.
func TestUnknownIDTypeNeverCoerces(t *testing.T) {
	d := testDeps()
	for _, c := range d.Manifest.Collections {
		if c.Slug == "media" {
			c.IDType = discovery.IDTypeUnknown
		}
	}
	w := writeData{set: []string{`heroImage="abc"`}}
	body, warnings, err := w.build(d, testTarget(), pagesShard())
	if err != nil {
		t.Fatalf("an unknown id_type must not reject: %v", err)
	}
	if body["heroImage"] != "abc" {
		t.Fatalf("heroImage = %#v", body["heroImage"])
	}
	found := false
	for _, warn := range warnings {
		if warn.Code == warnWriteShapeUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected write_shape_unknown, got %v", warnings)
	}
}

func TestBadJSONBodies(t *testing.T) {
	tests := []struct {
		name string
		in   writeData
		code apierr.Code
	}{
		{"malformed --data", writeData{data: `{"a":`}, apierr.CodeBadRequestBody},
		{"--data is an array", writeData{data: `[1,2]`}, apierr.CodeBadRequestBody},
		{"--set without =", writeData{set: []string{"title"}}, apierr.CodeInvalidArgs},
		{"--set-json with bad JSON", writeData{setJSON: []string{"a={"}}, apierr.CodeInvalidArgs},
		{"missing --data-file", writeData{dataFile: "/nonexistent/pay-cli/body.json"}, apierr.CodeFileMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.in.build(testDeps(), &collTarget{Slug: "pages"}, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := apierr.CodeOf(err); got != tc.code {
				t.Fatalf("code = %s, want %s", got, tc.code)
			}
		})
	}
}

func TestDataStdin(t *testing.T) {
	d := &Deps{Manifest: pagesManifest(), RT: &Runtime{App: App{Stdin: strings.NewReader(`{"title":"piped"}`)}}}
	w := writeData{data: "@-"}
	body, _, err := w.build(d, &collTarget{Slug: "pages"}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["title"] != "piped" {
		t.Fatalf("body = %v", body)
	}
}

// ---------------------------------------------------------------------------
// §9.7 — unknown block types are warned about BEFORE sending
// ---------------------------------------------------------------------------

func TestBlockTypeWarnings(t *testing.T) {
	shard := pagesShard()

	t.Run("an unknown slug warns", func(t *testing.T) {
		body := map[string]any{"layout": []any{map[string]any{"blockType": "nope"}}}
		got := blockTypeWarnings(testTarget(), shard, body)
		if len(got) != 1 || got[0].Code != warnUnknownBlockType {
			t.Fatalf("warnings = %+v", got)
		}
		if !strings.Contains(got[0].Message, `"nope"`) {
			t.Fatalf("message = %q", got[0].Message)
		}
	})

	t.Run("a known slug is silent", func(t *testing.T) {
		body := map[string]any{"layout": []any{map[string]any{"blockType": "cta"}}}
		if got := blockTypeWarnings(testTarget(), shard, body); len(got) != 0 {
			t.Fatalf("warnings = %+v", got)
		}
	})

	t.Run("unresolvable slugs warn on every value", func(t *testing.T) {
		s := pagesShard()
		s.Blocks = nil
		body := map[string]any{"layout": []any{map[string]any{"blockType": "cta"}}}
		got := blockTypeWarnings(testTarget(), s, body)
		if len(got) != 1 {
			t.Fatalf("warnings = %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// §10.2 — the echo-diff and its two classes
// ---------------------------------------------------------------------------

func TestEchoDiff(t *testing.T) {
	shard := pagesShard()

	sent := map[string]any{
		"title":  "t3",
		"slug":   "My Page",
		"layout": []any{map[string]any{"blockType": "nope"}},
	}
	doc := payload.Doc{
		"id":        16,
		"title":     "t3",
		"slug":      "my-page",
		"_status":   "draft",
		"layout":    []any{},
		"updatedAt": "2026-09-16T12:00:00Z",
	}

	warns := echoDiff("pages", sent, doc, echoConfig{Shard: shard})
	byCode := map[string]output.Warning{}
	for _, w := range warns {
		byCode[w.Code] = w
	}

	dropped, ok := byCode[output.WarnInputSilentlyDropped]
	if !ok {
		t.Fatalf("no input_silently_dropped warning: %+v", warns)
	}
	if len(dropped.Paths) != 1 || dropped.Paths[0] != "layout[0]" {
		t.Fatalf("dropped paths = %v, want [layout[0]]", dropped.Paths)
	}
	if dropped.Sent["layout[0].blockType"] != "nope" {
		t.Fatalf("dropped sent = %v", dropped.Sent)
	}
	if !strings.Contains(dropped.Hint, "pay describe pages --field layout") {
		t.Fatalf("hint = %q", dropped.Hint)
	}

	norm, ok := byCode[output.WarnValueNormalizedByServer]
	if !ok {
		t.Fatalf("no value_normalized_by_server warning: %+v", warns)
	}
	if len(norm.Paths) != 1 || norm.Paths[0] != "slug" {
		t.Fatalf("normalized paths = %v, want [slug]", norm.Paths)
	}
	if norm.Sent["slug"] != "My Page" || norm.Returned["slug"] != "my-page" {
		t.Fatalf("sent/returned = %v / %v", norm.Sent, norm.Returned)
	}
	// The formatSlug hook must NOT be reported as a dropped input: that false
	// positive is what trains an agent to ignore the one warning that matters.
	if _, bad := dropped.Sent["slug"]; bad {
		t.Fatal("a normalised value must never appear as a dropped input")
	}
}

func TestEchoDiffSkips(t *testing.T) {
	shard := pagesShard()
	localizedUnknown := pagesShard()
	for i := range localizedUnknown.Fields {
		if localizedUnknown.Fields[i].Path == "title" {
			localizedUnknown.Fields[i].Localized = nil
		}
	}

	tests := []struct {
		name string
		sent map[string]any
		doc  payload.Doc
		cfg  echoConfig
	}{
		{
			name: "bookkeeping fields",
			sent: map[string]any{"id": 1, "createdAt": "x", "updatedAt": "y", "_status": "draft"},
			doc:  payload.Doc{"id": 2, "createdAt": "a", "updatedAt": "b", "_status": "published"},
			cfg:  echoConfig{Shard: shard},
		},
		{
			name: "array item ids",
			sent: map[string]any{"layout": []any{map[string]any{"id": "abc", "blockType": "cta"}}},
			doc:  payload.Doc{"layout": []any{map[string]any{"id": "xyz", "blockType": "cta"}}},
			cfg:  echoConfig{Shard: shard},
		},
		{
			name: "echo_check_ignore",
			sent: map[string]any{"slug": "My Page"},
			doc:  payload.Doc{"slug": "my-page"},
			cfg:  echoConfig{Shard: shard, Ignore: []string{"slug"}},
		},
		{
			name: "hasMany reordering",
			sent: map[string]any{"tags": []any{"a", "b"}},
			doc:  payload.Doc{"tags": []any{"b", "a"}},
			cfg:  echoConfig{Shard: shard},
		},
		{
			name: "localized is unknown",
			sent: map[string]any{"title": "en"},
			doc:  payload.Doc{"title": map[string]any{"en": "en", "de": "de"}},
			cfg:  echoConfig{Shard: localizedUnknown},
		},
		{
			name: "locale=all disables the whole check",
			sent: map[string]any{"title": "en"},
			doc:  payload.Doc{},
			cfg:  echoConfig{Shard: shard, LocaleAll: true},
		},
		{
			name: "--no-echo-check disables the whole check",
			sent: map[string]any{"title": "en"},
			doc:  payload.Doc{},
			cfg:  echoConfig{Shard: shard, Disabled: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := echoDiff("pages", tc.sent, tc.doc, tc.cfg); len(got) != 0 {
				t.Fatalf("expected no warnings, got %+v", got)
			}
		})
	}
}

func TestCreatedAsDraftWarning(t *testing.T) {
	shard := pagesShard()
	doc := payload.Doc{"id": 16, "title": "t3", "_status": "draft", "layout": []any{}}

	w := createdAsDraftWarning("pages", doc, shard, boolp(true))
	if w == nil {
		t.Fatal("expected created_as_draft")
	}
	if w.Code != output.WarnCreatedAsDraft {
		t.Fatalf("code = %s", w.Code)
	}
	if len(w.StillMissing) != 1 || w.StillMissing[0] != "layout" {
		t.Fatalf("still_missing = %v, want [layout]", w.StillMissing)
	}
	if !strings.Contains(w.Hint, "pay publish pages 16") {
		t.Fatalf("hint = %q", w.Hint)
	}
	if !strings.Contains(w.Message, "publishing re-runs exactly that validation") {
		t.Fatalf("message = %q", w.Message)
	}

	if createdAsDraftWarning("pages", payload.Doc{"_status": "published"}, shard, boolp(true)) != nil {
		t.Fatal("a published document must not warn")
	}
	if createdAsDraftWarning("pages", doc, shard, nil) != nil {
		t.Fatal("unknown drafts support must not warn")
	}
}

func TestStillMissing(t *testing.T) {
	shard := pagesShard()
	tests := []struct {
		name string
		doc  payload.Doc
		want []string
	}{
		{"empty array counts as missing", payload.Doc{"title": "t", "layout": []any{}}, []string{"layout"}},
		{"absent counts as missing", payload.Doc{"title": "t"}, []string{"layout"}},
		{"empty string counts as missing", payload.Doc{"title": "", "layout": []any{1}}, []string{"title"}},
		{"complete", payload.Doc{"title": "t", "layout": []any{1}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stillMissing(shard, tc.doc)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("stillMissing = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPathHelpers(t *testing.T) {
	doc := map[string]any{
		"layout": []any{map[string]any{"blockType": "cta"}},
		"meta":   map[string]any{"title": "x"},
	}
	tests := []struct {
		path string
		want any
		ok   bool
	}{
		{"meta.title", "x", true},
		{"layout[0].blockType", "cta", true},
		{"layout[1].blockType", nil, false},
		{"nope", nil, false},
	}
	for _, tc := range tests {
		got, ok := lookupPath(doc, tc.path)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("lookupPath(%q) = (%v,%v), want (%v,%v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
	if got := firstMissingPrefix(doc, "layout[1].blockType"); got != "layout[1]" {
		t.Fatalf("firstMissingPrefix = %q", got)
	}
	if got := joinPath(splitPath("a[0].b")); got != "a[0].b" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestCreateCommandWiring(t *testing.T) {
	cmd := newCreateCmd(nil)
	for _, name := range []string{"data", "data-file", "set", "set-json", "draft", "no-echo-check", "depth", "select"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay create is missing --%s", name)
		}
	}
	if !strings.Contains(cmd.Long, "COMBINABLE") {
		t.Fatal("create help must say the data flags combine")
	}
}

// TestEchoDiffLocalizedUnknownStillChecksScalars is the narrowing of §7.9d:
// `localized: null` is the NORMAL answer (§7.6 LOCALIZATION_PER_FIELD_UNKNOWN),
// so skipping every such field would disable the whole check. Only a field
// whose echo really is a locale map is skipped.
func TestEchoDiffLocalizedUnknownStillChecksScalars(t *testing.T) {
	shard := pagesShard()
	for i := range shard.Fields {
		shard.Fields[i].Localized = nil
	}

	t.Run("a locale map is skipped", func(t *testing.T) {
		sent := map[string]any{"title": "en"}
		doc := payload.Doc{"title": map[string]any{"en": "en", "de": "de"}}
		if got := echoDiff("pages", sent, doc, echoConfig{Shard: shard}); len(got) != 0 {
			t.Fatalf("expected no warnings, got %+v", got)
		}
	})

	t.Run("a discarded blocks array is still reported", func(t *testing.T) {
		sent := map[string]any{"layout": []any{map[string]any{"blockType": "nope"}}}
		doc := payload.Doc{"layout": []any{}}
		got := echoDiff("pages", sent, doc, echoConfig{Shard: shard})
		if len(got) != 1 || got[0].Code != output.WarnInputSilentlyDropped {
			t.Fatalf("warnings = %+v", got)
		}
		if got[0].Paths[0] != "layout[0]" {
			t.Fatalf("paths = %v, want [layout[0]]", got[0].Paths)
		}
	})
}

// TestSelectNarrowsTheEchoCheckOnEveryWriteVerb is finding 9's remaining call
// sites. --select is forwarded into the write's query params and Payload
// honours it on POST and PATCH alike, so the echoed document legitimately
// omits every non-selected field. `update` was taught to narrow the check;
// `create` and `globals set` still ran the raw diff, so a write that fully
// succeeded reported every field the caller sent as input_silently_dropped —
// the exact false positive §10.2 argues at length must not exist.
func TestSelectNarrowsTheEchoCheckOnEveryWriteVerb(t *testing.T) {
	sent := map[string]any{"title": "Pricing", "slug": "pricing"}
	// What Payload echoes for --select id: the id, and nothing else.
	echoed := payload.Doc{"id": 28}

	raw := echoDiff("pages", sent, echoed, echoConfig{})
	if !hasWarn(raw, "input_silently_dropped") {
		t.Fatal("precondition: the raw diff is supposed to flag these as dropped")
	}

	narrowed := echoWarnings("pages", sent, echoed, []string{"id"}, echoConfig{})
	if hasWarn(narrowed, "input_silently_dropped") {
		t.Errorf("--select still reports a successful write as input_silently_dropped: %v", narrowed)
	}
	// The suppression must be VISIBLE: a genuine drop would hide here too.
	if !hasWarn(narrowed, warnEchoCheckNarrowed) {
		t.Errorf("the narrowed check is silent; expected %s: %v", warnEchoCheckNarrowed, narrowed)
	}
}

func hasWarn(ws []output.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestCreateWithSelectDoesNotCryDroppedInput is the CALL-SITE half of the test
// above: create.go passed no select set into the echo check, so the narrowing
// helper existed but was never reached from `pay create --select`.
func TestCreateWithSelectDoesNotCryDroppedInput(t *testing.T) {
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("POST", "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			// Payload honours ?select[id]=true on a create: id and nothing else.
			_, _ = io.WriteString(w, `{"doc":{"id":28},"message":"created"}`)
		}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"create", "pages", "--set", "title=Pricing", "--set", "slug=pricing",
			"--select", "id", "--draft"},
		Env: seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	warns, _ := res.Env["warnings"].([]any)
	var codes []string
	for _, w := range warns {
		if m, ok := w.(map[string]any); ok {
			codes = append(codes, fmt.Sprint(m["code"]))
		}
	}
	for _, c := range codes {
		if c == "input_silently_dropped" {
			t.Fatalf("`create --select id` reports a fully successful write as "+
				"input_silently_dropped; warnings: %v", codes)
		}
	}
	found := false
	for _, c := range codes {
		if c == warnEchoCheckNarrowed {
			found = true
		}
	}
	if !found {
		t.Errorf("the narrowed echo check is invisible; want %s in %v", warnEchoCheckNarrowed, codes)
	}
}
