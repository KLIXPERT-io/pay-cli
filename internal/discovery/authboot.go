package discovery

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// AuthBootstrap is Stage -1's result (§7.0).
type AuthBootstrap struct {
	// Collection is the resolved auth-collection slug.
	Collection string
	// Source is "bootstrap" when Stage -1 ran, "configured" when it was
	// skipped because the slug was set, "cached" when it came from
	// auth-resolution.json.
	Source string
	// Candidates are every slug that survived the /init filter, in the order
	// they were tried. They are recorded in diagnostics.auth_candidates[].
	Candidates []string
	// Truncated reports that the candidate set was the anonymous slug list
	// alone.
	Truncated bool
	// Requests is the number of HTTP requests Stage -1 spent.
	Requests int
}

// runAuthBootstrap resolves §7.0's circularity: the api-key header embeds the
// auth-collection slug, so with auth_collection = "auto" there is no slug to
// send on the very first request — and guessing wrong does not error.
//
// Verified live: `Authorization: admins API-Key <valid key>` and
// `Authorization: not-a-collection API-Key <valid key>` BOTH return HTTP 200
// with the anonymous 10-collection view, byte-indistinguishable from a real
// low-privilege key. A bootstrap that guessed would silently produce a
// truncated inventory in which 39 of 49 collections appear not to exist.
//
// Identity is therefore verified by body.user != null, never by a status code.
func (d *Discoverer) runAuthBootstrap(ctx context.Context) (*AuthBootstrap, error) {
	out := &AuthBootstrap{Source: SourceBootstrap, Candidates: []string{}}

	// Step 1: GET {api_path}/access with NO Authorization header, purely to
	// obtain a candidate slug set. The result is truncated by construction and
	// is never cached, never used as an inventory and never written to a
	// manifest.
	before, _ := d.counters()
	anon := d.get(ctx, "/access", "", true)
	candidates := map[string]bool{}
	sawAnon := false
	if view, ok := decodeAccess(anon); ok {
		sawAnon = true
		for _, s := range view.Slugs() {
			candidates[s] = true
		}
	}

	// Step 2: widen. The GraphQL Stage-1 signal is the only source that sees
	// every collection regardless of permission — an entity is an auth
	// collection iff Query has both me{S} and initialized{S}.
	widened := false
	if res, err := d.gqlOnce(ctx); err == nil || res != nil {
		if snap, ok := decodeStage1(res); ok {
			for _, slug := range authSlugsFromSchema(snap) {
				candidates[slug] = true
				widened = true
			}
		}
	}
	// Plus the project's own payload.config.ts slug literals, when §4.3's
	// upward walk found one. Local filesystem, no network.
	for _, slug := range d.projectAuthSlugs {
		if slug != "" {
			candidates[slug] = true
			widened = true
		}
	}
	if !widened && sawAnon {
		out.Truncated = true
	}
	if len(candidates) == 0 {
		out.Requests = currentRequests(d) - before
		return out, d.authUnknownError(nil)
	}

	// Step 3: filter to real auth collections. /{slug}/init needs no auth
	// header and is itself access-gated (verified: /api/crm-contacts/init does
	// not answer an `initialized` boolean), so this narrows the set but can
	// never widen it.
	sorted := make([]string, 0, len(candidates))
	for s := range candidates {
		sorted = append(sorted, s)
	}
	sort.Strings(sorted)

	survivors := d.filterInit(ctx, sorted)
	out.Candidates = survivors
	if len(survivors) == 0 {
		out.Requests = currentRequests(d) - before
		return out, d.authUnknownError(sorted)
	}

	// Step 4: test the credential. Accept the first candidate in sorted order
	// whose body.user != null. Ties are impossible by construction and the
	// deterministic sort makes the choice reproducible.
	if d.credentialAbsent || d.authMode == payload.AuthModeAnonymous {
		out.Collection = survivors[0]
		out.Requests = currentRequests(d) - before
		return out, nil
	}
	for _, slug := range survivors {
		req := &payload.Request{
			Method:   http.MethodGet,
			Path:     d.apiPath + "/" + slug + "/me",
			Absolute: true,
			// In api-key mode the slug is part of the header. In jwt mode it
			// is not, and the first candidate returning user != null wins.
			AuthCollectionOverride: slug,
		}
		req.Classify.Collection = slug
		resp, _ := d.client.Do(ctx, req)
		d.countRequest(resp)
		if resp == nil {
			continue
		}
		id := decodeIdentity(&restResult{
			Status: resp.Status, ContentType: resp.ContentType, Body: resp.Body, Header: resp.Header,
		}, slug)
		if id != nil && id.Verified {
			out.Collection = slug
			out.Requests = currentRequests(d) - before
			return out, nil
		}
	}

	// Step 5: nothing matched. PayCLI never guesses `users`.
	out.Requests = currentRequests(d) - before
	return out, d.authUnknownError(survivors)
}

// authSlugsFromSchema implements step 2's GraphQL signal.
func authSlugsFromSchema(snap *schemaSnapshot) []string {
	if snap == nil {
		return nil
	}
	entities, _ := zipInventory(snap, nil)
	out := []string{}
	for slug, e := range entities {
		if e.Singular == "" {
			continue
		}
		if snap.QueryByName["me"+e.Singular] != nil && snap.QueryByName["initialized"+e.Singular] != nil {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
}

// filterInit runs step 3's /init probe in parallel at --concurrency, keeping
// only the candidates whose body carries an `initialized` boolean.
func (d *Discoverer) filterInit(ctx context.Context, candidates []string) []string {
	keep := make([]bool, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.concurrency)
	for i, slug := range candidates {
		wg.Add(1)
		go func(i int, slug string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			keep[i] = d.probeInit(ctx, slug)
		}(i, slug)
	}
	wg.Wait()
	out := []string{}
	for i, ok := range keep {
		if ok {
			out = append(out, candidates[i])
		}
	}
	return out
}

// probeInit reports whether GET /{slug}/init answers an `initialized` boolean.
// The boolean's presence is the signal — a 200 with any other shape is a
// negative, because a custom endpoint can shadow the route.
func (d *Discoverer) probeInit(ctx context.Context, slug string) bool {
	r := d.get(ctx, "/"+slug+"/init", "", true)
	if r == nil || r.Status != http.StatusOK {
		return false
	}
	m, ok := r.object()
	if !ok {
		return false
	}
	_, isBool := m["initialized"].(bool)
	return isBool
}

// currentRequests reads the shared request counter under the discoverer's
// lock, so Stage -1 can report its own cost without racing the worker pool.
func currentRequests(d *Discoverer) int {
	n, _ := d.counters()
	return n
}

func (d *Discoverer) authUnknownError(tried []string) error {
	hint := "--auth-collection <slug>"
	if len(tried) > 0 {
		shown := tried
		if len(shown) > 8 {
			shown = shown[:8]
		}
		hint = "--auth-collection <slug>  (candidates tried: " + strings.Join(shown, ", ") + ")"
	}
	return apierr.New(apierr.CodeAuthCollectionUnknown,
		"no auth collection on this server accepted the credential, so PayCLI cannot build the Authorization header").
		WithHint("%s", hint)
}
