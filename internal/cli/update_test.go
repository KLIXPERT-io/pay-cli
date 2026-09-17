package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// TestBulkEnvelopeSuccess covers the all-succeeded path: no error block, no
// partial flag, exit 0.
func TestBulkEnvelopeSuccess(t *testing.T) {
	res := &payload.BulkResult{Docs: []payload.Doc{{"id": 11}, {"id": 12}}}
	env := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(), res, []any{11, 12}, nil, "")
	if !env.OK || env.Partial || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	if env.ExitCode() != 0 {
		t.Fatalf("exit = %d, want 0", env.ExitCode())
	}
	if env.Changed.Updated != 2 || len(env.Changed.IDs) != 2 {
		t.Fatalf("changed = %+v", env.Changed)
	}
}

// TestBulkEnvelopePartialFailure is §12.5: HTTP 400 with committed documents
// in the same body must become exit 7 with a "never re-run" hint.
func TestBulkEnvelopePartialFailure(t *testing.T) {
	res := &payload.BulkResult{
		Docs: []payload.Doc{{"id": 11, "title": "Bulk Test"}},
		Errors: []payload.BulkFailure{
			{ID: 10, Message: "The following field is invalid: Content > Layout"},
		},
		HTTP: &payload.Response{Status: 400, Method: "PATCH", URL: "http://x/api/pages"},
		Raw:  []byte(`{"errors":[{"id":10}]}`),
	}
	env := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(), res, []any{10, 11}, nil,
		"pay update pages --where 'id in 10' --set title='Bulk Test'")

	if env.OK {
		t.Fatal("a partial failure must not be ok")
	}
	if !env.Partial {
		t.Fatal("partial must be true")
	}
	if env.ExitCode() != apierr.ExitPartial {
		t.Fatalf("exit = %d, want 7", env.ExitCode())
	}
	if env.Error.Code != apierr.CodePartialFailure {
		t.Fatalf("code = %s", env.Error.Code)
	}
	if env.Error.Retriable {
		t.Fatal("partial_failure must never be retriable")
	}
	if !strings.Contains(env.Error.Hint, "ALREADY COMMITTED") ||
		!strings.Contains(env.Error.Hint, "NOT transactional") {
		t.Fatalf("hint = %q", env.Error.Hint)
	}
	if !strings.Contains(env.Error.Message, "Updated 1 of 2") {
		t.Fatalf("message = %q", env.Error.Message)
	}
	if len(env.Error.Failures) != 1 || env.Error.Failures[0].Code != apierr.CodeValidationFailed {
		t.Fatalf("failures = %+v", env.Error.Failures)
	}
	if env.Changed.Updated != 1 {
		t.Fatalf("changed = %+v — the committed write must still be reported", env.Changed)
	}
	if env.Next == nil || env.Next.Reason != output.ReasonRetryFailedSubset {
		t.Fatalf("next = %+v", env.Next)
	}
	data, ok := env.Data.(bulkData)
	if !ok {
		t.Fatalf("data = %T", env.Data)
	}
	if len(data.Succeeded) != 1 || len(data.Failed) != 1 || data.NotAttempted == nil {
		t.Fatalf("data = %+v", data)
	}
}

// TestBulkEnvelopeWithheldMessage covers the isPublic:false rewrite.
func TestBulkEnvelopeWithheldMessage(t *testing.T) {
	res := &payload.BulkResult{
		Errors: []payload.BulkFailure{{ID: 10, Message: serverErrorMessage}},
	}
	env := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(), res, []any{10}, nil, "")
	f := env.Error.Failures[0]
	if f.Code != apierr.CodeServerError {
		t.Fatalf("code = %s, want server_error", f.Code)
	}
	if !strings.Contains(f.Message, "debug: true") {
		t.Fatalf("message = %q — it must say how to see the real one", f.Message)
	}
}

func TestBulkEnvelopeDeleteCounters(t *testing.T) {
	res := &payload.BulkResult{Docs: []payload.Doc{{"id": 1}}}
	env := bulkEnvelope(safety.CmdDelete, "Deleted", testTarget(), res, []any{1}, nil, "")
	if env.Changed.Deleted != 1 || env.Changed.Updated != 0 {
		t.Fatalf("changed = %+v", env.Changed)
	}
}

// TestBulkWriteSendsNoLimit is §12.3's golden assertion: the query string a
// bulk write builds carries no `limit=` parameter, because bulk DELETE ignores
// it and would delete every match.
func TestBulkWriteSendsNoLimit(t *testing.T) {
	where := query.Term("category", query.OpEquals, "news")
	q, err := query.Params{Where: where}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "limit=") {
		t.Fatalf("bulk query string must not carry a limit: %s", q)
	}
}

