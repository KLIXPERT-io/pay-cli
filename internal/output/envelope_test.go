package output

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/google/go-cmp/cmp"
)

const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

func intptr(i int) *int     { return &i }
func i64ptr(i int64) *int64 { return &i }

func fullSuccessEnvelope() *Envelope {
	version := "3.86.0"
	return New("find", KindDocList, []any{
		map[string]any{"id": float64(11), "title": "Home", "slug": "home", "_status": "published"},
	}).
		WithTarget(&Target{Kind: "collection", Slug: "pages", Singular: "Page", IDType: "number"}).
		WithPage(&Page{
			Limit: 3, Page: 1, TotalPages: 4, TotalDocs: 11, Returned: 3,
			HasNextPage: true, HasPrevPage: false, NextPage: intptr(2), PrevPage: nil, Truncated: true,
		}).
		WithNext(&Next{
			Reason: ReasonMorePages,
			Cmd:    "pay find pages --limit 3 --page 2 --profile dev",
			Args:   map[string]any{"page": 2},
			Alternatives: []Alternative{
				{Why: "stream every page without looping", Cmd: "pay find pages --all --output jsonl"},
			},
		}).
		WithMeta(Meta{
			RequestID: "01K5Q7TZ4V3B8C", CLIVersion: "0.1.0", Profile: "dev",
			BaseURL: "http://localhost:3900", APIPath: "/api", AuthMode: AuthModeAPIKey,
			PayloadVersion: &version, PayloadVersionSource: "project-package-json",
			DurationMS: 41, HTTPRequests: 1, Retries: 0, Bytes: 612,
			Cache:             &CacheMeta{Discovery: CacheHit, AgeS: i64ptr(412), TTLS: i64ptr(600), Fingerprint: "0df77681"},
			DiscoveryRevision: "2026-09-16T17:00:00Z",
		})
}

// TestEnvelopeKeyOrder pins §10.1's "keys in this order". The envelope is the
// single most-read object in the codebase and its order is part of the
// contract, not a formatting detail.
func TestEnvelopeKeyOrder(t *testing.T) {
	tests := []struct {
		name string
		env  *Envelope
		want []string
	}{
		{
			name: "success list (§10.1)",
			env:  fullSuccessEnvelope(),
			want: []string{"ok", "v", "command", "data_kind", "target", "data", "page", "next", "meta", "warnings"},
		},
		{
			name: "write (§10.2)",
			env: New("create", KindDoc, map[string]any{"id": 16}).
				WithTarget(&Target{Kind: "collection", Slug: "pages", ID: 16}).
				WithChanged(&Changed{Created: 1, IDs: []any{16}}).
				WithNext(&Next{Reason: ReasonVerifyWrite, Cmd: "pay get pages 16 --depth 0"}),
			want: []string{"ok", "v", "command", "data_kind", "target", "data", "changed", "next", "meta", "warnings"},
		},
		{
			name: "error (§11.1)",
			env: NewError("create", apierr.New(apierr.CodeValidationFailed, "3 fields are invalid.")).
				WithTarget(&Target{Kind: "collection", Slug: "pages"}),
			want: []string{"ok", "v", "command", "data_kind", "target", "error", "meta", "warnings"},
		},
		{
			name: "partial failure (§12.5)",
			env: func() *Envelope {
				e := NewError("update", apierr.New(apierr.CodePartialFailure, "1 of 2 failed."))
				e.DataKind = KindBulkResult
				e.Partial = true
				e.Data = map[string]any{"succeeded": []any{}, "failed": []any{}}
				e.Changed = &Changed{Updated: 1, IDs: []any{11}}
				return e
			}(),
			want: []string{"ok", "v", "command", "data_kind", "partial", "data", "changed", "error", "meta", "warnings"},
		},
		{
			name: "count",
			env:  New("count", KindCount, 11),
			want: []string{"ok", "v", "command", "data_kind", "data", "meta", "warnings"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.env.MarshalIndentTo()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, topLevelKeys(t, b)); diff != "" {
				t.Errorf("key order (-want +got):\n%s\n%s", diff, b)
			}
		})
	}
}

// TestCountZeroIsNotOmitted: `data` uses omitempty, which for an interface
// field omits only a nil interface. A count of 0 must still print.
func TestCountZeroIsNotOmitted(t *testing.T) {
	b, err := New("count", KindCount, 0).MarshalIndentTo()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data": 0`) {
		t.Errorf("a zero count must be printed: %s", b)
	}
}

func TestEmptyDocListIsAnArray(t *testing.T) {
	b, err := New("find", KindDocList, []any{}).MarshalIndentTo()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data": []`) {
		t.Errorf("an empty doc_list must print []: %s", b)
	}
}

