package apierr

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNewDefaults(t *testing.T) {
	e := New(CodeValidationFailed, "%d field(s) are invalid on %q.", 3, "pages")
	if e.Exit != 5 {
		t.Errorf("Exit = %d, want 5", e.Exit)
	}
	if e.Message != `3 field(s) are invalid on "pages".` {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Confidence != ConfidenceCertain {
		t.Errorf("Confidence = %q, want certain", e.Confidence)
	}
	if e.Retriable {
		t.Error("validation_failed is not retriable")
	}
	if e.Fields == nil || e.DidYouMean == nil {
		t.Error("Fields and DidYouMean must be [] rather than nil")
	}
	if e.Docs != DefaultDocs {
		t.Errorf("Docs = %q", e.Docs)
	}
	if e.Hint == "" {
		t.Error("a hint is mandatory")
	}
}

// TestMessagesAreRedacted: server messages routinely embed URLs, and a URL can
// carry a credential (§5.3).
func TestMessagesAreRedacted(t *testing.T) {
	const key = "paycli-dev-key-deadbeefdeadbeef"
	e := New(CodeServerError, "failed calling http://admin:hunter2@h/api?api-key=%s", key)
	if strings.Contains(e.Message, key) || strings.Contains(e.Message, "hunter2") {
		t.Errorf("message leaked a credential: %q", e.Message)
	}
	e.WithHint("retry at http://admin:hunter2@h/api?api-key=%s", key)
	if strings.Contains(e.Hint, key) {
		t.Errorf("hint leaked a credential: %q", e.Hint)
	}
}

func TestErrorStringAndUnwrap(t *testing.T) {
	root := errors.New("connection reset")
	e := Wrap(root, CodeNetworkUnreachable, "could not reach the server")
	if got, want := e.Error(), "network_unreachable (exit 6): could not reach the server"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(e, root) {
		t.Error("errors.Is must find the wrapped cause")
	}
	var target *Error
	if !errors.As(e, &target) {
		t.Error("errors.As must find the *Error")
	}
}

func TestIsMatchesOnCode(t *testing.T) {
	e := New(CodeAccessDenied, "nope")
	if !errors.Is(e, New(CodeAccessDenied, "different message")) {
		t.Error("errors.Is must match on the code alone")
	}
	if errors.Is(e, New(CodeAuthRequired, "nope")) {
		t.Error("errors.Is must not match a different code")
	}
	if !HasCode(e, CodeAccessDenied) {
		t.Error("HasCode")
	}
	if CodeOf(e) != CodeAccessDenied {
		t.Error("CodeOf")
	}
	if CodeOf(nil) != "" {
		t.Error("CodeOf(nil) must be empty")
	}
	if CodeOf(errors.New("plain")) != CodeInternal {
		t.Error("CodeOf of a plain error must be internal")
	}
}

func TestFrom(t *testing.T) {
	if From(nil) != nil {
		t.Error("From(nil) must be nil")
	}
	e := New(CodeDocNotFound, "x")
	if From(e) != e {
		t.Error("From must return an existing *Error unchanged")
	}
	got := From(errors.New("boom"))
	if got.Code != CodeInternal || got.Exit != 1 {
		t.Errorf("From(plain) = %s/%d", got.Code, got.Exit)
	}
	if !strings.Contains(got.Message, "boom") {
		t.Errorf("From must keep the original text: %q", got.Message)
	}
}

func TestRecode(t *testing.T) {
	e := New(CodeRouteNotFound, "no route")
	defaultHint := e.Hint
	e.Recode(CodeCollectionUnknown)
	if e.Code != CodeCollectionUnknown || e.Exit != 10 {
		t.Errorf("Recode gave %s/%d, want collection_unknown/10", e.Code, e.Exit)
	}
	if e.Hint == defaultHint {
		t.Error("Recode should adopt the new code's default hint")
	}

	// A custom hint survives a recode.
	e2 := New(CodeRouteNotFound, "no route").WithHint("custom")
	e2.Recode(CodeCollectionUnknown)
	if e2.Hint != "custom" {
		t.Errorf("Recode clobbered a custom hint: %q", e2.Hint)
	}
}

// TestJSONKeyOrder pins §11.1's error object: agents read these envelopes by
// eye as often as by parser, and the order is part of the contract.
func TestJSONKeyOrder(t *testing.T) {
	e := New(CodeValidationFailed, "3 fields are invalid.").
		WithFields(Field{Path: "title", Label: "Title", Message: "required", Sent: true}).
		WithDidYouMean("pages").
		WithHTTP(&HTTP{Status: 400, Method: "POST", URL: "http://h/api/pages", PayloadErrorName: "ValidationError", Attempts: 1}).
		WithRaw([]byte(`{"errors":[]}`), false)
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"code", "exit", "message", "hint", "retriable", "confidence", "fields", "did_you_mean", "docs", "http", "raw"}
	if diff := cmp.Diff(want, keyOrder(t, b)); diff != "" {
		t.Errorf("key order (-want +got):\n%s", diff)
	}
	if strings.Contains(string(b), "RawRedactedPaths") || strings.Contains(string(b), "wrapped") {
		t.Error("internal fields must never reach the envelope")
	}
}

