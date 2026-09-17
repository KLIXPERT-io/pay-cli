package discovery

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func gqlOK(data string, errs ...string) *gqlResult {
	r := &gqlResult{Status: http.StatusOK, ContentType: "application/json", Body: []byte(data)}
	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []payload.GraphQLError     `json:"errors"`
	}
	_ = json.Unmarshal([]byte(data), &envelope)
	r.Data = envelope.Data
	r.Errors = envelope.Errors
	r.DataPresent = envelope.Data != nil
	_ = errs
	return r
}

func TestClassifyGraphQLLadder(t *testing.T) {
	tests := []struct {
		name          string
		res           *gqlResult
		err           error
		wantMode      string
		wantIntrospec any
	}{
		{
			name:          "ok",
			res:           gqlOK(`{"data":{"__typename":"Query","q":{"queryType":{"fields":[]}},"acc":{"fields":[]}}}`),
			wantMode:      GraphQLModeOK,
			wantIntrospec: true,
		},
		{
			// Payload's guard is a validation rule that reports an error only
			// when the AST contains __schema or __type. Matching /introspection/i
			// and never the full sentence is deliberate: the text is a
			// @payloadcms/graphql implementation detail.
			name:          "introspection disabled",
			res:           gqlOK(`{"errors":[{"message":"GraphQL introspection is not allowed"}]}`),
			wantMode:      GraphQLModeIntrospectionDisabled,
			wantIntrospec: false,
		},
		{
			name:          "errors without an introspection match",
			res:           gqlOK(`{"errors":[{"message":"Query is too complex"}]}`),
			wantMode:      GraphQLModeErrors,
			wantIntrospec: false,
		},
		{
			name:          "200 with a null alias",
			res:           gqlOK(`{"data":{"q":null,"acc":{"fields":[]}}}`),
			wantMode:      GraphQLModeErrors,
			wantIntrospec: false,
		},
		{
			// graphQL: { disable: true } removes the route and the framework
			// answers an empty 404.
			name:          "disabled",
			res:           &gqlResult{Status: http.StatusNotFound, ContentType: "application/json", Body: nil},
			wantMode:      GraphQLModeDisabled,
			wantIntrospec: false,
		},
		{
			name: "payload catch-all 404",
			res: &gqlResult{Status: http.StatusNotFound, ContentType: "application/json",
				Body: []byte(`{"message":"Route not found \"/api/graphql\""}`)},
			wantMode:      GraphQLModeRouteMissing,
			wantIntrospec: false,
		},
		{
			name: "html 404",
			res: &gqlResult{Status: http.StatusNotFound, ContentType: "text/html; charset=utf-8",
				Body: []byte("<!DOCTYPE html>")},
			wantMode:      GraphQLModeRouteMissing,
			wantIntrospec: false,
		},
		{
			name:          "5xx",
			res:           &gqlResult{Status: 502, ContentType: "application/json", Body: []byte(`{}`)},
			wantMode:      GraphQLModeUnreachable,
			wantIntrospec: nil,
		},
		{
			// A wrong base URL answers 200 text/html (verified), so the
			// Content-Type check is load-bearing here too.
			name:          "200 html",
			res:           &gqlResult{Status: 200, ContentType: "text/html", Body: []byte("<html>")},
			wantMode:      GraphQLModeUnreachable,
			wantIntrospec: nil,
		},
		{
			name:          "no response at all",
			res:           nil,
			wantMode:      GraphQLModeUnreachable,
			wantIntrospec: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGraphQL(tt.res, tt.err, []string{"q", "acc"})
			if got.Mode != tt.wantMode {
				t.Fatalf("mode = %q, want %q (detail %q)", got.Mode, tt.wantMode, got.Detail)
			}
			switch want := tt.wantIntrospec.(type) {
			case nil:
				if got.Introspection != nil {
					t.Errorf("introspection = %v, want null", *got.Introspection)
				}
			case bool:
				if got.Introspection == nil || *got.Introspection != want {
					t.Errorf("introspection = %v, want %v", got.Introspection, want)
				}
			}
		})
	}
}