func TestWarningsIsAlwaysAnArray(t *testing.T) {
	for _, env := range []*Envelope{New("find", KindDocList, []any{}), NewError("find", apierr.New(apierr.CodeInternal, "x"))} {
		b, err := env.MarshalIndentTo()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"warnings": []`) {
			t.Errorf("warnings must always be an array: %s", b)
		}
	}
}

// TestHTMLIsNotEscaped: encoding/json escapes <, > and & by default, which
// turns every "<redacted>" sentinel into an escape sequence for an audience
// that is not a browser.
func TestHTMLIsNotEscaped(t *testing.T) {
	env := New("get", KindDoc, map[string]any{"apiKey": "<redacted>", "q": "a&b", "html": "<p>"})
	b, err := env.MarshalIndentTo()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<redacted>", "a&b", "<p>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%q was escaped: %s", want, b)
		}
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(string(b), escaped) {
			t.Errorf("HTML escaping is on (%s): %s", escaped, b)
		}
	}
}

// TestMetaBaseURLIsRedacted covers §5.3's mandatory redaction of meta.base_url.
func TestMetaBaseURLIsRedacted(t *testing.T) {
	env := New("find", KindDocList, []any{}).
		WithMeta(Meta{BaseURL: "http://admin:hunter2@localhost:3900"})
	if strings.Contains(env.Meta.BaseURL, "hunter2") {
		t.Errorf("meta.base_url leaked a credential: %q", env.Meta.BaseURL)
	}
	if env.Meta.BaseURL != "http://localhost:3900" {
		t.Errorf("meta.base_url = %q", env.Meta.BaseURL)
	}
}

func TestNullableMetaFieldsArePresent(t *testing.T) {
	b, err := New("find", KindDocList, []any{}).WithMeta(Meta{}).MarshalIndentTo()
	if err != nil {
		t.Fatal(err)
	}
	// payload_version is null rather than a fabricated constant (§7.11), and
	// locale/date_field/since/until are explicitly null rather than absent.
	for _, want := range []string{
		`"payload_version": null`, `"date_field": null`, `"since": null`, `"until": null`,
		`"requested": null`, `"fallback": null`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in:\n%s", want, b)
		}
	}
}

func TestPageNullableFields(t *testing.T) {
	b, err := New("find", KindDocList, []any{}).
		WithPage(&Page{Limit: 10, Page: 1, TotalPages: 1, TotalDocs: 0, Returned: 0}).
		MarshalIndentTo()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"next_page": null`, `"prev_page": null`, `"truncated": false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in:\n%s", want, b)
		}
	}
}

func TestWithRawBody(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		noRedact    bool
		wantChanged bool
		wantLeak    bool
	}{
		{name: "clean body is untouched", body: `{"id":1,"title":"Home"}`},
		{name: "secret body is redacted", body: `{"id":1,"apiKey":"` + fixtureKey + `"}`, wantChanged: true},
		{name: "no-redact keeps the wire bytes", body: `{"apiKey":"` + fixtureKey + `"}`, noRedact: true, wantLeak: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := New("get", KindDoc, nil).WithRawBody([]byte(tc.body), tc.noRedact)
			if env.RawRedacted != tc.wantChanged {
				t.Errorf("RawRedacted = %v, want %v", env.RawRedacted, tc.wantChanged)
			}
			leaked := strings.Contains(string(env.RawBody), fixtureKey)
			if leaked != tc.wantLeak {
				t.Errorf("leak = %v, want %v (body %s)", leaked, tc.wantLeak, env.RawBody)
			}
			if !tc.wantChanged && !tc.noRedact && string(env.RawBody) != tc.body {
				t.Errorf("an unchanged body must stay byte-faithful, got %s", env.RawBody)
			}
		})
	}
}

func TestPropagateRawRedaction(t *testing.T) {
	e := apierr.New(apierr.CodePartialFailure, "x").
		WithRaw([]byte(`{"docs":[{"apiKey":"`+fixtureKey+`"}]}`), false)
	env := NewError("update", e).PropagateRawRedaction()
	if len(env.Warnings) != 1 {
		t.Fatalf("Warnings = %d, want 1", len(env.Warnings))
	}
	w := env.Warnings[0]
	if w.Code != WarnRawRedacted {
		t.Errorf("code = %q", w.Code)
	}
	if diff := cmp.Diff([]string{"docs[0].apiKey"}, w.Paths); diff != "" {
		t.Errorf("paths (-want +got):\n%s", diff)
	}
	if !strings.Contains(w.Hint, "--no-redact") {
		t.Errorf("the hint must name the escape hatch: %q", w.Hint)
	}

	// No redaction, no warning.
	clean := NewError("update", apierr.New(apierr.CodeInternal, "x").WithRaw([]byte(`{"a":1}`), false)).
		PropagateRawRedaction()
	if len(clean.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", clean.Warnings)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		env  *Envelope
		want int
	}{
		{"success", New("find", KindDocList, nil), 0},
		{"validation", NewError("create", apierr.New(apierr.CodeValidationFailed, "x")), 5},
		{"partial", NewError("update", apierr.New(apierr.CodePartialFailure, "x")), 7},
		{"access denied", NewError("find", apierr.New(apierr.CodeAccessDenied, "x")), 8},
		{"plain error", NewError("find", errString("boom")), 1},
		{"nil envelope", nil, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.env.ExitCode(); got != tc.want {
				t.Errorf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDataKindValid(t *testing.T) {
	for _, k := range DataKinds {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if DataKind("nope").Valid() {
		t.Error("an undeclared kind must not validate")
	}
	if len(DataKinds) != 13 {
		t.Errorf("§10.1 declares 13 data kinds, DataKinds has %d", len(DataKinds))
	}
}

func TestAddWarning(t *testing.T) {
	env := New("create", KindDoc, nil).
		AddWarning(Warning{Code: WarnInputSilentlyDropped, Message: "m", Paths: []string{"layout[0]"}}).
		AddWarning(Warning{Code: WarnCreatedAsDraft, Message: "m"})
	if len(env.Warnings) != 2 {
		t.Fatalf("Warnings = %d", len(env.Warnings))
	}
	if !env.OK {
		t.Error("warnings never change ok")
	}
	if env.ExitCode() != 0 {
		t.Error("warnings never change the exit code")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func topLevelKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("not an object: %s", b)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// warningCodes lists the codes of a rendered envelope, read back out of the
// bytes rather than the struct, so the assertion is about what the agent
// actually receives.
func renderedWarnings(t *testing.T, env *Envelope) []Warning {
	t.Helper()
	var buf bytes.Buffer
	w := &Writer{Stdout: &buf, Stderr: io.Discard, Format: FormatJSON, Quiet: true}
	if _, err := w.Render(env); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got struct {
		Warnings []Warning `json:"warnings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode rendered envelope: %v\n%s", err, buf.String())
	}
	return got.Warnings
}

