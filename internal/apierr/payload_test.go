package apierr

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const jsonCT = "application/json; charset=utf-8"

const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

// validationEN is the shape Payload returns for a failed create (§11.1).
const validationEN = `{"errors":[{"name":"ValidationError","data":{"collection":"pages","errors":[
	{"label":"Title","message":"This field is required.","path":"title"},
	{"label":"Content > Layout","message":"This field requires at least 1 Row.","path":"layout"},
	{"label":"Slug","message":"This field is required.","path":"slug"}]},
	"message":"The following fields are invalid: Title, Content > Layout, Slug"}]}`

// validationDE is the §11.2 regression guard: the same body from a German
// project. Every human string differs; nothing the normaliser reads does.
const validationDE = `{"errors":[{"name":"ValidationError","data":{"collection":"pages","errors":[
	{"label":"Titel","message":"Dieses Feld ist erforderlich.","path":"title"},
	{"label":"Inhalt > Layout","message":"Dieses Feld erfordert mindestens 1 Zeile.","path":"layout"},
	{"label":"Slug","message":"Dieses Feld ist erforderlich.","path":"slug"}]},
	"message":"Die folgenden Felder sind ungültig: Titel, Inhalt > Layout, Slug"}]}`

func TestClassifyNormalisation(t *testing.T) {
	tests := []struct {
		name       string
		resp       Response
		wantCode   Code
		wantExit   int
		wantFields []string
		wantConf   Confidence
	}{
		{
			name:       "rule 2: ValidationError by name",
			resp:       Response{Status: 400, Method: "POST", URL: "http://h/api/pages", ContentType: jsonCT, Body: []byte(validationEN), Collection: "pages"},
			wantCode:   CodeValidationFailed,
			wantExit:   5,
			wantFields: []string{"title", "layout", "slug"},
		},
		{
			name:       "rule 2 survives German i18n",
			resp:       Response{Status: 400, Method: "POST", URL: "http://h/api/pages", ContentType: jsonCT, Body: []byte(validationDE), Collection: "pages"},
			wantCode:   CodeValidationFailed,
			wantExit:   5,
			wantFields: []string{"title", "layout", "slug"},
		},
		{
			name: "rule 3: QueryError data is an ARRAY",
			resp: Response{Status: 400, ContentType: jsonCT, Collection: "pages",
				Body: []byte(`{"errors":[{"name":"QueryError","data":["nope","deeper.nope"],"message":"…"}]}`)},
			wantCode:   CodeQueryPathInvalid,
			wantExit:   5,
			wantFields: []string{"nope", "deeper.nope"},
		},
		{
			name: "rule 4: the Mongoose field branch",
			resp: Response{Status: 400, ContentType: jsonCT, Collection: "pages",
				Body: []byte(`{"errors":[{"field":"slug","message":"Value must be unique"}]}`)},
			wantCode:   CodeValidationFailed,
			wantExit:   5,
			wantFields: []string{"slug"},
		},
		{
			name: "rule 1: docs + errors is a partial failure whatever the status",
			resp: Response{Status: 400, ContentType: jsonCT, Collection: "pages",
				Body: []byte(`{"docs":[{"id":11}],"errors":[{"id":10,"message":"bad","isPublic":true}],"message":"Unable to update 1 out of 2 Pages."}`)},
			wantCode: CodePartialFailure,
			wantExit: 7,
		},
		{
			name: "rule 5: message with no errors key, 404",
			resp: Response{Status: 404, Method: "GET", URL: "http://h/api/nope", ContentType: jsonCT,
				Body: []byte(`{"message":"Route not found \"/api/nope\""}`)},
			wantCode: CodeRouteNotFound,
			wantExit: 4,
		},
		{
			name: "rule 5: message with no errors key, 501",
			resp: Response{Status: 501, Method: "PUT", URL: "http://h/api/pages", ContentType: jsonCT,
				Body: []byte(`{"message":"Cannot PUT /api/pages"}`)},
			wantCode: CodeOperationUnsupported,
			wantExit: 10,
		},
		{
			name: "rule 6: 404 with an errors array is a missing document",
			resp: Response{Status: 404, Method: "GET", URL: "http://h/api/pages/999", ContentType: jsonCT,
				Body: []byte(`{"errors":[{"message":"Not Found"}]}`)},
			wantCode: CodeDocNotFound,
			wantExit: 4,
		},
		{
			name:     "401",
			resp:     Response{Status: 401, ContentType: jsonCT, Body: []byte(`{"errors":[{"message":"Unauthorized"}]}`)},
			wantCode: CodeAuthInvalid, wantExit: 2,
		},
		{
			name:     "423 locked",
			resp:     Response{Status: 423, ContentType: jsonCT, Body: []byte(`{"errors":[{"message":"Locked"}]}`)},
			wantCode: CodeDocLocked, wantExit: 3,
		},
		{
			name:     "429 rate limited",
			resp:     Response{Status: 429, ContentType: jsonCT, Body: []byte(`{"errors":[{"message":"Too many requests"}]}`)},
			wantCode: CodeRateLimited, wantExit: 3,
		},
		{
			name:     "413 request too large",
			resp:     Response{Status: 413, ContentType: jsonCT, Body: []byte(`{}`)},
			wantCode: CodeRequestTooLarge, wantExit: 5,
		},
		{
			name: "500 is server_error and is NOT retried",
			resp: Response{Status: 500, Method: "POST", URL: "http://h/api/pages", ContentType: jsonCT,
				Body: []byte(`{"errors":[{"message":"Something went wrong."}]}`)},
			wantCode: CodeServerError, wantExit: 6,
		},
		{
			name:     "502 server unavailable",
			resp:     Response{Status: 502, ContentType: jsonCT, Body: []byte(`{}`)},
			wantCode: CodeServerUnavailable, wantExit: 6,
		},
		{
			name:     "503 without Retry-After is an outage",
			resp:     Response{Status: 503, ContentType: jsonCT, Body: []byte(`{}`)},
			wantCode: CodeServerUnavailable, wantExit: 6,
		},
		{
			name:     "503 with Retry-After is a throttle",
			resp:     Response{Status: 503, ContentType: jsonCT, Body: []byte(`{}`), RetryAfter: "30"},
			wantCode: CodeServerBusy, wantExit: 3,
		},
		{
			name:     "unparseable body keeps the status signal",
			resp:     Response{Status: 401, ContentType: jsonCT, Body: []byte(`not json at all`)},
			wantCode: CodeAuthInvalid, wantExit: 2,
		},
		{
			name:     "unparseable body and an uninformative status degrades to unknown",
			resp:     Response{Status: 200, ContentType: jsonCT, Body: []byte(`not json at all`)},
			wantCode: CodeUnknown, wantExit: 1,
		},
		{
			name:     "errors[] of bare strings does not panic",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":["a string","another"]}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "errors[] of numbers does not panic",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":[1,2,3]}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "ValidationError whose data is an array falls through, not a crash",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":[{"name":"ValidationError","data":["oops"]}]}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "QueryError whose data is an object falls through, not a crash",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":[{"name":"QueryError","data":{"errors":[]}}]}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "400 Invalid JSON",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":[{"message":"Invalid JSON"}]}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "400 missing where",
			resp:     Response{Status: 400, ContentType: jsonCT, Body: []byte(`{"errors":[{"message":"Missing 'where' query of type object."}]}`)},
			wantCode: CodeWhereRequired, wantExit: 5,
		},
		{
			name: "400 on an upload collection with no error name is file_missing (structural)",
			resp: Response{Status: 400, Method: "POST", ContentType: jsonCT, UploadCollection: true,
				Body: []byte(`{"errors":[{"message":"Die Datei fehlt."}]}`)},
			wantCode: CodeFileMissing, wantExit: 5, wantConf: ConfidenceProbable,
		},
		{
			name: "the English string only raises confidence",
			resp: Response{Status: 400, Method: "POST", ContentType: jsonCT, UploadCollection: true,
				Body: []byte(`{"errors":[{"message":"No files were uploaded."}]}`)},
			wantCode: CodeFileMissing, wantExit: 5, wantConf: ConfidenceCertain,
		},
		{
			name: "an upload collection still reports a real ValidationError",
			resp: Response{Status: 400, Method: "POST", ContentType: jsonCT, UploadCollection: true, Collection: "media",
				Body: []byte(`{"errors":[{"name":"ValidationError","data":{"errors":[{"path":"alt","message":"required"}]}}]}`)},
			wantCode: CodeValidationFailed, wantExit: 5, wantFields: []string{"alt"},
		},
		{
			name:     "unmapped 4xx is the caller's problem",
			resp:     Response{Status: 405, ContentType: jsonCT, Body: []byte(`{}`)},
			wantCode: CodeBadRequestBody, wantExit: 5,
		},
		{
			name:     "unmapped 5xx is the server's problem",
			resp:     Response{Status: 599, ContentType: jsonCT, Body: []byte(`{}`)},
			wantCode: CodeServerUnavailable, wantExit: 6,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.resp)
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q (message %q)", got.Code, tc.wantCode, got.Message)
			}
			if got.Exit != tc.wantExit {
				t.Errorf("Exit = %d, want %d", got.Exit, tc.wantExit)
			}
			if tc.wantConf != "" && got.Confidence != tc.wantConf {
				t.Errorf("Confidence = %q, want %q", got.Confidence, tc.wantConf)
			}
			if got.Confidence == "" {
				t.Error("Confidence is always present and never absent (§11.1)")
			}
			if got.Fields == nil {
				t.Error("Fields is [] rather than absent when there are no field details (§11.1)")
			}
			if got.DidYouMean == nil {
				t.Error("DidYouMean must be [] rather than nil")
			}
			if tc.wantFields != nil {
				var paths []string
				for _, f := range got.Fields {
					paths = append(paths, f.Path)
				}
				if diff := cmp.Diff(tc.wantFields, paths); diff != "" {
					t.Errorf("field paths (-want +got):\n%s", diff)
				}
			}
			if got.HTTP == nil {
				t.Fatal("HTTP context must always be attached")
			}
			if got.HTTP.Status != tc.resp.Status {
				t.Errorf("http.status = %d, want %d", got.HTTP.Status, tc.resp.Status)
			}
			if got.HTTP.Attempts < 1 {
				t.Errorf("http.attempts = %d, want at least 1", got.HTTP.Attempts)
			}
			if got.Retriable != tc.wantCode.Retriable() {
				t.Errorf("Retriable = %v, want %v", got.Retriable, tc.wantCode.Retriable())
			}
		})
	}
}

