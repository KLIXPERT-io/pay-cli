package discovery

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// probeFilename is the deliberately impossible filename used by §7.5 probe 2.
// It never matches a real upload, so the probe can only ever learn whether the
// /file/ route exists.
const probeFilename = "__paycli_probe__"

// Two English substrings are load-bearing, and only these two. Both are
// emitted by Payload WITHOUT req.t (verified hardcoded), so they survive
// Accept-Language negotiation and a project's own translations override:
//
//	Route not found "…"   — the REST catch-all
//	Cannot <METHOD> …     — the endpoints: false guard
//
// Everything else a probe matches on is a JSON key's presence, a value's JSON
// type or an HTTP status.
const (
	msgRouteNotFound = `Route not found`
	msgCannot        = `Cannot `
)

// probeResult is one collection's Stage-3 harvest. Every field is tri-state:
// a probe that did not run leaves nil, which the manifest renders as null and
// which every consumer must treat as "attempt the operation".
type probeResult struct {
	EndpointsDisabled *bool
	Upload            *bool
	Versions          *bool
	Drafts            *bool
	Trash             *bool
	Folders           *bool
	Auth              *bool

	PluralLabel string
	LabelParsed bool
	LabelTried  bool

	SampleID        any
	SampleIDPresent bool

	VersionParentID      any
	VersionParentPresent bool

	TotalDocs *int
}

// probeNeed says which probes a collection still needs. A fact GraphQL already
// established is never re-probed: that is what keeps §7.1's four-request cold
// budget intact on a project whose GraphQL is reachable, while a project with
// GraphQL disabled still gets every capability measured.
type probeNeed struct {
	Endpoints bool
	Upload    bool
	Versions  bool
	Drafts    bool
	Trash     bool
	Folders   bool
	Auth      bool
	Labels    bool
	IDSample  bool
	Count     bool
}

// any reports whether anything at all needs probing.
func (n probeNeed) any() bool {
	return n.Endpoints || n.Upload || n.Versions || n.Drafts || n.Trash ||
		n.Folders || n.Auth || n.Labels || n.IDSample || n.Count
}

// probeCollection runs §7.5's probes for one collection, in the specified
// order, on one goroutine. The 501 guard runs FIRST and short-circuits
// everything else: without it payload-migrations reads as an upload positive,
// because a 501 body has no top-level `message` starting `Route not found`.
func (d *Discoverer) probeCollection(ctx context.Context, slug string, need probeNeed) probeResult {
	var out probeResult

	// 1. 501 guard. The first request doubles as the id sample, so a
	// collection that is fine costs nothing extra for the guard.
	r := d.get(ctx, "/"+slug, "limit=1&depth=0&select%5Bid%5D=true", false)
	if isEndpointsDisabled(r) {
		out.EndpointsDisabled = boolPtr(true)
		return out
	}
	out.EndpointsDisabled = boolPtr(false)
	if r.Status == http.StatusOK {
		if m, ok := r.object(); ok {
			if docs, ok := m["docs"].([]any); ok && len(docs) > 0 {
				if doc, ok := docs[0].(map[string]any); ok {
					if id, present := doc["id"]; present {
						out.SampleID, out.SampleIDPresent = id, true
					}
				}
			}
			if total, ok := m["totalDocs"]; ok {
				if n, ok := jsonInt(total); ok {
					out.TotalDocs = intPtr(n)
				}
			}
		}
	}

	// 2. upload. The negative is a body whose message starts `Route not
	// found`; anything else (including the 500 an upload collection answers
	// for a missing file, verified live on media) is a positive.
	if need.Upload {
		r := d.get(ctx, "/"+slug+"/file/"+probeFilename, "", false)
		if isEndpointsDisabled(r) {
			out.EndpointsDisabled = boolPtr(true)
			return out
		}
		out.Upload = boolPtr(!strings.HasPrefix(r.message(), msgRouteNotFound))
	}

	// 3. versions. The `docs` array check is mandatory: payload-preferences
	// registers a custom GET /:key that shadows /versions and answers 200
	// {"message":"Not Found","value":null} (verified).
	if need.Versions {
		r := d.get(ctx, "/"+slug+"/versions", "limit=1&depth=0", false)
		if isEndpointsDisabled(r) {
			out.EndpointsDisabled = boolPtr(true)
			return out
		}
		ok := false
		if r.Status == http.StatusOK {
			if m, decoded := r.object(); decoded {
				docs, isArray := m["docs"].([]any)
				ok = isArray
				if ok && len(docs) > 0 {
					if doc, isObj := docs[0].(map[string]any); isObj {
						// parent is the DOCUMENT id (verified: parent 16 while
						// the version's own id is 20), which is what the
						// id_type ladder needs.
						if p, present := doc["parent"]; present {
							out.VersionParentID, out.VersionParentPresent = p, true
						}
					}
				}
			}
		}
		out.Versions = boolPtr(ok)
	}

	// 4. drafts. A collection without drafts answers 400 QueryError on
	// _status. The count endpoint is used rather than a find, because
	// `limit=0` means UNLIMITED in Payload (verified) and would download the
	// whole collection to learn one boolean.
	if need.Drafts {
		r := d.get(ctx, "/"+slug+"/count", "where%5B_status%5D%5Bequals%5D=published", false)
		if isEndpointsDisabled(r) {
			out.EndpointsDisabled = boolPtr(true)
			return out
		}
		out.Drafts = boolPtr(r.Status == http.StatusOK && hasKey(r, "totalDocs"))
	}

	// 5. trash.
	if need.Trash {
		r := d.get(ctx, "/"+slug+"/count", "where%5BdeletedAt%5D%5Bexists%5D=true", false)
		out.Trash = boolPtr(r.Status == http.StatusOK && hasKey(r, "totalDocs"))
	}

	// 6. folders.
	if need.Folders {
		r := d.get(ctx, "/"+slug+"/count", "where%5Bfolder%5D%5Bexists%5D=true", false)
		out.Folders = boolPtr(r.Status == http.StatusOK && hasKey(r, "totalDocs"))
	}

	// 7. auth.
	if need.Auth {
		out.Auth = boolPtr(d.probeInit(ctx, slug))
	}

	// 8. plural label. DELETE with where[id][equals]=-1 matches nothing and
	// has zero side effects (verified), but it is still a DELETE verb, so it
	// is skipped entirely when labels are not wanted.
	if need.Labels {
		out.LabelTried = true
		r := d.do(ctx, http.MethodDelete, "/"+slug, "where%5Bid%5D%5Bequals%5D=-1", false, nil, "")
		if !isEndpointsDisabled(r) && r.Status == http.StatusOK {
			if plural, ok := ParseDeleteMessage(r.message()); ok {
				out.PluralLabel, out.LabelParsed = plural, true
			}
		}
	}

	return out
}

