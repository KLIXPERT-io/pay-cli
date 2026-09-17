package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// introspectionRe is §7.6's matcher. It matches /introspection/i and NEVER the
// full English sentence: the message text is a @payloadcms/graphql
// implementation detail, and the rest of §7 forbids classifying on prose.
var introspectionRe = regexp.MustCompile(`(?i)introspection`)

// complexityRe is §7.4's adaptive-batching trigger.
var complexityRe = regexp.MustCompile(`(?i)complexity|too complex|maximum`)

// maxGraphQLBody is §7.4's 8 MB ceiling: a response over it halves the batch.
const maxGraphQLBody = 8 << 20

// gqlResult is one raw GraphQL exchange. The client's own GraphQL helper folds
// a 404 into an error, but §7.6's ladder has to tell an empty 404 (GraphQL
// disabled) from an HTML 404 (wrong path) from a JSON 404 (Payload's catch-all
// route), so discovery drives the transport directly.
type gqlResult struct {
	Status      int
	ContentType string
	Body        []byte
	Data        map[string]json.RawMessage
	Errors      []payload.GraphQLError
	DataPresent bool
	Bytes       int64
}

// alias returns a decoded alias from the response data.
func (r *gqlResult) alias(name string) (json.RawMessage, bool) {
	if r == nil || r.Data == nil {
		return nil, false
	}
	v, ok := r.Data[name]
	if !ok {
		return nil, false
	}
	return v, true
}

// aliasNull reports an alias that is present but null — §7.6's universal
// degrade trigger, not a failure.
func (r *gqlResult) aliasNull(name string) bool {
	v, ok := r.alias(name)
	if !ok {
		return true
	}
	return isJSONNull(v)
}

func isJSONNull(v json.RawMessage) bool {
	return len(bytes.TrimSpace(v)) == 0 || string(bytes.TrimSpace(v)) == "null"
}

// errorMessages flattens the GraphQL errors for a limitations[] detail.
func (r *gqlResult) errorMessages() []string {
	if r == nil {
		return nil
	}
	return payload.GraphQLErrorMessages(r.Errors)
}

// postGraphQL issues one GraphQL query against the resolved graphql_path.
//
// The request context is never marked for §8.4 Level-3 reactive
// invalidation: §8.4(a) forbids this package from arming it, because discovery
// deliberately generates every trigger (arch-lint greps for the opt-in helper
// by name anywhere under this directory).
func (d *Discoverer) postGraphQL(ctx context.Context, query string, noAuth bool) (*gqlResult, error) {
	body, err := json.Marshal(payload.GraphQLRequest{Query: query})
	if err != nil {
		return nil, err
	}
	req := &payload.Request{
		Method:      http.MethodPost,
		Path:        d.graphQLPath,
		Absolute:    true,
		Body:        body,
		ContentType: "application/json",
		// A GraphQL query is idempotency-safe under §6.1, so it may be
		// retried; introspection is never a mutation.
		ReadOnly:   true,
		NoOverride: true,
		NoAuth:     noAuth,
		// See stage0.do: the client's configured slug may still be the
		// unresolved placeholder.
		AuthCollectionOverride: d.authCollection,
	}
	resp, reqErr := d.client.Do(ctx, req)
	d.countRequest(resp)
	if resp == nil {
		return nil, reqErr
	}
	out := &gqlResult{
		Status:      resp.Status,
		ContentType: resp.ContentType,
		Body:        resp.Body,
		Bytes:       resp.Bytes,
	}
	if !isJSONContentType(resp.ContentType) {
		return out, reqErr
	}
	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []payload.GraphQLError     `json:"errors"`
	}
	if json.Unmarshal(resp.Body, &envelope) == nil {
		out.Data = envelope.Data
		out.Errors = envelope.Errors
		out.DataPresent = envelope.Data != nil
	}
	return out, reqErr
}

func isJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "application/json" || strings.HasSuffix(ct, "+json") || ct == "application/graphql-response+json"
}

// modeOutcome is §7.6's ladder result.
type modeOutcome struct {
	Mode          string
	Introspection *bool
	Detail        string
	Hint          string
}