// TestClassify403 is the one split the server cannot make: a 403 body is
// byte-identical whether the caller is unauthenticated or unpermitted.
func TestClassify403(t *testing.T) {
	body := []byte(`{"errors":[{"message":"You are not allowed to perform this action."}]}`)
	tests := []struct {
		name     string
		mode     string
		verified bool
		wantCode Code
		wantExit int
	}{
		{"anonymous is always auth_required", AuthModeAnonymous, false, CodeAuthRequired, 2},
		{"anonymous stays auth_required even if verified is set", AuthModeAnonymous, true, CodeAuthRequired, 2},
		{"api-key with a verified identity is access_denied", AuthModeAPIKey, true, CodeAccessDenied, 8},
		{"jwt with a verified identity is access_denied", AuthModeJWT, true, CodeAccessDenied, 8},
		{"api-key with an unverified identity is auth_required", AuthModeAPIKey, false, CodeAuthRequired, 2},
		{"jwt with an unverified identity is auth_required", AuthModeJWT, false, CodeAuthRequired, 2},
		{"an unset mode is treated as unauthenticated", "", false, CodeAuthRequired, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(Response{
				Status: 403, Method: "GET", URL: "http://h/api/users", ContentType: jsonCT, Body: body,
				AuthMode: tc.mode, IdentityVerified: tc.verified,
			})
			if got.Code != tc.wantCode || got.Exit != tc.wantExit {
				t.Errorf("got %s/%d, want %s/%d", got.Code, got.Exit, tc.wantCode, tc.wantExit)
			}
			if got.Hint == "" {
				t.Error("a 403 must always name its remedy")
			}
		})
	}
}