// isEndpointsDisabled is §7.5's 501 guard. The status and the message prefix
// are checked together because a 501 from a reverse proxy carries neither.
func isEndpointsDisabled(r *restResult) bool {
	if r == nil || r.Status != http.StatusNotImplemented {
		return false
	}
	return strings.HasPrefix(r.message(), msgCannot)
}

func hasKey(r *restResult, key string) bool {
	m, ok := r.object()
	if !ok {
		return false
	}
	_, present := m[key]
	return present
}

func jsonInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	default:
		if s := IDString(v); s != "" {
			n := 0
			for _, r := range s {
				if r < '0' || r > '9' {
					return 0, false
				}
				n = n*10 + int(r-'0')
			}
			return n, true
		}
		return 0, false
	}
}

// runProbes fans §7.5's per-collection probes out over a worker pool of
// --concurrency (default 8).
func (d *Discoverer) runProbes(ctx context.Context, work map[string]probeNeed) map[string]probeResult {
	slugs := make([]string, 0, len(work))
	for slug, need := range work {
		if need.any() {
			slugs = append(slugs, slug)
		}
	}
	out := make(map[string]probeResult, len(slugs))
	if len(slugs) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.concurrency)
	for _, slug := range slugs {
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := d.probeCollection(ctx, slug, work[slug])
			mu.Lock()
			out[slug] = res
			mu.Unlock()
		}(slug)
	}
	wg.Wait()
	return out
}

// projectProbes is §7.5's three project-level probes.
type projectProbes struct {
	Reorder     *bool
	OG          *bool
	Preferences *bool
}

// runProjectProbes issues the three project-level probes in parallel.
//
// None of them is a write: POST {api_path}/reorder answers 404 `Route not
// found` when the project has no orderable fields, and it is Payload's own
// reorder endpoint which requires a body to do anything.
func (d *Discoverer) runProjectProbes(ctx context.Context) projectProbes {
	var out projectProbes
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		r := d.do(ctx, http.MethodPost, "/reorder", "", false, []byte("{}"), "application/json")
		if r.Status == http.StatusNotFound && strings.HasPrefix(r.message(), msgRouteNotFound) {
			out.Reorder = boolPtr(false)
			return
		}
		if r.Err != nil && r.Status == 0 {
			return // network failure teaches nothing; stay unknown
		}
		out.Reorder = boolPtr(true)
	}()

	go func() {
		defer wg.Done()
		r := d.get(ctx, "/og", "", false)
		if r.Status == 0 {
			return
		}
		out.OG = boolPtr(r.Status == http.StatusOK)
	}()

	go func() {
		defer wg.Done()
		r := d.get(ctx, "/payload-preferences/"+probeFilename, "", false)
		if r.Status == 0 {
			return
		}
		// The preferences collection registers a custom GET /:key, so any
		// answer that is not `Route not found` proves the collection exists.
		out.Preferences = boolPtr(!strings.HasPrefix(r.message(), msgRouteNotFound))
	}()

	wg.Wait()
	return out
}

// sampleDocs fetches a small page of documents for the REST-only field
// fallback and for §7.9b's locale=all analysis.
//
// Payload returns every field including nulls at depth 0, so the key set of one
// document is the field list — the only REST field-name source there is.
// /api/access `fields` is NEVER used for this: it is the boolean true for a
// privileged key (verified) and an object for a restricted one.
func (d *Discoverer) sampleDocs(ctx context.Context, slug string, limit int, localeAll bool) []map[string]any {
	q := "limit=" + itoa(limit) + "&depth=0"
	if localeAll {
		q += "&locale=all"
	}
	r := d.get(ctx, "/"+slug, q, false)
	if r.Status != http.StatusOK {
		return nil
	}
	m, ok := r.object()
	if !ok {
		return nil
	}
	raw, ok := m["docs"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if doc, ok := v.(map[string]any); ok {
			out = append(out, doc)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