func TestJSONOmitsEmptyOptionalBlocks(t *testing.T) {
	b, err := json.Marshal(New(CodeInvalidArgs, "bad flag"))
	if err != nil {
		t.Fatal(err)
	}
	got := keyOrder(t, b)
	for _, absent := range []string{"http", "raw", "failures", "likely_causes"} {
		for _, k := range got {
			if k == absent {
				t.Errorf("%q must be absent when empty", absent)
			}
		}
	}
	// fields and did_you_mean are [] rather than absent (§11.1).
	var present int
	for _, k := range got {
		if k == "fields" || k == "did_you_mean" {
			present++
		}
	}
	if present != 2 {
		t.Errorf("fields and did_you_mean must always be present, got %v", got)
	}
}

func TestSortFields(t *testing.T) {
	in := []Field{
		{Path: "title", Sent: false},
		{Path: "layout", Sent: true},
		{Path: "slug", Sent: false},
		{Path: "heroImage", Sent: true},
	}
	got := SortFields(in)
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	want := []string{"layout", "heroImage", "title", "slug"}
	if diff := cmp.Diff(want, paths); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if in[0].Path != "title" {
		t.Error("SortFields mutated its input")
	}
	if !AnySent(in) {
		t.Error("AnySent")
	}
	if AnySent([]Field{{Path: "x"}}) {
		t.Error("AnySent on an all-unsent slice")
	}
	if got := SortFields(nil); got == nil || len(got) != 0 {
		t.Errorf("SortFields(nil) = %v, want an empty slice", got)
	}
}

func TestHumanLine(t *testing.T) {
	e := New(CodeValidationFailed, "3 fields are invalid on %q.", "pages").WithHint("Fix them.")
	want := `pay: validation_failed (exit 5): 3 fields are invalid on "pages". — hint: Fix them.`
	if got := HumanLine(e); got != want {
		t.Errorf("HumanLine() = %q, want %q", got, want)
	}
	if HumanLine(nil) != "" {
		t.Error("HumanLine(nil) must be empty")
	}
}

func keyOrder(t *testing.T, b []byte) []string {
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

// TestWithRawKeepsTheEnvelopeEncodable covers the misrouted-request case: a
// request that misses the API prefix is answered by Next.js with an HTML page,
// and those bytes must not make the error envelope unencodable.
func TestWithRawKeepsTheEnvelopeEncodable(t *testing.T) {
	const html = "<!DOCTYPE html><html><body>404</body></html>"
	for _, noRedact := range []bool{false, true} {
		e := New(CodeRouteNotFound, "no route").WithRaw([]byte(html), noRedact)
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("no-redact=%v: marshal: %v", noRedact, err)
		}
		var back struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("no-redact=%v: raw is not a JSON string: %v\n%s", noRedact, err, b)
		}
		if back.Raw != html {
			t.Errorf("no-redact=%v: raw = %q, want the body verbatim", noRedact, back.Raw)
		}
	}
}