func findWarning(ws []Warning, code string) *Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

// TestAnonymousSessionWarningSurvivesIntoTheRenderedEnvelope is §5.0/§21.1-A1:
// EVERY envelope of an anonymous run — success and error — carries
// `anonymous_session`, because an anonymous caller sees a reduced permission
// matrix, a reduced collection list and reduced fields, and a truncated answer
// presented as a complete one is a wrong answer. Before meta.auth_mode was
// wired to warnings[], the constant existed and was emitted nowhere.
func TestAnonymousSessionWarningSurvivesIntoTheRenderedEnvelope(t *testing.T) {
	anon := Meta{Profile: "local", BaseURL: "http://localhost:3900", AuthMode: AuthModeAnonymous}

	t.Run("success envelope", func(t *testing.T) {
		env := New("collections", KindCapabilities, map[string]any{"collections": []any{}}).WithMeta(anon)
		w := findWarning(renderedWarnings(t, env), WarnAnonymousSession)
		if w == nil {
			t.Fatal("anonymous_session missing from a rendered anonymous success envelope")
		}
		wantMsg := `No credential resolved for profile "local"; running unauthenticated. Permissions, the collection list and field visibility are all reduced.`
		if w.Message != wantMsg {
			t.Errorf("message = %q, want %q", w.Message, wantMsg)
		}
		if want := "pay auth login --profile local --base-url http://localhost:3900"; w.Hint != want {
			t.Errorf("hint = %q, want %q", w.Hint, want)
		}
	})

	t.Run("error envelope", func(t *testing.T) {
		env := NewError("find", apierr.New(apierr.CodeAuthRequired, "nope")).WithMeta(anon)
		if findWarning(renderedWarnings(t, env), WarnAnonymousSession) == nil {
			t.Fatal("anonymous_session missing from a rendered anonymous error envelope")
		}
	})

	t.Run("authenticated envelopes do not carry it", func(t *testing.T) {
		env := New("collections", KindCapabilities, nil).
			WithMeta(Meta{Profile: "local", BaseURL: "http://localhost:3900", AuthMode: AuthModeAPIKey})
		if findWarning(renderedWarnings(t, env), WarnAnonymousSession) != nil {
			t.Fatal("anonymous_session on an api-key run")
		}
	})

	t.Run("warnings never change ok or the exit code", func(t *testing.T) {
		env := New("collections", KindCapabilities, nil).WithMeta(anon)
		if !env.OK || env.ExitCode() != 0 {
			t.Fatalf("ok = %v, exit = %d", env.OK, env.ExitCode())
		}
	})

	t.Run("a second WithMeta does not duplicate it", func(t *testing.T) {
		// A command sets its own meta block and Runtime.emit then merges the
		// runtime's over it, so WithMeta runs twice on the same envelope.
		env := New("collections", KindCapabilities, nil).WithMeta(anon).WithMeta(anon)
		got := renderedWarnings(t, env)
		n := 0
		for _, w := range got {
			if w.Code == WarnAnonymousSession {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("anonymous_session appears %d times, want 1", n)
		}
	})

	t.Run("a later authenticated meta withdraws it", func(t *testing.T) {
		env := New("collections", KindCapabilities, nil).
			WithMeta(anon).
			WithMeta(Meta{Profile: "local", BaseURL: "http://localhost:3900", AuthMode: AuthModeAPIKey})
		if findWarning(renderedWarnings(t, env), WarnAnonymousSession) != nil {
			t.Fatal("stale anonymous_session survived an authenticated meta")
		}
	})

	t.Run("other warnings are preserved", func(t *testing.T) {
		env := New("create", KindDoc, nil).
			AddWarning(Warning{Code: WarnCreatedAsDraft, Message: "m"}).
			WithMeta(anon)
		got := renderedWarnings(t, env)
		if findWarning(got, WarnCreatedAsDraft) == nil || findWarning(got, WarnAnonymousSession) == nil {
			t.Fatalf("warnings = %+v", got)
		}
	})

	t.Run("the hint never carries a credential", func(t *testing.T) {
		env := New("collections", KindCapabilities, nil).WithMeta(Meta{
			Profile: "local", AuthMode: AuthModeAnonymous,
			BaseURL: "http://admin:" + fixtureKey + "@localhost:3900",
		})
		w := findWarning(renderedWarnings(t, env), WarnAnonymousSession)
		if w == nil {
			t.Fatal("anonymous_session missing")
		}
		if strings.Contains(w.Hint, fixtureKey) || strings.Contains(w.Hint, "admin:") {
			t.Fatalf("§5.3: the hint leaked a credential: %q", w.Hint)
		}
	})

	t.Run("an empty base_url leaves the hint runnable", func(t *testing.T) {
		env := New("explain", KindCapabilities, nil).WithMeta(Meta{Profile: "local", AuthMode: AuthModeAnonymous})
		w := findWarning(renderedWarnings(t, env), WarnAnonymousSession)
		if w == nil || w.Hint != "pay auth login --profile local" {
			t.Fatalf("hint = %+v", w)
		}
	})
}

// TestTransparencyWarningConstructors pins the two other §7 notices the
// envelope owes an agent. The emission sites are in internal/cli (the CheckID
// call sites) and internal/discovery (the learned-operator memo); the output
// side must at least offer them, spelled the way §7.6(a)/§7.11 spell them.
func TestTransparencyWarningConstructors(t *testing.T) {
	idw := IDTypeUnknownWarning("pages", "local")
	if idw.Code != WarnIDTypeUnknown || WarnIDTypeUnknown != "id_type_unknown" {
		t.Errorf("code = %q", idw.Code)
	}
	if want := "pin it once with `pay config set profiles.local.id_type string`; until then ids are passed through unchecked"; idw.Hint != want {
		t.Errorf("hint = %q, want §7.6(a)'s wording %q", idw.Hint, want)
	}
	if !strings.Contains(idw.Message, "pages") || !strings.Contains(idw.Message, "skipped") {
		t.Errorf("message = %q, want it to name the collection and say the check was skipped", idw.Message)
	}

	opw := OperatorPreviouslyFailedWarning("all", "crm-contacts", "HTTP 500 Something went wrong.")
	if opw.Code != WarnOperatorPreviouslyFailed || WarnOperatorPreviouslyFailed != "operator_previously_failed" {
		t.Errorf("code = %q", opw.Code)
	}
	if !strings.Contains(opw.Message, "all") || !strings.Contains(opw.Message, "crm-contacts") {
		t.Errorf("message = %q, want it to name the operator and the collection", opw.Message)
	}
	if !strings.Contains(opw.Message, "HTTP 500 Something went wrong.") {
		t.Errorf("message = %q, want it to carry the recorded evidence", opw.Message)
	}
}
