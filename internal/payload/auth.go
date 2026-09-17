package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Identity is the result of GET /{authCollection}/me (§7.2 Stage 0).
//
// The user object contains the API key IN PLAINTEXT on a project with
// useAPIKey (verified), so it is never logged and every consumer must render
// it through internal/redact.
type Identity struct {
	// User is nil when the credential did not authenticate. Note that a WRONG
	// API key returns HTTP 200 with {"user":null} — status codes are useless
	// for auth here, which is why this field, not the status, is the signal.
	User Doc
	// Collection is the auth collection the identity was resolved against.
	Collection string
	// UserID is user.id.
	UserID any
	// Strategy is Payload's auth strategy name, when it reports one.
	Strategy string
	// CanAccessAdmin is true only when the key is present and true: Payload
	// omits the key entirely when the answer is false.
	CanAccessAdmin bool
	// Verified reports user != null.
	Verified bool
	// Token and Exp are set when the endpoint refreshed a JWT.
	Token string
	Exp   int64

	Raw  []byte
	HTTP *Response
}

// Me issues GET /{collection}/me. Pass the empty string to use the client's
// configured auth collection.
func (c *Client) Me(ctx context.Context, collection string, opts ...Option) (*Identity, error) {
	collection = resolvedAuthCollection(collection)
	if collection == "" {
		collection = c.AuthCollection()
	}
	if collection == "" {
		return nil, apierr.New(apierr.CodeAuthCollectionUnknown,
			"no auth collection is known, so PayCLI cannot ask the server who you are").
			WithHint("pass --auth-collection SLUG or let discovery's Stage -1 resolve it")
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(collection) + "/me",
	}, opts)
	req.Classify.Collection = collection

	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	var body struct {
		User    Doc         `json:"user"`
		Token   string      `json:"token"`
		Exp     json.Number `json:"exp"`
		Message string      `json:"message"`
	}
	if err := decodeJSON(resp, &body); err != nil {
		return nil, err
	}
	id := &Identity{
		User:       body.User,
		Collection: collection,
		Token:      body.Token,
		Verified:   body.User != nil,
		Raw:        resp.Body,
		HTTP:       resp,
	}
	if body.Exp != "" {
		if n, convErr := body.Exp.Int64(); convErr == nil {
			id.Exp = n
		}
	}
	if body.User != nil {
		id.UserID = body.User["id"]
		if s, ok := body.User["_strategy"].(string); ok {
			id.Strategy = s
		}
		// canAccessAdmin is absent when false, never literally false.
		if v, ok := body.User["canAccessAdmin"].(bool); ok {
			id.CanAccessAdmin = v
		}
	}
	return id, nil
}

// AssertIdentity turns an unverified identity into §7.2's auth_invalid.
func AssertIdentity(id *Identity) error {
	if id != nil && id.Verified {
		return nil
	}
	return apierr.New(apierr.CodeAuthInvalid,
		"the server accepted the request but reports no user for this credential").
		WithHint("a wrong API key answers HTTP 200 with user:null, and a wrong auth-collection slug " +
			"silently gives the anonymous view — re-check both with `pay auth login`")
}

// AccessResult is GET /{api}/access.
//
// It is NOT a complete inventory: a collection on which the identity has zero
// permissions is absent entirely (payload-kv is missing live even though
// GET /api/payload-kv answers 403). collections.{slug}.fields is the boolean
// `true` when every field permission is granted, and an object otherwise.
type AccessResult struct {
	CanAccessAdmin bool           `json:"canAccessAdmin"`
	Collections    map[string]any `json:"collections"`
	Globals        map[string]any `json:"globals"`

	Raw  []byte    `json:"-"`
	HTTP *Response `json:"-"`
}

// Slugs returns the collection slugs present, sorted.
func (a *AccessResult) Slugs() []string { return sortedMapKeys(a.Collections) }

// GlobalSlugs returns the global slugs present, sorted.
func (a *AccessResult) GlobalSlugs() []string { return sortedMapKeys(a.Globals) }

// Access issues GET {api_path}/access and enforces H1: 200 + JSON + a decoded
// object with a `collections` key. Status alone is useless — a wrong base URL
// answers 200 text/html (verified) — so the Content-Type check is load-bearing.
func (c *Client) Access(ctx context.Context, opts ...Option) (*AccessResult, error) {
	req := applyOptions(&Request{Method: http.MethodGet, Path: "/access"}, opts)
	resp, err := c.Do(ctx, req)
	if err != nil {
		if resp != nil && !isJSONResponse(resp) {
			return nil, c.notPayload(resp)
		}
		return nil, err
	}
	if !isJSONResponse(resp) {
		return nil, c.notPayload(resp)
	}
	out := &AccessResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, c.notPayload(resp)
	}
	if out.Collections == nil {
		return nil, c.notPayload(resp)
	}
	return out, nil
}