func TestRouteMissingHint(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		api    string
		route  string
		source string
		want   string
	}{
		{"derived non-default api path", "/cms-api/graphql", "/cms-api", "/graphql", SourceDerivedGraphQLPath,
			"GraphQL not found at /cms-api/graphql (derived from api_path=/cms-api + graphql_route=/graphql). If this project sets routes.graphQL, pass --graphql-path <path>."},
		{"default api path stays silent", "/api/graphql", "/api", "/graphql", SourceDerivedGraphQLPath, ""},
		{"explicitly configured stays silent", "/x/graphql", "/x", "/graphql", SourceConfigured, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RouteMissingHint(tt.path, tt.api, tt.route, tt.source); got != tt.want {
				t.Fatalf("hint = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestIsComplexityError(t *testing.T) {
	for _, msg := range []string{
		"Query is too complex: 1200. Maximum allowed complexity: 1000",
		"maximum call stack",
		"COMPLEXITY exceeded",
	} {
		if !IsComplexityError([]string{msg}) {
			t.Errorf("%q must halve the batch", msg)
		}
	}
	if IsComplexityError([]string{"You are not allowed to perform this action"}) {
		t.Error("an access error is not a complexity error")
	}
}

func TestBuildShardFromDocsTypesEveryKey(t *testing.T) {
	docs := []map[string]any{
		{
			"id":        json.Number("16"),
			"title":     "Home",
			"createdAt": "2026-09-16T17:00:00.000Z",
			"count":     json.Number("3"),
			"published": true,
			"content":   map[string]any{"root": map[string]any{"children": []any{}}},
			"meta":      map[string]any{"title": "x"},
			"ref":       map[string]any{"relationTo": "posts", "value": json.Number("4")},
			"layout":    []any{map[string]any{"blockType": "cta"}},
			"rows":      []any{map[string]any{"label": "a"}},
			"never":     nil,
		},
		{"onlyHere": "x"},
	}
	shard := buildShardFromDocs("gen", "pages", docs, BlockSources{})
	byPath := map[string]Field{}
	for _, f := range shard.Fields {
		byPath[f.Path] = f
	}
	tests := map[string]struct {
		payloadType string
		poly        bool
		relTo       []string
	}{
		"id":        {TypeID, false, nil},
		"title":     {TypeText, false, nil},
		"createdAt": {TypeDate, false, nil},
		"count":     {TypeNumber, false, nil},
		"published": {TypeCheckbox, false, nil},
		"content":   {TypeRichText, false, nil},
		"meta":      {TypeGroup, false, nil},
		"ref":       {TypeRelationship, true, []string{"posts"}},
		"layout":    {TypeBlocks, false, nil},
		"rows":      {TypeArray, false, nil},
		// A field that was null in every sample has a name and nothing else.
		"never":    {TypeUnknown, false, nil},
		"onlyHere": {TypeText, false, nil},
	}
	for path, want := range tests {
		got, ok := byPath[path]
		if !ok {
			t.Errorf("missing field %q", path)
			continue
		}
		if got.PayloadType != want.payloadType {
			t.Errorf("%s payload_type = %q, want %q", path, got.PayloadType, want.payloadType)
		}
		if got.Polymorphic != want.poly {
			t.Errorf("%s polymorphic = %v", path, got.Polymorphic)
		}
		if want.relTo != nil && !reflect.DeepEqual(got.RelationTo, want.relTo) {
			t.Errorf("%s relation_to = %v, want %v", path, got.RelationTo, want.relTo)
		}
		// Required-ness comes from the mutation input type and nothing else,
		// so without GraphQL it must stay null rather than default to false.
		if got.Required != nil {
			t.Errorf("%s required = %v; REST cannot establish required-ness", path, *got.Required)
		}
		if got.RequiredSource != SourceUnknown {
			t.Errorf("%s required_source = %q", path, got.RequiredSource)
		}
	}
	if !byPath["id"].ReadOnly {
		t.Error("id must be read-only")
	}
}

func TestIDStringKeepsExactDigits(t *testing.T) {
	// A 19-digit id must not round-trip through float64.
	big := json.Number("9007199254740993")
	if got := IDString(big); got != "9007199254740993" {
		t.Fatalf("IDString = %q", got)
	}
}

func TestIsJSONContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"application/graphql-response+json": true,
		"text/html; charset=utf-8":          false,
		"":                                  false,
		"application/octet-stream":          false,
	} {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestTruncateCollapsesNewlines(t *testing.T) {
	got := truncate("a\nb", 10)
	if strings.Contains(got, "\n") {
		t.Fatalf("truncate kept a newline: %q", got)
	}
}
