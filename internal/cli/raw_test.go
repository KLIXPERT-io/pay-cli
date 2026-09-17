package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestNormalizeRawMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
		code apierr.Code
	}{
		{"upper", "GET", "GET", ""},
		{"lower", "delete", "DELETE", ""},
		{"padded", "  patch  ", "PATCH", ""},
		{"head", "head", "HEAD", ""},
		{"options", "options", "OPTIONS", ""},
		{"put", "put", "PUT", ""},
		{"typo", "DELTE", "", apierr.CodeInvalidArgs},
		{"unsupported verb", "TRACE", "", apierr.CodeInvalidArgs},
		{"empty", "", "", apierr.CodeInvalidArgs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeRawMethod(tc.in)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("normalizeRawMethod(%q) = error %v, want %q", tc.in, err, tc.want)
				}
				if got != tc.want {
					t.Fatalf("normalizeRawMethod(%q) = %q, want %q", tc.in, got, tc.want)
				}
				return
			}
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("normalizeRawMethod(%q) = %v, want code %s", tc.in, err, tc.code)
			}
		})
	}
}

func TestNormalizeRawMethodSuggests(t *testing.T) {
	t.Parallel()
	_, err := normalizeRawMethod("DELTE")
	e, ok := apierr.As(err)
	if !ok {
		t.Fatalf("want an *apierr.Error, got %T", err)
	}
	found := false
	for _, s := range e.DidYouMean {
		if s == "DELETE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("did_you_mean = %v, want it to contain DELETE", e.DidYouMean)
	}
}

func TestResolveRawPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		in           string
		wantPath     string
		wantAbsolute bool
	}{
		{"relative", "users/me", "/users/me", false},
		{"relative with spaces", "  pages  ", "/pages", false},
		{"absolute api", "/api/graphql", "/api/graphql", true},
		{"absolute admin", "/admin/login", "/admin/login", true},
		{"empty", "", "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, abs := resolveRawPath(tc.in)
			if got != tc.wantPath || abs != tc.wantAbsolute {
				t.Fatalf("resolveRawPath(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, abs, tc.wantPath, tc.wantAbsolute)
			}
		})
	}
}

func TestIsReadMethod(t *testing.T) {
	t.Parallel()
	read := []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	write := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for _, m := range read {
		if !isReadMethod(m) {
			t.Errorf("isReadMethod(%s) = false, want true", m)
		}
		if rawWouldAffect(m) != 0 {
			t.Errorf("rawWouldAffect(%s) = %d, want 0", m, rawWouldAffect(m))
		}
	}
	for _, m := range write {
		if isReadMethod(m) {
			t.Errorf("isReadMethod(%s) = true, want false", m)
		}
		if rawWouldAffect(m) != 1 {
			t.Errorf("rawWouldAffect(%s) = %d, want 1", m, rawWouldAffect(m))
		}
	}
}

func TestBuildRawQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      []string
		want    string
		wantErr bool
	}{
		{"none", nil, "", false},
		{"simple", []string{"limit=1"}, "limit=1", false},
		{
			"bracketed survives",
			[]string{"where[slug][equals]=home"},
			"where%5Bslug%5D%5Bequals%5D=home",
			false,
		},
		{
			"sorted and repeatable",
			[]string{"limit=1", "depth=0"},
			"depth=0&limit=1",
			false,
		},
		{"empty value is legal", []string{"draft="}, "draft=", false},
		{"missing equals", []string{"limit"}, "", true},
		{"empty key", []string{"=1"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildRawQuery(tc.in)
			if tc.wantErr {
				if !apierr.HasCode(err, apierr.CodeInvalidArgs) {
					t.Fatalf("buildRawQuery(%v) = %q, %v; want invalid_args", tc.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildRawQuery(%v) = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("buildRawQuery(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildRawHeaders(t *testing.T) {
	t.Parallel()
	h, err := buildRawHeaders([]string{"X-Tenant: acme", "X-Trace:  abc  "})
	if err != nil {
		t.Fatalf("buildRawHeaders: %v", err)
	}
	if got := h.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme", got)
	}
	if got := h.Get("X-Trace"); got != "abc" {
		t.Fatalf("X-Trace = %q, want abc", got)
	}
	if _, err := buildRawHeaders([]string{"nope"}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("buildRawHeaders(bad) = %v, want invalid_args", err)
	}
	if _, err := buildRawHeaders([]string{": value"}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("buildRawHeaders(empty name) = %v, want invalid_args", err)
	}
}

func TestRawBodyPreview(t *testing.T) {
	t.Parallel()
	if got := rawBodyPreview(nil, nil); got != nil {
		t.Fatalf("rawBodyPreview(nil) = %s, want nil", got)
	}
	if got := rawBodyPreview([]byte(`{"a":1}`), nil); string(got) != `{"a":1}` {
		t.Fatalf("rawBodyPreview(json) = %s", got)
	}
	got := rawBodyPreview([]byte("not json"), nil)
	if !json.Valid(got) {
		t.Fatalf("rawBodyPreview(non-json) = %s, want valid JSON", got)
	}
	if string(got) != `"not json"` {
		t.Fatalf("rawBodyPreview(non-json) = %s, want a JSON string", got)
	}
	multi := rawBodyPreview([]byte(`{"a":1}`), &rawMultipart{})
	if string(multi) != `"<multipart/form-data>"` {
		t.Fatalf("rawBodyPreview(multipart) = %s", multi)
	}
}

func TestDecodeRawData(t *testing.T) {
	t.Parallel()
	if got := decodeRawData(nil); got != nil {
		t.Fatalf("decodeRawData(nil) = %v, want nil", got)
	}
	if got := decodeRawData([]byte("<html>")); got != nil {
		t.Fatalf("decodeRawData(html) = %v, want nil", got)
	}
	// A 19-digit id must survive: json.Number, never float64.
	v := decodeRawData([]byte(`{"id":9007199254740993}`))
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("decodeRawData = %T, want map", v)
	}
	num, ok := m["id"].(json.Number)
	if !ok {
		t.Fatalf("id = %T, want json.Number", m["id"])
	}
	if num.String() != "9007199254740993" {
		t.Fatalf("id = %s, want 9007199254740993 (precision lost)", num)
	}
}

func TestAnnotateRawError(t *testing.T) {
	t.Parallel()
	notFound := apierr.New(apierr.CodeRouteNotFound, `Route not found "/users/me"`)

	t.Run("host-absolute path missing the api prefix gets the hint", func(t *testing.T) {
		t.Parallel()
		err := annotateRawError(notFound, "/users/me", "/api")
		e, ok := apierr.As(err)
		if !ok {
			t.Fatalf("want *apierr.Error, got %T", err)
		}
		if !strings.Contains(e.Hint, "bypassing the profile's api_path") {
			t.Fatalf("hint = %q, want the api_path diagnosis", e.Hint)
		}
		if !strings.Contains(e.Hint, "/api/users/me") {
			t.Fatalf("hint = %q, want it to spell the corrected path", e.Hint)
		}
	})

	// Finding 3. On a real project the leading-slash mistake does NOT produce
	// route_not_found: the path misses the API mount entirely, so the Next.js
	// app answers with an HTML 404 and the error is non_json_response. The
	// annotation skipped that code, so the agent got the generic hint pointing
	// at `pay doctor` — which reports every check ok, because base_url and
	// api_path are both fine. The path is the problem.
	t.Run("an HTML 404 from Next.js gets the same diagnosis", func(t *testing.T) {
		t.Parallel()
		htmlErr := apierr.New(apierr.CodeNonJSONResponse,
			"The server returned text/html; charset=utf-8, not JSON (HTTP 404).")
		err := annotateRawError(htmlErr, "/pages", "/api")
		e, ok := apierr.As(err)
		if !ok {
			t.Fatalf("want *apierr.Error, got %T", err)
		}
		if strings.Contains(e.Hint, "pay doctor") {
			t.Errorf("hint still sends the agent to `pay doctor`, which reports ok: %q", e.Hint)
		}
		for _, want := range []string{"sent verbatim", "pages", "/api/pages"} {
			if !strings.Contains(e.Hint, want) {
				t.Errorf("hint = %q, want it to name %q", e.Hint, want)
			}
		}
	})

	t.Run("a non-JSON response on a RELATIVE path is left alone", func(t *testing.T) {
		t.Parallel()
		htmlErr := apierr.New(apierr.CodeNonJSONResponse, "not JSON")
		if err := annotateRawError(htmlErr, "pages", "/api"); err != error(htmlErr) {
			t.Fatalf("annotateRawError rewrote a relative-path non-JSON error")
		}
	})

	t.Run("relative path is left alone", func(t *testing.T) {
		t.Parallel()
		err := annotateRawError(notFound, "users/me", "/api")
		if err != error(notFound) {
			t.Fatalf("annotateRawError rewrote a relative-path error")
		}
	})

	t.Run("already prefixed is left alone", func(t *testing.T) {
		t.Parallel()
		err := annotateRawError(notFound, "/api/nope", "/api")
		if err != error(notFound) {
			t.Fatalf("annotateRawError rewrote an already-prefixed error")
		}
	})

	t.Run("unrelated code is left alone", func(t *testing.T) {
		t.Parallel()
		other := apierr.New(apierr.CodeServerError, "boom")
		if err := annotateRawError(other, "/users/me", "/api"); err != error(other) {
			t.Fatalf("annotateRawError rewrote a non-404")
		}
	})
}

func TestRawFileBodyRejectsNonJSONFields(t *testing.T) {
	t.Parallel()
	_, _, err := rawFileBody("testdata-does-not-exist", "", []byte("not json"))
	if !apierr.HasCode(err, apierr.CodeBadRequestBody) {
		t.Fatalf("rawFileBody(bad fields) = %v, want bad_request_body", err)
	}
}

func TestRawFileBodyMissingFile(t *testing.T) {
	t.Parallel()
	_, _, err := rawFileBody("./definitely/not/here.png", "", nil)
	if !apierr.HasCode(err, apierr.CodeFileMissing) {
		t.Fatalf("rawFileBody(missing) = %v, want file_missing", err)
	}
}
