package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// applyPipe runs a `pay blocks …` stage and feeds its envelope to `pay apply`,
// which is what the shell does between two processes. Both halves run against
// the same seeded cache, so nothing reaches the network.
func applyPipe(t *testing.T, home, baseURL string, stage, apply []string, stdin string) cliResult {
	t.Helper()
	env := seededEnv(baseURL)
	first := cliRun(t, invocation{Home: home, Env: env, Args: stage, Stdin: stdin})
	if first.Code != 0 {
		t.Fatalf("the edit stage failed (exit %d): %s", first.Code, first.Stdout)
	}
	return cliRun(t, invocation{Home: home, Env: env, Args: apply, Stdin: first.Stdout})
}

// dryRunBody digs the PATCH body out of a --dry-run preview.
func dryRunBody(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	req, ok := r.data(t)["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request in the dry-run preview: %s", r.Stdout)
	}
	body, ok := req["body"].(map[string]any)
	if !ok {
		t.Fatalf("no body in the dry-run preview: %v", req)
	}
	return body
}

// The whole point of `edits`: a pipeline that moved one block writes ONE key.
// PATCHing the document back wholesale would also rewrite _status, createdAt
// and every relationship the read expanded.
func TestApplyWritesOnlyTheEditedFields(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta", "content", "mediaBlock"})

	doc := `{"id":12,"title":"Home","slug":"home","_status":"published",
	  "createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z",
	  "layout":[{"id":"a1","blockType":"cta"},{"id":"b2","blockType":"content"}]}`

	r := applyPipe(t, home, srv.URL,
		[]string{"blocks", "mv", "type:content", "--first"},
		[]string{"apply", "--dry-run"},
		pageEnvelope(t, doc))
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}

	body := dryRunBody(t, r)
	if len(body) != 1 {
		t.Fatalf("the PATCH carries %d fields, want exactly `layout`: %v", len(body), sortedMapKeys(body))
	}
	if _, ok := body["layout"]; !ok {
		t.Fatalf("body = %v", body)
	}
	if got := blockTypes(t, body["layout"]); strings.Join(got, ",") != "content,cta" {
		t.Errorf("order = %v", got)
	}

	req, _ := r.data(t)["request"].(map[string]any)
	if req["method"] != "PATCH" {
		t.Errorf("method = %v", req["method"])
	}
	// The destination came out of the piped envelope; nothing was retyped.
	if !strings.Contains(req["url"].(string), "/api/pages/12") {
		t.Errorf("url = %v", req["url"])
	}
}

// --all-fields is the escape hatch for a document edited by hand. It says so
// out loud, because it sends back everything the read returned.
func TestApplyAllFieldsWarnsAndDropsServerOwnedKeys(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"apply", "--all-fields", "--dry-run"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	for _, k := range []string{"id", "createdAt", "updatedAt"} {
		if _, ok := body[k]; ok {
			t.Errorf("--all-fields sent the server-owned %q", k)
		}
	}
	if _, ok := body["title"]; !ok {
		t.Errorf("--all-fields did not send an untouched field: %v", sortedMapKeys(body))
	}
	if !r.hasWarning(t, "apply_all_fields") {
		t.Error("--all-fields must announce what it widened")
	}
}

// An envelope no transform touched has no field list, and writing the whole
// document is not a safe default to fall back to.
func TestApplyRefusesAnEnvelopeWithNoEdits(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"apply", "--dry-run"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if got := r.code(t); got != "no_edits" {
		t.Fatalf("code = %q, want no_edits (%s)", got, r.Stdout)
	}
	if r.Code != 5 {
		t.Errorf("exit = %d, want 5", r.Code)
	}
	e, _ := r.Env["error"].(map[string]any)
	hint, _ := e["hint"].(string)
	if !strings.Contains(hint, "--data @-") {
		t.Errorf("the hint must name the way to write the whole document: %q", hint)
	}
}