// TestLimitOnBulkWriteIsRejected pins §12.3's flag split.
func TestLimitOnBulkWriteIsRejected(t *testing.T) {
	for _, command := range []string{safety.CmdUpdate, safety.CmdDelete, safety.CmdPublish, safety.CmdUnpublish} {
		err := safety.CheckLimitFlag(command, safety.SelectorBulk, true)
		if err == nil {
			t.Fatalf("%s --where --limit must be invalid_args", command)
		}
		if apierr.ExitCode(err) != apierr.ExitValidation {
			t.Fatalf("%s: exit = %d, want 5", command, apierr.ExitCode(err))
		}
		if e, _ := apierr.As(err); !strings.Contains(e.Hint, "--max-docs") {
			t.Fatalf("%s: hint = %q", command, e.Hint)
		}
	}
	// The same flag on the scoped form is page size and must be accepted.
	if err := safety.CheckLimitFlag(safety.CmdUpdate, safety.SelectorID, true); err != nil {
		t.Fatalf("scoped update must accept --limit's presence: %v", err)
	}
}

func TestBlastRadiusCap(t *testing.T) {
	tests := []struct {
		name    string
		matched int
		max     int
		all     bool
		wantErr bool
	}{
		{"within the cap", 50, 100, false, false},
		{"over the cap", 1200, 100, false, true},
		{"--all lifts the cap", 1200, 100, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := safety.Plan{Command: safety.CmdDelete, Collection: "pages", Matched: tc.matched, MaxDocs: tc.max, All: tc.all}
			err := p.Check()
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				if apierr.CodeOf(err) != apierr.CodeBulkLimitExceeded {
					t.Fatalf("code = %s", apierr.CodeOf(err))
				}
				e, _ := apierr.As(err)
				if !strings.Contains(e.Hint, "pass --all to delete everything matching, or narrow --where") {
					t.Fatalf("hint = %q", e.Hint)
				}
			}
		})
	}
}

func TestUpdateCommandWiring(t *testing.T) {
	cmd := newUpdateCmd(nil)
	for _, name := range []string{"set", "where", "max-docs", "all", "per-doc", "limit", "publish", "unpublish", "no-echo-check"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay update is missing --%s", name)
		}
	}
	if !strings.Contains(cmd.Long, "validates the WHOLE document on a PATCH") {
		t.Fatal("update help must carry §9.10.3's COMMON MISTAKES entry")
	}
}

func TestRerunFlags(t *testing.T) {
	f := &updateFlags{publish: true}
	f.data.set = []string{"title=Bulk Test", "featured=true"}
	got := rerunFlags(f)
	if !strings.Contains(got, "--set 'title=Bulk Test'") || !strings.Contains(got, "--publish") {
		t.Fatalf("rerunFlags = %q", got)
	}
	if joinIDs([]any{10, 12}) != "10,12" {
		t.Fatalf("joinIDs = %q", joinIDs([]any{10, 12}))
	}
}

// ---------------------------------------------------------------------------
// §12.3: count, id resolution and the write share one scope
// ---------------------------------------------------------------------------

// trashScopedServer is a collection holding one live document and one
// soft-deleted one that both match the filter. Payload hides the trashed
// document from find/count unless trash=true, so a phase that forgets the
// scope resolves a different population than the phase that counted.
func trashScopedServer(t *testing.T) *payloadtest.Server {
	t.Helper()
	trashed := func(r *http.Request) bool { return r.URL.Query().Get("trash") == "true" }
	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Powered-By", "Payload")
		_, _ = io.WriteString(w, body)
	}
	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/crm-contacts/count", func(w http.ResponseWriter, r *http.Request) {
			if trashed(r) {
				writeJSON(w, `{"totalDocs":2}`)
				return
			}
			writeJSON(w, `{"totalDocs":1}`)
		}),
		payloadtest.WithHandler("GET", "/api/crm-contacts", func(w http.ResponseWriter, r *http.Request) {
			if trashed(r) {
				writeJSON(w, `{"docs":[{"id":3},{"id":4}],"totalDocs":2,"limit":200,"page":1,"hasNextPage":false}`)
				return
			}
			writeJSON(w, `{"docs":[{"id":3}],"totalDocs":1,"limit":200,"page":1,"hasNextPage":false}`)
		}),
	)
}

// scopeOf returns the query of the first request matching a method and path.
func scopeOf(t *testing.T, srv *payloadtest.Server, method, path string) url.Values {
	t.Helper()
	for _, r := range srv.Requests() {
		if r.Method == method && r.Path == path {
			v, err := url.ParseQuery(r.Query)
			if err != nil {
				t.Fatalf("query %q: %v", r.Query, err)
			}
			return v
		}
	}
	t.Fatalf("no %s %s request was made; saw %+v", method, path, srv.Requests())
	return nil
}

