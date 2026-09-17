package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// apiPathCandidates is §7.2's probe list. The configured value is prepended by
// the caller, so this is the fallback order only.
var apiPathCandidates = []string{"/api", "/cms-api", "/payload-api", ""}

// restResult is one raw REST exchange. Discovery keeps the status, the
// content type and the bytes because every probe classifies on *shape*, and
// several of them need a non-2xx body.
type restResult struct {
	Status      int
	ContentType string
	Body        []byte
	Header      http.Header
	Err         error
}

// json decodes the body into v, reporting false for a non-JSON or unparseable
// body. Numbers decode through json.Number so a 19-digit id never round-trips
// through float64.
func (r *restResult) json(v any) bool {
	if r == nil || !isJSONContentType(r.ContentType) {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(r.Body)))
	dec.UseNumber()
	return dec.Decode(v) == nil
}

// object decodes the body into a generic map.
func (r *restResult) object() (map[string]any, bool) {
	var m map[string]any
	if !r.json(&m) || m == nil {
		return nil, false
	}
	return m, true
}

// message returns the top-level "message" string, which is the only field two
// Payload responses carry *without* req.t — `Route not found "…"` and
// `Cannot <METHOD> …`. Those two literals are the only English substrings any
// probe is permitted to depend on.
func (r *restResult) message() string {
	m, ok := r.object()
	if !ok {
		return ""
	}
	s, _ := m["message"].(string)
	return s
}

// get issues one REST request against the resolved api_path.
//
// Every request is built with Absolute: true and an explicit prefix, because
// discovery may resolve an api_path the client was not configured with
// (§7.2's autodiscovery) and a stale client prefix would silently probe the
// wrong routes.
func (d *Discoverer) get(ctx context.Context, path, query string, noAuth bool) *restResult {
	return d.do(ctx, http.MethodGet, path, query, noAuth, nil, "")
}

func (d *Discoverer) do(ctx context.Context, method, path, query string, noAuth bool, body []byte, contentType string) *restResult {
	req := &payload.Request{
		Method:      method,
		Path:        d.apiPath + path,
		Absolute:    true,
		Query:       query,
		Body:        body,
		ContentType: contentType,
		NoAuth:      noAuth,
		// The client may have been built before Stage -1 resolved the auth
		// collection, and in api-key mode the slug is part of the header. The
		// override keeps every probe on the slug discovery actually resolved
		// without mutating the shared client.
		AuthCollectionOverride: d.authCollection,
		// KnownRoute is deliberately never set: it is what turns a 404 into a
		// §8.4 Level-3 staleness signal, and discovery's probes produce 404s
		// by design.
	}
	if slug := slugFromProbePath(path); slug != "" {
		req.Classify.Collection = slug
	}
	resp, err := d.client.Do(ctx, req)
	d.countRequest(resp)
	if resp == nil {
		return &restResult{Err: err}
	}
	return &restResult{
		Status:      resp.Status,
		ContentType: resp.ContentType,
		Body:        resp.Body,
		Header:      resp.Header,
		Err:         err,
	}
}

// slugFromProbePath names the collection an error message may mention, so a
// classified error never invents one.
func slugFromProbePath(path string) string {
	p := strings.TrimPrefix(path, "/")
	if p == "" {
		return ""
	}
	seg := p
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "access", "globals", "graphql", "reorder", "og":
		return ""
	}
	return seg
}

// accessView is a decoded GET {api_path}/access.
type accessView struct {
	CanAccessAdmin bool
	Collections    map[string]any
	Globals        map[string]any
	PoweredBy      string
	Raw            []byte
}

// Slugs returns the collection slugs, sorted.
func (a *accessView) Slugs() []string { return sortedKeys(a.Collections) }

// GlobalSlugs returns the global slugs, sorted.
func (a *accessView) GlobalSlugs() []string { return sortedKeys(a.Globals) }

// decodeAccess enforces H1: 200 + application/json + a decoded object with a
// `collections` key. Status alone is useless — a wrong base URL answers
// 200 text/html (verified) — so the Content-Type check is load-bearing.
func decodeAccess(r *restResult) (*accessView, bool) {
	if r == nil || r.Status != http.StatusOK {
		return nil, false
	}
	m, ok := r.object()
	if !ok {
		return nil, false
	}
	colls, ok := m["collections"].(map[string]any)
	if !ok {
		return nil, false
	}
	globals, _ := m["globals"].(map[string]any)
	if globals == nil {
		globals = map[string]any{}
	}
	admin, _ := m["canAccessAdmin"].(bool)
	view := &accessView{
		CanAccessAdmin: admin,
		Collections:    colls,
		Globals:        globals,
		Raw:            r.Body,
	}
	if r.Header != nil {
		view.PoweredBy = r.Header.Get(payload.HeaderPoweredBy)
	}
	return view, true
}