// A bare document has no target, so `pay apply` cannot know where it goes —
// and must say so rather than guess a collection.
func TestApplyNeedsATargetOrArguments(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})
	env := seededEnv(srv.URL)

	edited := cliRun(t, invocation{
		Home: home, Env: env,
		Args: []string{"blocks", "mv", "0", "--last"}, Stdin: pageDoc,
	})
	if edited.Code != 0 {
		t.Fatalf("edit failed: %s", edited.Stdout)
	}

	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"apply", "--dry-run"}, Stdin: edited.Stdout})
	if got := r.code(t); got != "invalid_args" {
		t.Fatalf("code = %q, want invalid_args", got)
	}
	if !strings.Contains(r.Stdout, "carries no target") {
		t.Errorf("the message does not say what is missing: %s", r.Stdout)
	}

	// Naming it explicitly is the documented way out.
	r = cliRun(t, invocation{
		Home: home, Env: env,
		Args: []string{"apply", "pages", "12", "--dry-run"}, Stdin: edited.Stdout,
	})
	if r.Code != 0 {
		t.Fatalf("explicit arguments did not resolve it: %s", r.Stdout)
	}
	if _, ok := dryRunBody(t, r)["layout"]; !ok {
		t.Errorf("body = %v", dryRunBody(t, r))
	}
}

// `--path .layout` between the edit and the apply leaves apply with rows and no
// document. That has to be a named failure, not a body of nonsense.
func TestApplyRefusesABareArray(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"apply", "--dry-run"},
		Stdin: `[{"id":"a","blockType":"cta"}]`,
	})
	if got := r.code(t); got != "no_edits" {
		t.Fatalf("code = %q, want no_edits (%s)", got, r.Stdout)
	}
}

// The op log survives onto the envelope of the command that performed the
// write, so the record of WHAT was done lives with the thing that did it.
func TestApplyCarriesTheOpLogOntoItsOwnEnvelope(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta", "content", "mediaBlock"})

	r := applyPipe(t, home, srv.URL,
		[]string{"blocks", "rm", "type:content"},
		[]string{"apply", "--dry-run"},
		pageEnvelope(t, pageDoc))
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	edits, _ := r.Env["edits"].(map[string]any)
	if edits == nil {
		t.Fatal("apply dropped the edit log")
	}
	ops, _ := edits["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("ops = %v", ops)
	}
	op, _ := ops[0].(map[string]any)
	if op["command"] != "blocks rm" {
		t.Errorf("ops[0] = %v", op)
	}
}

// --field narrows the write further. It can only ever be a subset of the piped
// document, never a way to widen it.
func TestApplyFieldNarrowsTheWrite(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta", "content", "mediaBlock"})

	r := applyPipe(t, home, srv.URL,
		[]string{"blocks", "mv", "0", "--last"},
		[]string{"apply", "--field", "title", "--dry-run"},
		pageEnvelope(t, pageDoc))
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	if len(body) != 1 || body["title"] != "Home" {
		t.Fatalf("body = %v", body)
	}
}

// A field the pipe dropped between the edit and the apply cannot be written,
// and the message has to name the cause rather than send a null.
func TestApplyRefusesAMissingField(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"apply", "--field", "nosuchfield", "--dry-run"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if got := r.code(t); got != "invalid_args" {
		t.Fatalf("code = %q", got)
	}
	if !strings.Contains(r.Stdout, "nosuchfield") {
		t.Errorf("the message does not name the field: %s", r.Stdout)
	}
}

// ---------------------------------------------------------------------------
// --data @- envelope unwrapping
// ---------------------------------------------------------------------------