// TestValidationHintIsBranched is §11.1's fix for the worst hint in the spec:
// telling an agent to "fix every path" when it sent none of them makes it
// invent content for someone else's document.
func TestValidationHintIsBranched(t *testing.T) {
	tests := []struct {
		name         string
		sent         map[string]bool
		wantContains string
		wantOrder    []string
	}{
		{
			name:         "caller sent one of the fields",
			sent:         map[string]bool{"layout": true},
			wantContains: "Fix every path in error.fields",
			wantOrder:    []string{"layout", "title", "slug"},
		},
		{
			name:         "caller sent none of them",
			sent:         nil,
			wantContains: "already invalid on 3 field(s) you did not send",
			wantOrder:    []string{"title", "layout", "slug"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(Response{
				Status: 400, Method: "PATCH", URL: "http://h/api/pages/1", ContentType: jsonCT,
				Body: []byte(validationEN), Collection: "pages", SentPaths: tc.sent,
			})
			if !strings.Contains(got.Hint, tc.wantContains) {
				t.Errorf("hint = %q, want it to contain %q", got.Hint, tc.wantContains)
			}
			var paths []string
			for _, f := range got.Fields {
				paths = append(paths, f.Path)
			}
			if diff := cmp.Diff(tc.wantOrder, paths); diff != "" {
				t.Errorf("fields must be sorted sent-first (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClassifyNonJSON(t *testing.T) {
	tests := []struct {
		name        string
		resp        Response
		wantCode    Code
		wantExcerpt bool
		wantNextJS  bool
	}{
		{
			name: "next.js error boundary",
			resp: Response{Status: 404, ContentType: "text/html; charset=utf-8",
				Body: []byte(`<!DOCTYPE html><html id="__next_error__"><body>404</body></html>`)},
			wantCode: CodeNonJSONResponse, wantExcerpt: true, wantNextJS: true,
		},
		{
			name:     "plain html",
			resp:     Response{Status: 502, ContentType: "text/html", Body: []byte("<html>bad gateway</html>")},
			wantCode: CodeNonJSONResponse, wantExcerpt: true,
		},
		{
			name:     "no content type but JSON bytes is parsed anyway",
			resp:     Response{Status: 400, Body: []byte(`{"errors":[{"name":"QueryError","data":["x"]}]}`)},
			wantCode: CodeQueryPathInvalid,
		},
		{
			name:     "no content type and no body falls to the status",
			resp:     Response{Status: 401},
			wantCode: CodeAuthInvalid,
		},
		{
			name:     "application/vnd.api+json counts as JSON",
			resp:     Response{Status: 401, ContentType: "application/vnd.api+json", Body: []byte(`{}`)},
			wantCode: CodeAuthInvalid,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.resp)
			if got.Code != tc.wantCode {
				t.Fatalf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if tc.wantExcerpt {
				if got.HTTP.BodyExcerpt == "" {
					t.Error("a non-JSON response must carry a body_excerpt")
				}
				if len([]rune(got.HTTP.BodyExcerpt)) > excerptLimit+1 {
					t.Errorf("body_excerpt is %d runes, want <= %d", len([]rune(got.HTTP.BodyExcerpt)), excerptLimit)
				}
			}
			if tc.wantNextJS && !strings.Contains(got.Message, "Next.js") {
				t.Errorf("message should name Next.js's error boundary: %q", got.Message)
			}
		})
	}
}

func TestBodyExcerptIsTruncated(t *testing.T) {
	long := "<html>" + strings.Repeat("x", 5000) + "</html>"
	got := Classify(Response{Status: 500, ContentType: "text/html", Body: []byte(long)})
	if n := len([]rune(got.HTTP.BodyExcerpt)); n > excerptLimit+1 {
		t.Errorf("excerpt is %d runes, want <= %d", n, excerptLimit)
	}
}

// TestPartialFailureDetail covers §12.5's isPublic:false rewrite.
func TestPartialFailureDetail(t *testing.T) {
	body := `{"docs":[{"id":11,"title":"Bulk Test"}],
	 "errors":[{"id":10,"isPublic":true,"message":"The following field is invalid: Content > Layout"},
	           {"id":12,"isPublic":false,"message":"Something went wrong."}],
	 "message":"Unable to update 2 out of 3 Pages."}`
	got := Classify(Response{Status: 400, Method: "PATCH", URL: "http://h/api/pages", ContentType: jsonCT, Body: []byte(body), Collection: "pages"})
	if got.Code != CodePartialFailure || got.Exit != 7 {
		t.Fatalf("got %s/%d", got.Code, got.Exit)
	}
	if got.Retriable {
		t.Error("partial_failure must never be retriable")
	}
	if !strings.Contains(got.Hint, "ALREADY COMMITTED") {
		t.Errorf("hint must warn that the successful writes committed: %q", got.Hint)
	}
	if len(got.Failures) != 2 {
		t.Fatalf("Failures = %d, want 2", len(got.Failures))
	}
	if got.Failures[0].Code != CodeValidationFailed {
		t.Errorf("failures[0].code = %q", got.Failures[0].Code)
	}
	if got.Failures[1].Code != CodeServerError {
		t.Errorf("failures[1].code = %q, want server_error for isPublic:false", got.Failures[1].Code)
	}
	if !strings.Contains(got.Failures[1].Message, "debug: true") {
		t.Errorf("an isPublic:false failure must say the message was withheld: %q", got.Failures[1].Message)
	}
	if !strings.Contains(got.Message, "1 of 3") {
		t.Errorf("message should count the successes: %q", got.Message)
	}
}

// TestRawIsRedacted is the §11.1 / §5.3 rule that made "byte-faithful raw"
// untenable: a bulk update against the auth collection echoes every touched
// user, apiKey in plaintext.
func TestRawIsRedacted(t *testing.T) {
	body := `{"docs":[{"id":1,"apiKey":"` + fixtureKey + `"}],"errors":[{"id":2,"message":"nope"}]}`
	t.Run("redacted by default", func(t *testing.T) {
		got := Classify(Response{Status: 400, ContentType: jsonCT, Body: []byte(body), Collection: "users", IncludeRaw: true})
		if strings.Contains(string(got.Raw), fixtureKey) {
			t.Fatalf("raw leaked the API key: %s", got.Raw)
		}
		if diff := cmp.Diff([]string{"docs[0].apiKey"}, got.RawRedactedPaths); diff != "" {
			t.Errorf("RawRedactedPaths (-want +got):\n%s", diff)
		}
		var v any
		if err := json.Unmarshal(got.Raw, &v); err != nil {
			t.Errorf("raw is not valid JSON after redaction: %v", err)
		}
	})
	t.Run("no-redact reproduces the wire bytes", func(t *testing.T) {
		got := Classify(Response{Status: 400, ContentType: jsonCT, Body: []byte(body), IncludeRaw: true, NoRedact: true})
		if string(got.Raw) != body {
			t.Errorf("raw = %s, want the wire bytes", got.Raw)
		}
	})
	t.Run("no-raw drops it entirely", func(t *testing.T) {
		got := Classify(Response{Status: 400, ContentType: jsonCT, Body: []byte(body)})
		if got.Raw != nil {
			t.Errorf("raw = %s, want absent", got.Raw)
		}
	})
	t.Run("an unchanged body stays byte-faithful", func(t *testing.T) {
		clean := `{"errors":[{"message":"nope"}]}`
		got := Classify(Response{Status: 400, ContentType: jsonCT, Body: []byte(clean), IncludeRaw: true})
		if string(got.Raw) != clean {
			t.Errorf("raw = %s, want %s", got.Raw, clean)
		}
		if got.RawRedactedPaths != nil {
			t.Errorf("RawRedactedPaths = %v, want nil", got.RawRedactedPaths)
		}
	})
}

// TestURLIsRedacted covers §5.3's mandatory redaction of error.http.url.
func TestURLIsRedacted(t *testing.T) {
	got := Classify(Response{
		Status: 401, Method: "GET", ContentType: jsonCT, Body: []byte(`{}`),
		URL: "http://admin:hunter2@localhost:3900/api/users/me?api-key=" + fixtureKey,
	})
	if strings.Contains(got.HTTP.URL, "hunter2") || strings.Contains(got.HTTP.URL, fixtureKey) {
		t.Errorf("error.http.url leaked a credential: %q", got.HTTP.URL)
	}
}

// TestClassifyNeverPanics throws deliberately hostile bodies at the normaliser.
// formatErrors lets a project author return literally anything, and afterError
// hooks can rewrite both the body and the status.
func TestClassifyNeverPanics(t *testing.T) {
	bodies := []string{
		``, `null`, `[]`, `[1,2,3]`, `"a string"`, `123`, `true`,
		`{"errors":null}`, `{"errors":{}}`, `{"errors":[null]}`,
		`{"errors":[{"name":"ValidationError"}]}`,
		`{"errors":[{"name":"ValidationError","data":null}]}`,
		`{"errors":[{"name":"ValidationError","data":{"errors":null}}]}`,
		`{"errors":[{"name":"ValidationError","data":{"errors":[null,1,"x"]}}]}`,
		`{"errors":[{"name":"QueryError","data":null}]}`,
		`{"errors":[{"field":123}]}`,
		`{"docs":null,"errors":[]}`,
		`{"docs":[],"errors":[{}]}`,
		`{"message":null}`,
		`{"stack":"…","errors":[{"message":"x","stack":"y"}]}`,
	}
	statuses := []int{0, 200, 400, 401, 403, 404, 418, 500, 501, 503, 999}
	for _, b := range bodies {
		for _, st := range statuses {
			e := Classify(Response{Status: st, Method: "POST", URL: "http://h/api/x", ContentType: jsonCT, Body: []byte(b), IncludeRaw: true})
			if e == nil {
				t.Fatalf("Classify returned nil for status %d body %q", st, b)
			}
			if !e.Code.Known() {
				t.Errorf("status %d body %q produced the undeclared code %q", st, b, e.Code)
			}
			if e.Exit < 1 || e.Exit > MaxExit {
				t.Errorf("status %d body %q produced exit %d", st, b, e.Exit)
			}
		}
	}
}

func TestServerMessageIsPreferredWhenSpecific(t *testing.T) {
	t.Run("a specific message is appended", func(t *testing.T) {
		got := Classify(Response{Status: 400, ContentType: jsonCT,
			Body: []byte(`{"errors":[{"message":"Slug must be unique across the site"}]}`)})
		if !strings.Contains(got.Message, "Slug must be unique") {
			t.Errorf("message = %q", got.Message)
		}
	})
	t.Run("a generic mask is not", func(t *testing.T) {
		got := Classify(Response{Status: 500, ContentType: jsonCT,
			Body: []byte(`{"errors":[{"message":"Something went wrong."}]}`)})
		if strings.Contains(got.Message, "Server said") {
			t.Errorf("a masked message adds nothing: %q", got.Message)
		}
	})
}