// TestBulkUpdateTrashScopeReachesEveryPhase is finding-8's regression: --trash
// used to reach only the PATCH, so `update --where … --trash` counted and
// resolved the live population, found nothing, and reported success having
// changed nothing.
func TestBulkUpdateTrashScopeReachesEveryPhase(t *testing.T) {
	srv := trashScopedServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"update", "crm-contacts", "--where", "id gt 0", "--trash",
			"--set", "jobTitle=zzz", "--all", "--dry-run"},
		Env: seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if got := scopeOf(t, srv, "GET", "/api/crm-contacts/count").Get("trash"); got != "true" {
		t.Errorf("the count phase dropped --trash: trash=%q", got)
	}
	if got := scopeOf(t, srv, "GET", "/api/crm-contacts").Get("trash"); got != "true" {
		t.Errorf("the id resolution dropped --trash: trash=%q", got)
	}
	if got := res.data(t)["would_affect"]; fmt.Sprint(got) != "2" {
		t.Errorf("would_affect = %v, want the 2 soft-deleted documents", got)
	}
}

// TestBulkDeletePermanentResolvesWhatItCounted is finding-7's regression: the
// cap was computed on live+trashed while the ids that would have been deleted
// were live-only, so a purge of the trash was a silent no-op.
func TestBulkDeletePermanentResolvesWhatItCounted(t *testing.T) {
	srv := trashScopedServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"delete", "crm-contacts", "--where", "id gt 0", "--permanent", "--all", "--dry-run"},
		Env:  seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	countScope := scopeOf(t, srv, "GET", "/api/crm-contacts/count")
	resolveScope := scopeOf(t, srv, "GET", "/api/crm-contacts")
	if countScope.Get("trash") != resolveScope.Get("trash") {
		t.Fatalf("phase 1 counted trash=%q while phase 3 resolved trash=%q",
			countScope.Get("trash"), resolveScope.Get("trash"))
	}
	if resolveScope.Get("trash") != "true" {
		t.Errorf("--permanent deletes trashed documents too, so it must resolve them: %v", resolveScope)
	}
	if got := res.data(t)["would_affect"]; fmt.Sprint(got) != "2" {
		t.Errorf("would_affect = %v, want 2 — the cap and the blast radius must describe one population", got)
	}
	ids, _ := res.data(t)["sample_ids"].([]any)
	if len(ids) != 2 {
		t.Errorf("sample_ids = %v, want both documents", ids)
	}
}

// ---------------------------------------------------------------------------
// §10.2 echo-diff under --select
// ---------------------------------------------------------------------------

func codesOf(warns []output.Warning) []string {
	out := make([]string, 0, len(warns))
	for _, w := range warns {
		out = append(out, w.Code)
	}
	return out
}

func warningWithCode(warns []output.Warning, code string) *output.Warning {
	for i, w := range warns {
		if w.Code == code {
			return &warns[i]
		}
	}
	return nil
}

// TestEchoDiffIsProjectionAware is finding-9's regression. Payload honours
// `select` on a write, so a field the projection excluded is absent from the
// echo because of the projection — reporting it as input_silently_dropped is
// the false positive §10.2 exists to prevent.
func TestEchoDiffIsProjectionAware(t *testing.T) {
	sent := map[string]any{"title": "abc", "slug": "xyz"}

	// Control: no --select, so an absent field really was dropped.
	warns := echoWarnings("pages", sent, payload.Doc{"id": 26, "title": "abc"}, nil, echoConfig{})
	if warningWithCode(warns, output.WarnInputSilentlyDropped) == nil {
		t.Fatalf("without --select a missing field must still be reported: %v", codesOf(warns))
	}

	// --select title: the echo cannot contain slug, and the write succeeded.
	warns = echoWarnings("pages", sent, payload.Doc{"id": 26, "title": "abc"}, []string{"title"}, echoConfig{})
	if w := warningWithCode(warns, output.WarnInputSilentlyDropped); w != nil {
		t.Errorf("--select produced a false input_silently_dropped: %+v", *w)
	}
	narrowed := warningWithCode(warns, warnEchoCheckNarrowed)
	if narrowed == nil {
		t.Fatalf("the narrowing must be announced, not silent: %v", codesOf(warns))
	}
	if len(narrowed.Paths) != 1 || narrowed.Paths[0] != "slug" {
		t.Errorf("paths = %v, want the unchecked field", narrowed.Paths)
	}

	// A field that IS selected and still came back missing is a real drop.
	warns = echoWarnings("pages", sent, payload.Doc{"id": 26, "title": "abc"}, []string{"title", "slug"}, echoConfig{})
	if warningWithCode(warns, output.WarnInputSilentlyDropped) == nil {
		t.Errorf("a selected-but-missing field must still be reported: %v", codesOf(warns))
	}
	if warningWithCode(warns, warnEchoCheckNarrowed) != nil {
		t.Errorf("nothing was narrowed away: %v", codesOf(warns))
	}

	// value_normalized_by_server keeps working under a projection.
	warns = echoWarnings("pages", sent, payload.Doc{"id": 26, "title": "ABC"}, []string{"title"}, echoConfig{})
	if warningWithCode(warns, output.WarnValueNormalizedByServer) == nil {
		t.Errorf("a selected field that came back different is still normalised: %v", codesOf(warns))
	}
}