// `pay get … | pay update … --data @-` is the shape everyone reaches for first.
// Before the unwrap it posted {"ok":true,"data":{…},"meta":{…}} as the body,
// which Payload accepts with a 2xx and silently drops every key of.
func TestUpdateDataUnwrapsAPipedEnvelope(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"update", "pages", "12", "--data", "@-", "--dry-run"},
		Stdin: pageEnvelope(t, `{"id":12,"title":"Home"}`),
	})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	if body["title"] != "Home" {
		t.Fatalf("body = %v — the envelope's .data was not used", body)
	}
	for _, k := range []string{"ok", "v", "data_kind", "meta", "data"} {
		if _, ok := body[k]; ok {
			t.Errorf("the envelope key %q reached the request body", k)
		}
	}
	if !r.hasWarning(t, WarnEnvelopeUnwrapped) {
		t.Error("the unwrap must be announced: the caller asked to send one thing and PayCLI sent another")
	}
}

// A Payload document that happens to have an `ok` field is not an envelope.
// Unwrapping one would drop the whole document.
func TestUpdateDataDoesNotUnwrapADocumentWithAnOkField(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"update", "pages", "12", "--data", "@-", "--dry-run"},
		Stdin: `{"ok":true,"title":"Home"}`,
	})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	if body["title"] != "Home" || body["ok"] != true {
		t.Fatalf("body = %v — a document with an `ok` column was mistaken for an envelope", body)
	}
	if r.hasWarning(t, WarnEnvelopeUnwrapped) {
		t.Error("nothing was unwrapped, so nothing should be announced")
	}
}

// An error envelope holds no document at all; posting its `error` object as a
// body would be nonsense.
func TestUpdateDataRefusesAnErrorEnvelope(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)

	upstream := `{"ok":false,"v":1,"command":"get","data_kind":"error",
	  "error":{"code":"doc_not_found","exit":4,"message":"no document 99","retriable":false,
	           "confidence":"certain","fields":[],"did_you_mean":[]},
	  "meta":{"request_id":"t"},"warnings":[]}`
	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL),
		Args:  []string{"update", "pages", "12", "--data", "@-", "--dry-run"},
		Stdin: upstream,
	})
	if got := r.code(t); got != "doc_not_found" {
		t.Fatalf("code = %q, want the upstream's", got)
	}
	if r.Code != 4 {
		t.Errorf("exit = %d, want 4", r.Code)
	}
}

// A global goes to POST /globals/{slug} and returns from its own branch, which
// is how it came to be the one envelope in the family that lost its edit log on
// --dry-run. The preview must explain itself exactly like the collection one.
func TestApplyGlobalDryRunKeepsTheOpLog(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta"})
	env := seededEnv(srv.URL)

	global := `{"ok":true,"v":1,"command":"globals get","data_kind":"global",
	  "target":{"kind":"global","slug":"header"},
	  "data":{"id":1,"globalType":"header","navItems":[
	    {"id":"n1","link":{"label":"Home"}},{"id":"n2","link":{"label":"Docs"}}]},
	  "meta":{"request_id":"t"},"warnings":[]}`

	edited := cliRun(t, invocation{
		Home: home, Env: env,
		Args: []string{"blocks", "mv", "id:n2", "--first", "--field", "navItems"}, Stdin: global,
	})
	if edited.Code != 0 {
		t.Fatalf("edit failed: %s", edited.Stdout)
	}

	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"apply", "--dry-run"}, Stdin: edited.Stdout})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	req, _ := r.data(t)["request"].(map[string]any)
	if req["method"] != "POST" || !strings.Contains(req["url"].(string), "/globals/header") {
		t.Errorf("request = %v", req)
	}
	body, _ := req["body"].(map[string]any)
	if len(body) != 1 {
		t.Errorf("the PATCH carries %d fields, want only navItems: %v", len(body), sortedMapKeys(body))
	}

	edits, _ := r.Env["edits"].(map[string]any)
	if edits == nil {
		t.Fatal("the global dry-run dropped the edit log")
	}
	ops, _ := edits["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("ops = %v", ops)
	}
	if op, _ := ops[0].(map[string]any); op["command"] != "blocks mv" {
		t.Errorf("ops[0] = %v", op)
	}
}
