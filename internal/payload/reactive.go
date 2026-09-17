package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// §8.4 Level-3 reactive invalidation.
//
// Level 1 (the /api/access topology hash) and Level 2 (the GraphQL Query field
// list) both miss "a field was added to an existing collection". A 400
// QueryError, a 404 `Route not found` for a route the manifest claims exists,
// or a 501 `Cannot <METHOD>` is proof the cached schema is stale.
//
// The mechanism is opt-in per request and never ambient, because discovery
// deliberately generates every one of those triggers: 47 of 49 live
// collections answer the upload probe with `Route not found`, the drafts probe
// is a 400 QueryError by design, and the endpoints probe exists to catch a 501.
// An ambient rule would turn one `pay discover` into unbounded nested
// re-discovery.

type reactiveKey struct{}

// WithReactiveInvalidation marks a context as eligible for Level-3
// invalidation. It is called ONLY by user-facing command handlers in
// internal/cli; arch-lint fails the build if it appears anywhere under
// internal/discovery.
func WithReactiveInvalidation(ctx context.Context) context.Context {
	return context.WithValue(ctx, reactiveKey{}, true)
}

// ReactiveRequested reports whether the context opted in.
func ReactiveRequested(ctx context.Context) bool {
	v, _ := ctx.Value(reactiveKey{}).(bool)
	return v
}

// Staleness signal kinds.
const (
	StaleQueryPath    = "query_path"
	StaleRouteMissing = "route_missing"
	StaleEndpoints    = "endpoints_disabled"
)

// StaleSignal describes the proof that the cached schema is out of date.
type StaleSignal struct {
	// Kind is one of the Stale* constants.
	Kind string
	// Paths are the field paths a QueryError rejected.
	Paths []string
	// Method and Route describe the request that produced the signal.
	Method string
	Route  string
	// Idempotent reports whether the original request may be re-sent after a
	// successful re-discovery (§8.4c).
	Idempotent bool
}

// Outcome is what an Invalidator did.
type Outcome struct {
	// Revalidated reports that discovery re-ran and the manifest is fresh.
	Revalidated bool
	// Fields is the refreshed field list, carried into the schema_stale error
	// when the request cannot be re-sent.
	Fields []string
}

// Invalidator is implemented by the cache/discovery layer. The client never
// imports it concretely, which keeps internal/payload free of a dependency on
// internal/discovery.
type Invalidator interface {
	Invalidate(ctx context.Context, sig StaleSignal) (Outcome, error)
}

// staleSignal detects a Level-3 trigger, honouring the opt-in and the
// once-per-process guard.
func (c *Client) staleSignal(ctx context.Context, req *Request, resp *Response) *StaleSignal {
	if c.cfg.Invalidator == nil || !ReactiveRequested(ctx) || resp == nil || resp.OK() {
		return nil
	}
	sig := DetectStale(resp, req.KnownRoute)
	if sig == nil {
		return nil
	}
	sig.Method = resp.Method
	sig.Route = resp.URL
	sig.Idempotent = isReadOnly(req) || resp.Promoted
	return sig
}

// DetectStale classifies a response as a staleness proof. knownRoute says the
// manifest claims the route exists, which is what makes a 404 meaningful — a
// 404 for a slug PayCLI never heard of is just a typo.
func DetectStale(resp *Response, knownRoute bool) *StaleSignal {
	if resp == nil || resp.OK() {
		return nil
	}
	switch resp.Status {
	case http.StatusBadRequest:
		if paths, ok := queryErrorPaths(resp.Body); ok {
			return &StaleSignal{Kind: StaleQueryPath, Paths: paths}
		}
	case http.StatusNotFound:
		// "Route not found" is one of the two hardcoded-English Payload
		// strings (§11.2), named explicitly here for that reason.
		if knownRoute && bodyMessageHasPrefix(resp.Body, "Route not found") {
			return &StaleSignal{Kind: StaleRouteMissing}
		}
	case http.StatusNotImplemented:
		if bodyMessageHasPrefix(resp.Body, "Cannot ") {
			return &StaleSignal{Kind: StaleEndpoints}
		}
	}
	return nil
}