// resolveAPIPath implements §7.2's api_path autodiscovery. It runs only when
// api_path was not explicitly configured.
//
// Candidates are probed concurrently and the first acceptance *in list order*
// wins, so the answer does not depend on which goroutine finished first.
func (d *Discoverer) resolveAPIPath(ctx context.Context) (string, string, *accessView) {
	if d.apiPathSource == SourceConfigured {
		r := d.get(ctx, "/access", "", false)
		view, ok := decodeAccess(r)
		if ok {
			return d.apiPath, SourceConfigured, view
		}
		return d.apiPath, SourceConfigured, nil
	}

	candidates := []string{}
	seen := map[string]bool{}
	for _, c := range append([]string{d.apiPath}, apiPathCandidates...) {
		c = normalizeAPIPath(c)
		if seen[c] {
			continue
		}
		seen[c] = true
		candidates = append(candidates, c)
	}

	views := make([]*accessView, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.concurrency)
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := &payload.Request{
				Method: http.MethodGet, Path: c + "/access", Absolute: true,
				AuthCollectionOverride: d.authCollection,
			}
			resp, _ := d.client.Do(ctx, req)
			d.countRequest(resp)
			if resp == nil {
				return
			}
			view, ok := decodeAccess(&restResult{
				Status: resp.Status, ContentType: resp.ContentType,
				Body: resp.Body, Header: resp.Header,
			})
			if ok {
				views[i] = view
			}
		}(i, c)
	}
	wg.Wait()

	for i, v := range views {
		if v == nil {
			continue
		}
		source := SourceProbed
		if i == 0 && candidates[0] == normalizeAPIPath(d.apiPath) {
			source = d.apiPathSource
			if source == "" {
				source = SourceProbed
			}
		}
		return candidates[i], source, v
	}
	return normalizeAPIPath(d.apiPath), d.apiPathSource, nil
}

func normalizeAPIPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// stage0 is §7.2: connectivity, then identity.
func (d *Discoverer) stage0(ctx context.Context) (*accessView, *payload.Identity, error) {
	apiPath, source, view := d.resolveAPIPath(ctx)
	d.apiPath = apiPath
	d.apiPathSource = source
	d.recomputeGraphQLPath()

	if view == nil {
		// §7.6: discovery_failed is reserved for the case where REST-only
		// discovery also fails, i.e. GET {api_path}/access itself did not
		// return parseable JSON. That is this branch.
		return nil, nil, d.notPayloadError()
	}

	if d.authMode == payload.AuthModeAnonymous || d.credentialAbsent {
		// §7.2: in anonymous mode the /me request is NOT made. Discovery
		// continues; it does not fail.
		return view, nil, nil
	}
	if d.authCollection == "" {
		return view, nil, nil
	}

	r := d.get(ctx, "/"+d.authCollection+"/me", "", false)
	id := decodeIdentity(r, d.authCollection)
	if id == nil || !id.Verified {
		return view, nil, apierr.New(apierr.CodeAuthInvalid,
			"the server accepted the request but reports no user for this credential on %q", d.authCollection).
			WithHint("a wrong API key answers HTTP 200 with user:null, and a wrong auth-collection slug silently " +
				"gives the anonymous view — re-check both with `pay auth login`")
	}
	return view, id, nil
}

// decodeIdentity reads /{auth}/me. The signal is body.user != null, NEVER the
// status code: a wrong key and a wrong auth-collection slug both answer 200.
func decodeIdentity(r *restResult, collection string) *payload.Identity {
	if r == nil || r.Status != http.StatusOK {
		return nil
	}
	var body struct {
		User    map[string]any `json:"user"`
		Token   string         `json:"token"`
		Exp     json.Number    `json:"exp"`
		Message string         `json:"message"`
	}
	if !r.json(&body) {
		return nil
	}
	id := &payload.Identity{
		Collection: collection,
		Verified:   body.User != nil,
		Token:      body.Token,
	}
	if body.User != nil {
		id.User = payload.Doc(body.User)
		id.UserID = body.User["id"]
		if s, ok := body.User["_strategy"].(string); ok {
			id.Strategy = s
		}
		// canAccessAdmin is absent when false, never literally false.
		if v, ok := body.User["canAccessAdmin"].(bool); ok {
			id.CanAccessAdmin = v
		}
	}
	// The /me body carries the API key in plaintext on a project with
	// useAPIKey, so it is deliberately not retained past this point (§7.8.3).
	return id
}

func (d *Discoverer) notPayloadError() error {
	e := apierr.New(apierr.CodeEndpointNotPayload,
		"%s does not serve a Payload REST API: GET %s/access must answer 200 application/json with a `collections` key",
		d.baseURL, d.apiPath)
	hint := "check --base-url, and try --api-path /api (PayCLI probes /api, /cms-api, /payload-api and \"\")"
	if len(d.projectRoutes) > 0 {
		sorted := append([]string(nil), d.projectRoutes...)
		sort.Strings(sorted)
		hint = "this project's payload.config.ts names routes " + strings.Join(sorted, ", ") +
			" — try --api-path " + sorted[0]
	}
	return e.WithHint("%s", hint)
}

// recomputeGraphQLPath keeps graphql_path derived from the resolved api_path
// (§4.2 / conflict 34): Payload computes the GraphQL route as
// routes.api + routes.graphQL, so the two are nested, never independent.
func (d *Discoverer) recomputeGraphQLPath() {
	if d.graphQLPathSource == SourceConfigured {
		return
	}
	route := d.graphQLRoute
	if route == "" {
		route = "/graphql"
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	d.graphQLPath = d.apiPath + route
	d.graphQLPathSource = SourceDerivedGraphQLPath
}
