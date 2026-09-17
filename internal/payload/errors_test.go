package payload

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func TestClassifyWiring(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		classify ClassifyContext
		authMode string
		verified bool
		want     apierr.Code
		wantExit int
	}{
		{
			name: "validation", status: 400,
			body: `{"errors":[{"name":"ValidationError","data":{"collection":"pages","errors":[` +
				`{"label":"Title","message":"This field is required.","path":"title"}]},` +
				`"message":"The following fields are invalid: Title"}]}`,
			want: apierr.CodeValidationFailed, wantExit: 5,
		},
		{
			name: "german validation body still classifies", status: 400,
			body: `{"errors":[{"name":"ValidationError","data":{"collection":"pages","errors":[` +
				`{"label":"Titel","message":"Dieses Feld ist erforderlich.","path":"title"}]},` +
				`"message":"Die folgenden Felder sind ungültig: Titel"}]}`,
			want: apierr.CodeValidationFailed, wantExit: 5,
		},
		{
			name: "query error", status: 400,
			body: `{"errors":[{"name":"QueryError","data":[{"path":"nosuch"}],"message":"The following path cannot be queried: nosuch"}]}`,
			want: apierr.CodeQueryPathInvalid, wantExit: 5,
		},
		{
			name: "doc not found", status: 404,
			body: `{"errors":[{"message":"Not Found"}]}`,
			want: apierr.CodeDocNotFound, wantExit: 4,
		},
		{
			name: "route not found", status: 404,
			body: `{"message":"Route not found \"/api/nope\""}`,
			want: apierr.CodeRouteNotFound, wantExit: 4,
		},
		{
			name: "endpoints disabled", status: 501,
			body: `{"message":"Cannot GET http://localhost:3900/api/payload-migrations"}`,
			want: apierr.CodeOperationUnsupported, wantExit: 10,
		},
		{
			name: "masked 500", status: 500,
			body: `{"errors":[{"message":"Something went wrong."}]}`,
			want: apierr.CodeServerError, wantExit: 6,
		},
		{
			name: "403 with a verified identity is access denied", status: 403,
			body:     `{"errors":[{"message":"You are not allowed to perform this action."}]}`,
			authMode: AuthModeAPIKey, verified: true,
			want: apierr.CodeAccessDenied, wantExit: 8,
		},
		{
			name: "403 while anonymous is auth required", status: 403,
			body:     `{"errors":[{"message":"You are not allowed to perform this action."}]}`,
			authMode: AuthModeAnonymous,
			want:     apierr.CodeAuthRequired, wantExit: 2,
		},
		{
			name: "400 on an upload collection is a missing file", status: 400,
			body:     `{"errors":[{"message":"No files were uploaded."}]}`,
			classify: ClassifyContext{Collection: "media", UploadCollection: true},
			want:     apierr.CodeFileMissing, wantExit: 5,
		},
		{
			name: "missing where", status: 400,
			body: `{"errors":[{"message":"Missing 'where' query of documents to delete."}]}`,
			want: apierr.CodeWhereRequired, wantExit: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(tc.status, tc.body))
			c, _ := newTestClient(t, s, func(cfg *Config) {
				if tc.authMode != "" {
					cfg.AuthMode = tc.authMode
					if tc.authMode == AuthModeAnonymous {
						cfg.Credential = ""
						cfg.AuthCollection = ""
					}
				}
				cfg.IdentityVerified = tc.verified
			})
			_, err := c.Do(context.Background(), &Request{
				Method: http.MethodGet, Path: "/pages", Classify: tc.classify,
			})
			if !apierr.HasCode(err, tc.want) {
				t.Fatalf("code = %s, want %s (%v)", apierr.CodeOf(err), tc.want, err)
			}
			if got := apierr.ExitCode(err); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d", got, tc.wantExit)
			}
		})
	}
}

func TestErrorCarriesHTTPContext(t *testing.T) {
	s := newStub(t, jsonHandler(404, `{"errors":[{"message":"Not Found"}]}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Path: "/pages/999"})
	e, ok := apierr.As(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if e.HTTP == nil || e.HTTP.Status != 404 || e.HTTP.Method != http.MethodGet {
		t.Fatalf("http context = %+v", e.HTTP)
	}
	if !strings.Contains(e.HTTP.URL, "/api/pages/999") {
		t.Fatalf("url = %s", e.HTTP.URL)
	}
}

func TestSentPaths(t *testing.T) {
	got := SentPaths(map[string]any{
		"title": "x",
		"hero":  map[string]any{"media": 4, "type": "lowImpact"},
		"tags":  []any{map[string]any{"name": "a"}},
	})
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"hero", "hero.media", "hero.type", "tags", "tags.name", "title"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("paths = %v, want %v", keys, want)
	}
}

func TestSentPathsDrivesTheValidationHint(t *testing.T) {
	// Payload re-validates the WHOLE document on PATCH, so an error can name
	// fields the caller never sent. The classifier needs SentPaths to tell the
	// two apart.
	body := `{"errors":[{"name":"ValidationError","data":{"collection":"posts","errors":[` +
		`{"label":"Title","message":"This field is required.","path":"title"},` +
		`{"label":"Hero Image","message":"invalid","path":"heroImage"}]},"message":"x"}]}`
	s := newStub(t, jsonHandler(400, body))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Update(context.Background(), "posts", "1", map[string]any{"heroImage": 4}, query.Params{})
	e, ok := apierr.As(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if len(e.Fields) != 2 {
		t.Fatalf("fields = %+v", e.Fields)
	}
	// Sent fields sort first.
	if e.Fields[0].Path != "heroImage" || !e.Fields[0].Sent {
		t.Fatalf("expected the sent field first: %+v", e.Fields)
	}
	if e.Fields[1].Path != "title" || e.Fields[1].Sent {
		t.Fatalf("expected the unsent field second: %+v", e.Fields)
	}
}

func TestRequireJSON(t *testing.T) {
	tests := []struct {
		name string
		resp Response
		ok   bool
	}{
		{"application/json", Response{ContentType: "application/json", Body: []byte("{}")}, true},
		{"with charset", Response{ContentType: "application/json; charset=utf-8", Body: []byte("{}")}, true},
		{"vendor json", Response{ContentType: "application/vnd.api+json", Body: []byte("{}")}, true},
		{"html", Response{ContentType: "text/html", Body: []byte("<html>")}, false},
		{"missing but obviously json", Response{Body: []byte("  {\"a\":1}")}, true},
		{"missing and not json", Response{Body: []byte("oops")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.resp
			err := requireJSON(&r)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, wantOK=%v", err, tc.ok)
			}
			if err != nil && !apierr.HasCode(err, apierr.CodeNonJSONResponse) {
				t.Fatalf("err = %v, want non_json_response", err)
			}
		})
	}
}

func TestDecodeJSONKeepsNumbersExact(t *testing.T) {
	resp := &Response{ContentType: "application/json", Body: []byte(`{"id":9007199254740993}`)}
	var doc Doc
	if err := decodeJSON(resp, &doc); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if got := idToString(doc.ID()); got != "9007199254740993" {
		t.Fatalf("id = %s: a large id must not round-trip through float64", got)
	}
}

func TestJSONBodyDoesNotEscapeHTML(t *testing.T) {
	b, err := jsonBody(map[string]any{"title": "a & b <c>"})
	if err != nil {
		t.Fatalf("jsonBody: %v", err)
	}
	if string(b) != `{"title":"a & b <c>"}` {
		t.Fatalf("body = %s", b)
	}
}