func TestNarrowEchoBodyMatchesOnRoots(t *testing.T) {
	body := map[string]any{"meta.title": "a", "meta.description": "b", "slug": "c"}
	narrowed, unchecked := narrowEchoBody(body, []string{"meta.title"})
	if len(narrowed) != 2 {
		t.Fatalf("narrowed = %v — select[meta][title] returns the meta root", narrowed)
	}
	if len(unchecked) != 1 || unchecked[0] != "slug" {
		t.Fatalf("unchecked = %v", unchecked)
	}
	// No projection is a no-op.
	same, none := narrowEchoBody(body, nil)
	if len(same) != len(body) || none != nil {
		t.Fatalf("narrowEchoBody(body, nil) = %v, %v", same, none)
	}
}

// ---------------------------------------------------------------------------
// §12.5 chunk provenance
// ---------------------------------------------------------------------------

// TestBulkEnvelopeNamesTheChunk is finding-12's CLI half: error.http/error.raw
// carry ONE chunk's response, so the envelope has to say which one.
func TestBulkEnvelopeNamesTheChunk(t *testing.T) {
	res := &payload.BulkResult{
		Docs:        []payload.Doc{{"id": 1}},
		Errors:      []payload.BulkFailure{{ID: 42, Message: "The following field is invalid: Title"}},
		HTTP:        &payload.Response{Status: 400, Method: "PATCH", URL: "http://x/api/pages"},
		Raw:         []byte(`{"errors":[{"id":42}]}`),
		Chunks:      2,
		FailedChunk: 1,
	}
	env := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(), res, make([]any, 150), nil, "")
	if !strings.Contains(env.Error.Hint, "chunk 1 of 2") {
		t.Fatalf("hint does not say which request error.http describes: %q", env.Error.Hint)
	}
	if env.Error.HTTP == nil || env.Error.HTTP.Status != 400 {
		t.Fatalf("error.http = %+v", env.Error.HTTP)
	}

	// A fully successful multi-chunk write says that `raw` is one chunk of N.
	ok := &payload.BulkResult{Docs: []payload.Doc{{"id": 1}}, Chunks: 2}
	okEnv := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(), ok, make([]any, 150), nil, "")
	if !okEnv.OK {
		t.Fatalf("envelope = %+v", okEnv)
	}
	found := false
	for _, w := range okEnv.Warnings {
		if w.Code == warnBulkChunked {
			found = true
		}
	}
	if !found {
		t.Fatalf("a 2-chunk write must say `raw` is one chunk: %+v", okEnv.Warnings)
	}
	// A single-chunk write is unchanged.
	single := bulkEnvelope(safety.CmdUpdate, "Updated", testTarget(),
		&payload.BulkResult{Docs: []payload.Doc{{"id": 1}}, Chunks: 1}, []any{1}, nil, "")
	if len(single.Warnings) != 0 {
		t.Fatalf("single-chunk warnings = %+v", single.Warnings)
	}
}

// TestQuoteArgIsShellSafe is the next.cmd half of §10.4's "literally runnable"
// promise. quoteArg used to quote only when the value held a space, tab or
// quote, so a retry string like --set 'slug=a*b' — or any value containing
// $VAR, ;, & or a backtick — was emitted bare and re-interpreted by the
// agent's shell.
func TestQuoteArgIsShellSafe(t *testing.T) {
	dangerous := []string{
		"slug=a*b",
		"title=$HOME",
		"body=a;rm -rf /",
		"body=a&&b",
		"body=`id`",
		"body=a|b",
		"body=a>b",
		"body=(x)",
		"body=a\nb",
		"",
	}
	for _, in := range dangerous {
		got := quoteArg(in)
		if got == in && in != "" {
			t.Errorf("quoteArg(%q) = %q: emitted unquoted, the shell will reinterpret it", in, got)
			continue
		}
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("quoteArg(%q) = %q, want single-quoted", in, got)
		}
	}
	// Plain values stay unquoted so next.cmd remains readable.
	for _, in := range []string{"title=Hello", "id", "a.b", "a,b", "a/b", "a=b"} {
		if got := quoteArg(in); got != in {
			t.Errorf("quoteArg(%q) = %q, want it left alone", in, got)
		}
	}
}