// handleStale implements §8.4(b) and (c).
func (c *Client) handleStale(ctx context.Context, req *Request, plan *requestPlan, resp *Response, sig StaleSignal) (*Response, error) {
	if c.state.staleUsed.Swap(true) {
		// A second occurrence in the same process is not a stale cache: it is
		// a genuinely invalid path. No second re-discovery, no ping-pong.
		return resp, c.schemaStale(req, resp, sig, nil)
	}
	outcome, err := c.cfg.Invalidator.Invalidate(ctx, sig)
	if err != nil {
		return resp, apierr.Wrap(err, apierr.CodeDiscoveryFailed,
			"the cached schema looked stale, but re-discovery failed")
	}
	if !outcome.Revalidated {
		return resp, c.classify(req, resp)
	}
	if !sig.Idempotent {
		// §6.1: once any response byte is read a write is never re-sent. The
		// original request may already have committed.
		return resp, c.schemaStale(req, resp, sig, outcome.Fields)
	}
	retryResp, retryErr := c.send(ctx, req, plan)
	if retryErr != nil {
		return retryResp, retryErr
	}
	if !retryResp.OK() {
		return retryResp, c.classify(req, retryResp)
	}
	return retryResp, nil
}

func (c *Client) schemaStale(req *Request, resp *Response, sig StaleSignal, fields []string) *apierr.Error {
	e := apierr.New(apierr.CodeSchemaStale,
		"the server rejected %s as unknown, and PayCLI's cached schema is already the refreshed one",
		describeSignal(sig)).
		WithHint("%s", "run `pay discover --refresh` and re-check the field name with `pay describe "+
			orCollection(req.Classify.Collection)+"`")
	if len(sig.Paths) > 0 {
		e = e.WithDidYouMean(apierr.DidYouMean(sig.Paths[0], fields)...)
	}
	if resp != nil {
		e = e.WithHTTP(&apierr.HTTP{
			Status: resp.Status, Method: resp.Method, URL: resp.URL,
			Attempts: resp.Attempts, BodyExcerpt: excerpt(resp.Body),
		})
	}
	return e
}

func describeSignal(sig StaleSignal) string {
	switch sig.Kind {
	case StaleQueryPath:
		if len(sig.Paths) > 0 {
			return "the path " + strings.Join(sig.Paths, ", ")
		}
		return "the query path"
	case StaleRouteMissing:
		return "the route"
	default:
		return "the operation"
	}
}

func orCollection(slug string) string {
	if slug == "" {
		return "<collection>"
	}
	return slug
}

// queryErrorPaths extracts the paths of a 400 QueryError. The `name` is the
// signal; the message is translated and is never matched against. Note that
// `data` is an ARRAY here and an object on a ValidationError — code that does
// errors[0].data.errors on a QueryError crashes (§11.2).
func queryErrorPaths(body []byte) ([]string, bool) {
	var envelope struct {
		Errors []struct {
			Name string           `json:"name"`
			Data []map[string]any `json:"data"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Errors) == 0 {
		return nil, false
	}
	if envelope.Errors[0].Name != "QueryError" {
		return nil, false
	}
	paths := make([]string, 0, len(envelope.Errors[0].Data))
	for _, entry := range envelope.Errors[0].Data {
		if p, ok := entry["path"].(string); ok {
			paths = append(paths, p)
		}
	}
	return paths, true
}

func bodyMessageHasPrefix(body []byte, prefix string) bool {
	var envelope struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	if strings.HasPrefix(envelope.Message, prefix) {
		return true
	}
	for _, e := range envelope.Errors {
		if strings.HasPrefix(e.Message, prefix) {
			return true
		}
	}
	return false
}
