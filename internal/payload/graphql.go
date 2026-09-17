package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// GraphQLRequest is one GraphQL operation.
//
// In development Payload rebuilds its entire schema on every /api/graphql
// request, so every call costs ~0.39 s regardless of query size (a 30-alias
// batch cost the same as a single type lookup). Batch aggressively; never loop.
type GraphQLRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
	OperationName string         `json:"operationName,omitempty"`
}

// GraphQLError is one entry of the GraphQL errors array.
type GraphQLError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path,omitempty"`
	Locations  []any          `json:"locations,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// GraphQLResult is a GraphQL response. GraphQL answers HTTP 200 for a
// partially failed query, so Errors must be inspected even when err is nil.
type GraphQLResult struct {
	Data   json.RawMessage `json:"data"`
	Errors []GraphQLError  `json:"errors,omitempty"`

	Raw  []byte    `json:"-"`
	HTTP *Response `json:"-"`
}

// Partial reports data alongside errors — the shape §7 tolerates by using
// whatever resolved and degrading the rest.
func (g *GraphQLResult) Partial() bool {
	return len(g.Errors) > 0 && len(g.Data) > 0 && string(g.Data) != "null"
}

// GraphQLPath is the resolved endpoint path, derived from api_path rather than
// configured independently: Payload computes it as routes.api + routes.graphQL.
func (c *Client) GraphQLPath() string { return c.routes().graphQL }

// GraphQL posts an operation to the GraphQL endpoint.
func (c *Client) GraphQL(ctx context.Context, in GraphQLRequest, opts ...Option) (*GraphQLResult, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "the GraphQL query is empty")
	}
	body, err := jsonBody(in)
	if err != nil {
		return nil, err
	}
	readOnly := IsReadOnlyGraphQL(in.Query)
	req := applyOptions(&Request{
		Method:      http.MethodPost,
		Path:        c.GraphQLPath(),
		Absolute:    true,
		Body:        body,
		ContentType: "application/json",
		// §6.1: a read-only GraphQL POST is idempotency-safe and may be
		// retried; a mutation never is.
		ReadOnly:   readOnly,
		NoOverride: true,
	}, opts)

	resp, err := c.Do(ctx, req)
	if resp != nil && resp.Status == http.StatusNotFound {
		return nil, c.graphQLDisabled(resp)
	}
	if err != nil {
		return nil, err
	}
	if !isJSONResponse(resp) {
		// An HTML answer here means the path is not the GraphQL route.
		return nil, c.graphQLDisabled(resp)
	}
	out := &GraphQLResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) graphQLDisabled(resp *Response) *apierr.Error {
	e := apierr.New(apierr.CodeGraphQLDisabled,
		"no GraphQL endpoint answered at %s", c.GraphQLPath()).
		WithHint("the endpoint is routes.api + routes.graphQL; pass --graphql-path, or accept the " +
			"REST-only degraded discovery (`pay discover` continues without it)")
	if resp != nil {
		e = e.WithHTTP(&apierr.HTTP{
			Status: resp.Status, Method: resp.Method, URL: resp.URL,
			BodyExcerpt: excerpt(resp.Body),
		})
	}
	return e
}

var (
	// gqlComment strips # comments; gqlString strips string literals, so the
	// keyword scan below cannot be fooled by the word "mutation" inside one.
	gqlComment = regexp.MustCompile(`#[^\n]*`)
	gqlString  = regexp.MustCompile(`"""[\s\S]*?"""|"(?:[^"\\]|\\.)*"`)
	gqlWrite   = regexp.MustCompile(`(^|[\s})])(mutation|subscription)\s*[\s({@a-zA-Z_]`)
)

// IsReadOnlyGraphQL reports whether a document contains only queries, which is
// §6.1's condition for retrying a GraphQL POST. It is deliberately
// conservative: anything it cannot prove is read-only is treated as a write.
func IsReadOnlyGraphQL(doc string) bool {
	stripped := gqlString.ReplaceAllString(doc, `""`)
	stripped = gqlComment.ReplaceAllString(stripped, "")
	return !gqlWrite.MatchString(stripped)
}

// IntrospectionBlocked reports Payload's NoProductionIntrospection guard.
//
// The guard fires only on a field literally named __schema or __type, and only
// under NODE_ENV=production, so a {__typename} probe can never detect it. The
// signal used here is structural — the request asked for introspection and the
// response carries errors with no data — with the English message kept only as
// a confirming hint.
func IntrospectionBlocked(in GraphQLRequest, res *GraphQLResult) bool {
	if res == nil || len(res.Errors) == 0 {
		return false
	}
	if len(res.Data) > 0 && string(res.Data) != "null" {
		return false
	}
	if !strings.Contains(in.Query, "__schema") && !strings.Contains(in.Query, "__type") {
		return false
	}
	return true
}

// GraphQLErrorMessages flattens the error list for an error message.
func GraphQLErrorMessages(errs []GraphQLError) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		if e.Message != "" {
			out = append(out, e.Message)
		}
	}
	return out
}

// AsError folds GraphQL errors into one *apierr.Error. A result with both data
// and errors is a partial success and yields nil, so the caller can use what
// resolved (§7's degradation ladder).
func (g *GraphQLResult) AsError() error {
	if g == nil || len(g.Errors) == 0 || g.Partial() {
		return nil
	}
	msgs := GraphQLErrorMessages(g.Errors)
	e := apierr.New(apierr.CodeDiscoveryFailed, "the GraphQL query failed: %s", strings.Join(msgs, "; "))
	if g.HTTP != nil {
		e = e.WithHTTP(&apierr.HTTP{
			Status: g.HTTP.Status, Method: g.HTTP.Method, URL: g.HTTP.URL,
			BodyExcerpt: excerpt(g.Raw),
		})
	}
	return e
}