func isJSONResponse(resp *Response) bool { return requireJSON(resp) == nil }

func (c *Client) notPayload(resp *Response) *apierr.Error {
	e := apierr.New(apierr.CodeEndpointNotPayload,
		"%s does not serve a Payload REST API: GET %s/access must answer 200 application/json with a `collections` key",
		c.BaseURL(), c.APIPath()).
		WithHint("check --base-url, and try --api-path /api (PayCLI probes /api, /cms-api, /payload-api and \"\")")
	if resp != nil {
		e = e.WithHTTP(&apierr.HTTP{
			Status: resp.Status, Method: resp.Method, URL: resp.URL,
			BodyExcerpt: excerpt(resp.Body),
		})
	}
	return e
}

// DocAccess issues POST /{collection}/access/{id} for per-document
// permissions.
func (c *Client) DocAccess(ctx context.Context, collection, id string, opts ...Option) (Doc, error) {
	req := applyOptions(&Request{
		Method: http.MethodPost,
		Path:   "/" + url.PathEscape(collection) + "/access/" + url.PathEscape(id),
		// This POST computes permissions and changes nothing, so it is
		// idempotency-safe under §6.1.
		ReadOnly: true,
	}, opts)
	req.Classify.Collection = collection
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	var doc Doc
	if err := decodeJSON(resp, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// InitResult is GET /{slug}/init.
type InitResult struct {
	// IsAuthCollection reports that the body carried an `initialized`
	// boolean, which is the only trustworthy signal that the slug is an auth
	// collection. The route is itself access-gated (crm-contacts answers 403
	// live), so this filter can narrow a candidate set but never widen it.
	IsAuthCollection bool
	Initialized      bool
	HTTP             *Response
}

// Init issues GET /{slug}/init with no Authorization header — Stage -1 step 3.
func (c *Client) Init(ctx context.Context, slug string, opts ...Option) (*InitResult, error) {
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(slug) + "/init",
		NoAuth: true,
	}, opts)
	req.Classify.Collection = slug
	resp, err := c.Do(ctx, req)
	if resp == nil {
		return nil, err
	}
	out := &InitResult{HTTP: resp}
	if !resp.OK() || !isJSONResponse(resp) {
		return out, nil // a non-2xx is a clean negative, not a command failure
	}
	var body map[string]any
	if json.Unmarshal(resp.Body, &body) != nil {
		return out, nil
	}
	if v, ok := body["initialized"].(bool); ok {
		out.IsAuthCollection = true
		out.Initialized = v
	}
	return out, nil
}

// ProbeIdentity is Stage -1 step 4: try the credential against one candidate
// auth collection without mutating the client's configuration.
func (c *Client) ProbeIdentity(ctx context.Context, candidate string, opts ...Option) (*Identity, error) {
	opts = append(opts, WithAuthCollection(candidate))
	return c.Me(ctx, candidate, opts...)
}

// AnonymousAccess fetches /access with no credential, which is Stage -1 step
// 1's candidate source. The result is truncated by construction and must never
// be cached or used as an inventory.
func (c *Client) AnonymousAccess(ctx context.Context, opts ...Option) (*AccessResult, error) {
	return c.Access(ctx, append(opts, WithoutAuth())...)
}

// Can reports whether a permission is granted in an access result.
// permission is one of create, read, update, delete. The second return is
// §7.6's tri-state: false means the server reported nothing about this
// permission, which is never the same as a denial.
func (a *AccessResult) Can(collection, permission string) (bool, bool) {
	return Permission(a.Collections[collection], permission)
}

// CanGlobal is Can for a global.
func (a *AccessResult) CanGlobal(slug, permission string) (bool, bool) {
	return Permission(a.Globals[slug], permission)
}

// Permission reads one permission out of an /api/access entry.
//
// Payload emits TWO shapes for the same fact: a collection with field-level
// access control answers {"read": {"permission": true, "fields": {…}}}, and one
// without answers a bare {"read": true} (verified live). Accepting only the
// nested form made PayCLI report "unknown" for a question the server had
// already answered, so both are decoded here, at the source.
func Permission(entry any, permission string) (permitted bool, known bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return false, false
	}
	switch t := m[permission].(type) {
	case bool:
		return t, true
	case map[string]any:
		if p, ok := t["permission"].(bool); ok {
			return p, true
		}
	}
	return false, false
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
