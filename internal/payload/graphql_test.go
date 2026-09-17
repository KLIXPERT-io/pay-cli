package payload

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestGraphQLPostsToTheDerivedPath(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"data":{"Pages":{"totalDocs":11}}}`))
	c, _ := newTestClient(t, s, nil)
	res, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "{Pages(limit:1){totalDocs}}"})
	if err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if string(res.Data) != `{"Pages":{"totalDocs":11}}` {
		t.Fatalf("data = %s", res.Data)
	}
	seen := s.last()
	// The endpoint is routes.api + routes.graphQL, not an independent path.
	if seen.Path != "/api/graphql" || seen.Method != http.MethodPost {
		t.Fatalf("%s %s", seen.Method, seen.Path)
	}
	if got := seen.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
		t.Fatalf("body = %s", seen.Body)
	}
	if body["query"] != "{Pages(limit:1){totalDocs}}" {
		t.Fatalf("body = %v", body)
	}
}

func TestGraphQLPathIsConfigurable(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"data":{}}`))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.APIPath = "/cms-api"; cfg.GraphQLPath = "/cms-api/gql" })
	if _, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "{x}"}); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if s.last().Path != "/cms-api/gql" {
		t.Fatalf("path = %s", s.last().Path)
	}
}

func TestGraphQLEmptyQuery(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.GraphQL(context.Background(), GraphQLRequest{}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("err = %v, want invalid_args", err)
	}
	if s.count() != 0 {
		t.Fatal("an empty query must not reach the network")
	}
}

func TestGraphQL404IsGraphQLDisabled(t *testing.T) {
	s := newStub(t, jsonHandler(404, `{"message":"Route not found \"/api/graphql\""}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "{x}"})
	if !apierr.HasCode(err, apierr.CodeGraphQLDisabled) {
		t.Fatalf("err = %v, want graphql_disabled", err)
	}
	if apierr.ExitCode(err) != 10 {
		t.Fatalf("exit = %d, want 10", apierr.ExitCode(err))
	}
}

func TestGraphQLHTMLIsGraphQLDisabled(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>nope</html>")
	})
	c, _ := newTestClient(t, s, nil)
	if _, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "{x}"}); !apierr.HasCode(err, apierr.CodeGraphQLDisabled) {
		t.Fatalf("err = %v, want graphql_disabled", err)
	}
}

func TestIsReadOnlyGraphQL(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"anonymous query", "{Pages{totalDocs}}", true},
		{"named query", "query Q { Pages { totalDocs } }", true},
		{"introspection", `{q: __schema { queryType { fields { name } } }}`, true},
		{"mutation", "mutation { createPage(data:{title:\"x\"}) { id } }", false},
		{"named mutation", "mutation M($d: mutationPageInput!) { createPage(data:$d){id} }", false},
		{"subscription", "subscription S { pageUpdated { id } }", false},
		{"mutation after a query", "query Q{a}\nmutation M{b}", false},
		{"the word mutation inside a string", `{Pages(where:{title:{equals:"mutation"}}){id}}`, true},
		{"the word mutation in a comment", "# mutation of the schema\n{Pages{id}}", true},
		{"a field named mutationInput", "{mutationPageInput{id}}", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReadOnlyGraphQL(tc.doc); got != tc.want {
				t.Fatalf("IsReadOnlyGraphQL = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadOnlyGraphQLIsRetriedAndMutationIsNot(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
	})
	c, _ := newTestClient(t, s, nil)
	if _, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "{Pages{id}}"}); err != nil {
		t.Fatalf("a read-only GraphQL POST must be retried: %v", err)
	}
	if s.count() != 2 {
		t.Fatalf("made %d requests", s.count())
	}

	before := s.count()
	_, err := c.GraphQL(context.Background(), GraphQLRequest{Query: "mutation { createPage { id } }"})
	if err == nil {
		t.Fatal("want an error")
	}
	if s.count()-before != 1 {
		t.Fatalf("a mutation was retried %d times", s.count()-before-1)
	}
}

func TestGraphQLPartialAndErrors(t *testing.T) {
	partial := &GraphQLResult{
		Data:   json.RawMessage(`{"Pages":{"totalDocs":1}}`),
		Errors: []GraphQLError{{Message: "field x failed"}},
	}
	if !partial.Partial() {
		t.Fatal("data plus errors is a partial result")
	}
	if err := partial.AsError(); err != nil {
		t.Fatalf("a partial result must be usable: %v", err)
	}

	failed := &GraphQLResult{Data: json.RawMessage("null"), Errors: []GraphQLError{{Message: "boom"}}}
	if failed.Partial() {
		t.Fatal("null data is not a partial result")
	}
	if err := failed.AsError(); err == nil {
		t.Fatal("want an error")
	}
	ok := &GraphQLResult{Data: json.RawMessage(`{"a":1}`)}
	if err := ok.AsError(); err != nil {
		t.Fatalf("AsError = %v", err)
	}
}

func TestIntrospectionBlocked(t *testing.T) {
	req := GraphQLRequest{Query: `{__type(name:"Page"){fields{name}}}`}
	blocked := &GraphQLResult{Data: json.RawMessage("null"), Errors: []GraphQLError{
		{Message: "GraphQL introspection is not allowed, but the query contained __type"},
	}}
	if !IntrospectionBlocked(req, blocked) {
		t.Fatal("want blocked")
	}
	// Structural, not string-based: a non-introspection query is never
	// reported as blocked, and data present means it resolved.
	if IntrospectionBlocked(GraphQLRequest{Query: "{Pages{id}}"}, blocked) {
		t.Fatal("a non-introspection query cannot be introspection-blocked")
	}
	if IntrospectionBlocked(req, &GraphQLResult{Data: json.RawMessage(`{"__type":{}}`)}) {
		t.Fatal("a resolved introspection query is not blocked")
	}
	if IntrospectionBlocked(req, nil) {
		t.Fatal("nil result")
	}
}

func TestGraphQLErrorMessages(t *testing.T) {
	got := GraphQLErrorMessages([]GraphQLError{{Message: "a"}, {}, {Message: "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("messages = %v", got)
	}
}