// classifyGraphQL implements §7.6's table.
//
// The classification runs against the Stage-1 query itself rather than a
// separate probe, because §7.6's probe requirement — "must contain a field
// literally named __type" — is already satisfied by Stage 1's
// `acc: __type(name:"Access")`. Payload's introspection guard is a validation
// rule that fires only on a query AST containing __schema or __type, so a
// {__typename} probe returns a happy 200 against the exact deployment the
// guard targets. Folding the probe into Stage 1 keeps §7.1's cold budget and
// removes the possibility of the two disagreeing.
//
// expect are the aliases the query asked for; a null one is a degrade trigger.
func classifyGraphQL(res *gqlResult, err error, expect []string) modeOutcome {
	if res == nil {
		return modeOutcome{Mode: GraphQLModeUnreachable, Detail: errDetail(err)}
	}
	switch {
	case res.Status == http.StatusNotFound:
		if len(bytes.TrimSpace(res.Body)) == 0 {
			// graphQL: { disable: true } removes the route and the framework
			// answers an empty 404.
			return modeOutcome{Mode: GraphQLModeDisabled, Introspection: boolPtr(false),
				Detail: "the GraphQL route answered 404 with an empty body, which is what graphQL.disable: true produces"}
		}
		// Payload's catch-all answers a JSON 404 `Route not found "…"`, and a
		// framework-level miss answers HTML. Both mean "not at this path".
		return modeOutcome{Mode: GraphQLModeRouteMissing, Introspection: boolPtr(false),
			Detail: excerptBody(res.Body)}
	case res.Status >= 500:
		return modeOutcome{Mode: GraphQLModeUnreachable,
			Detail: fmt.Sprintf("HTTP %d from the GraphQL endpoint", res.Status)}
	case res.Status != http.StatusOK:
		return modeOutcome{Mode: GraphQLModeUnreachable,
			Detail: fmt.Sprintf("HTTP %d from the GraphQL endpoint", res.Status)}
	case !isJSONContentType(res.ContentType):
		return modeOutcome{Mode: GraphQLModeUnreachable,
			Detail: "the GraphQL endpoint answered 200 with a non-JSON body"}
	}

	missing := []string{}
	for _, a := range expect {
		if res.aliasNull(a) {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 && len(res.Errors) == 0 {
		return modeOutcome{Mode: GraphQLModeOK, Introspection: boolPtr(true)}
	}
	msgs := strings.Join(res.errorMessages(), "; ")
	if len(res.Errors) > 0 && introspectionRe.MatchString(msgs) {
		return modeOutcome{Mode: GraphQLModeIntrospectionDisabled, Introspection: boolPtr(false),
			Detail: truncate(msgs, 240)}
	}
	if len(res.Errors) > 0 {
		return modeOutcome{Mode: GraphQLModeErrors, Introspection: boolPtr(false),
			Detail: truncate(firstOf(res.errorMessages()), 240)}
	}
	// 200, no errors, but an expected alias came back null. §7.6 treats that as
	// a degrade trigger identically to an errors[] entry.
	return modeOutcome{Mode: GraphQLModeErrors, Introspection: boolPtr(false),
		Detail: "the GraphQL endpoint returned null for " + strings.Join(missing, ", ")}
}

// RouteMissingHint is §7.6's path-specific hint. It fires only when the mode is
// route_missing, graphql_path was derived rather than set, and api_path is not
// the default — the exact situation in which a working GraphQL endpoint is
// being missed.
func RouteMissingHint(graphQLPath, apiPath, graphQLRoute, graphQLPathSource string) string {
	if graphQLPathSource == SourceConfigured || apiPath == "/api" {
		return ""
	}
	if graphQLRoute == "" {
		graphQLRoute = "/graphql"
	}
	return fmt.Sprintf(
		"GraphQL not found at %s (derived from api_path=%s + graphql_route=%s). If this project sets routes.graphQL, pass --graphql-path <path>.",
		graphQLPath, apiPath, graphQLRoute)
}

// IsComplexityError reports §7.4's halve-the-batch trigger.
func IsComplexityError(msgs []string) bool {
	for _, m := range msgs {
		if complexityRe.MatchString(m) {
			return true
		}
	}
	return false
}

func errDetail(err error) string {
	if err == nil {
		return "no response from the GraphQL endpoint"
	}
	return truncate(err.Error(), 240)
}

func firstOf(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func excerptBody(b []byte) string {
	return truncate(strings.TrimSpace(string(b)), 240)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// buildShardFromDocs is §7.6's REST-only field discovery.
//
// Payload returns every field at depth 0 including the nulls, so one sampled
// document's key set IS the field list. Sampling several documents and taking
// the union covers a field that happened to be null-typed in the first one.
//
// /api/access `fields` is never consulted: it is the boolean true for a
// privileged key and an object for a restricted one, so it carries no names
// (§1 conflict 12).
func buildShardFromDocs(generation, slug string, docs []map[string]any, blocks BlockSources) *Shard {
	shard := NewShard(generation, slug)
	names := map[string]bool{}
	order := []string{}
	for _, doc := range docs {
		for name := range doc {
			if !names[name] {
				names[name] = true
				order = append(order, name)
			}
		}
	}
	sort.Strings(order)

	for _, name := range order {
		f := NewField(name, name)
		var sample any
		for _, doc := range docs {
			if v, ok := doc[name]; ok && v != nil {
				sample = v
				break
			}
		}
		f.PayloadType, f.JSONType, f.HasMany, f.Polymorphic, f.RelationTo = observeKind(name, sample)
		if f.RelationTo != nil {
			// A polymorphic relationship carries its target slug inline at
			// depth 0, which is the one relationship fact REST hands over for
			// free.
			f.RelationToSource = SourceObserved
		}
		f.PayloadTypeConfidence = ConfidenceObserved
		if sample == nil {
			// A field that was null in every sample has a name and nothing
			// else. Saying "unknown" is the honest answer.
			f.PayloadType = TypeUnknown
			f.JSONType = "unknown"
			f.PayloadTypeConfidence = ConfidenceUnknown
		}
		f.ReadOnly = serverManaged(name)
		f.Queryable = true
		f.Operators = OperatorsFor(f.PayloadType, "")
		f.Sortable = SortableKind(f.PayloadType)
		f.SortableConfidence = ConfidenceHeuristic
		// Required-ness comes from mutation{Singular}Input and nothing else;
		// without GraphQL it is genuinely unknown, so it stays null rather
		// than being defaulted to false.
		f.Required = nil
		f.RequiredSource = SourceUnknown
		f.WriteShape = WriteShapeFor(f.PayloadType, f.Polymorphic, f.HasMany)
		if f.PayloadType == TypeBlocks {
			res := ResolveBlocks(f.Path, nil, blocks)
			if res.Slugs != nil {
				if shard.Blocks == nil {
					shard.Blocks = map[string][]string{}
				}
				shard.Blocks[f.Path] = res.Slugs
				shard.BlocksSource = res.Source
			}
		}
		shard.Fields = append(shard.Fields, f)
	}
	shard.Finalize()
	return shard
}

// observeKind types one sampled JSON value.
func observeKind(name string, v any) (payloadType, jsonType string, hasMany, polymorphic bool, relationTo []string) {
	if name == "id" {
		// The primary key is reported as `id` whichever JSON type carries it,
		// exactly as the GraphQL path does, so a consumer branches on id_type
		// rather than on the sample it happened to see.
		if _, ok := IDTypeOfJSON(v); ok {
			t, _ := IDTypeOfJSON(v)
			if t == IDTypeNumber {
				return TypeID, "number", false, false, nil
			}
			return TypeID, "string", false, false, nil
		}
	}
	switch t := v.(type) {
	case nil:
		return TypeUnknown, "unknown", false, false, nil
	case bool:
		return TypeCheckbox, "boolean", false, false, nil
	case string:
		if looksISODate(t) {
			return TypeDate, "string", false, false, nil
		}
		return TypeText, "string", false, false, nil
	case json.Number:
		return TypeNumber, "number", false, false, nil
	case float64:
		return TypeNumber, "number", false, false, nil
	case map[string]any:
		if rel, ok := t["relationTo"].(string); ok {
			if _, hasValue := t["value"]; hasValue {
				return TypeRelationship, "object", false, true, []string{rel}
			}
		}
		if looksRichText(name) || isLexicalRoot(t) {
			return TypeRichText, "object", false, false, nil
		}
		return TypeGroup, "object", false, false, nil
	case []any:
		if len(t) == 0 {
			return TypeUnknown, "array", true, false, nil
		}
		if first, ok := t[0].(map[string]any); ok {
			if rel, ok := first["relationTo"].(string); ok {
				return TypeRelationship, "array", true, true, []string{rel}
			}
			if _, hasBlockType := first["blockType"]; hasBlockType {
				return TypeBlocks, "array", true, false, nil
			}
			return TypeArray, "array", true, false, nil
		}
		return TypeArray, "array", true, false, nil
	default:
		return TypeUnknown, "unknown", false, false, nil
	}
}

// isLexicalRoot recognises a lexical richText value, which is an object with a
// `root` node.
func isLexicalRoot(m map[string]any) bool {
	root, ok := m["root"].(map[string]any)
	if !ok {
		return false
	}
	_, hasChildren := root["children"]
	return hasChildren
}

// isoDateRe is deliberately narrow: only a full ISO-8601 timestamp counts, so
// a slug like "2024-recap" is not typed as a date.
var isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)

func looksISODate(s string) bool { return isoDateRe.MatchString(s) }
