package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// Doc is one decoded Payload document. Numbers are json.Number, so a large
// integer id round-trips verbatim instead of through float64.
type Doc map[string]any

// ID returns the document id, or nil when the document has none.
func (d Doc) ID() any { return d["id"] }

// IDString renders the id for a URL path.
func (d Doc) IDString() string { return idToString(d["id"]) }

func idToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	}
}

// Option customises one request without widening every operation signature.
type Option func(*Request)

// WithClassify attaches the §11.2 classification context.
func WithClassify(ctx ClassifyContext) Option {
	return func(r *Request) { r.Classify = ctx }
}

// WithKnownRoute marks the route as one the manifest claims exists, which is
// what lets a 404 become a Level-3 staleness signal (§8.4).
func WithKnownRoute() Option { return func(r *Request) { r.KnownRoute = true } }

// WithTimeout overrides the per-request timeout (uploads and bulk use 120 s).
func WithTimeout(d time.Duration) Option { return func(r *Request) { r.Timeout = d } }

// WithHeader adds a request-specific header. Authorization is owned by the
// transport and cannot be set here.
func WithHeader(name, value string) Option {
	return func(r *Request) {
		if strings.EqualFold(name, HeaderAuthorization) {
			return
		}
		if r.Header == nil {
			r.Header = http.Header{}
		}
		r.Header.Set(name, value)
	}
}

// WithoutAuth suppresses the Authorization header (Stage -1's candidate probes).
func WithoutAuth() Option { return func(r *Request) { r.NoAuth = true } }

// WithAuthCollection sends the Authorization header for a specific candidate
// slug without mutating the client (Stage -1 step 4).
func WithAuthCollection(slug string) Option {
	return func(r *Request) { r.AuthCollectionOverride = slug }
}

func applyOptions(r *Request, opts []Option) *Request {
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	if r.Classify.Collection == "" && !r.Absolute {
		r.Classify.Collection = collectionFromPath(r.Path)
	}
	return r
}

// nonCollectionRoutes are first path segments that are Payload routes rather
// than collection slugs, so an error on them never claims a collection name.
var nonCollectionRoutes = map[string]bool{
	"access":  true,
	"globals": true,
	"graphql": true,
}

func collectionFromPath(p string) string {
	p = strings.TrimPrefix(NormalizePath(p), "/")
	if p == "" {
		return ""
	}
	slug, _, _ := strings.Cut(p, "/")
	if nonCollectionRoutes[slug] {
		return ""
	}
	return slug
}

// Page is Payload's pagination envelope.
type Page struct {
	TotalDocs     int  `json:"totalDocs"`
	Limit         int  `json:"limit"`
	Page          int  `json:"page"`
	TotalPages    int  `json:"totalPages"`
	PagingCounter int  `json:"pagingCounter"`
	HasNextPage   bool `json:"hasNextPage"`
	HasPrevPage   bool `json:"hasPrevPage"`
	NextPage      *int `json:"nextPage"`
	PrevPage      *int `json:"prevPage"`
}

// ListResult is a find response.
type ListResult struct {
	Docs []Doc `json:"docs"`
	Page
	// Raw is the untouched response body, for --output raw.
	Raw []byte `json:"-"`
	// HTTP is the response that produced it.
	HTTP *Response `json:"-"`
}

// IDs returns the ids of the returned documents in order.
func (l *ListResult) IDs() []any {
	out := make([]any, 0, len(l.Docs))
	for _, d := range l.Docs {
		out = append(out, d.ID())
	}
	return out
}

// Find issues GET /{collection} with the encoded parameters.
func (c *Client) Find(ctx context.Context, collection string, p query.Params, opts ...Option) (*ListResult, error) {
	q, err := p.Encode()
	if err != nil {
		return nil, err
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(collection),
		Query:  q,
	}, opts)
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	out := &ListResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get issues GET /{collection}/{id}. Payload returns the document itself, not
// a wrapper.
func (c *Client) Get(ctx context.Context, collection, id string, p query.Params, opts ...Option) (Doc, *Response, error) {
	q, err := p.Encode()
	if err != nil {
		return nil, nil, err
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(collection) + "/" + url.PathEscape(id),
		Query:  q,
	}, opts)
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, resp, err
	}
	var doc Doc
	if err := decodeJSON(resp, &doc); err != nil {
		return nil, resp, err
	}
	return doc, resp, nil
}

// Count issues GET /{collection}/count. Payload's count accepts only `where`
// and `trash`; it ignores `draft` entirely (verified), so the caller is
// expected to have stripped the rest.
func (c *Client) Count(ctx context.Context, collection string, p query.Params, opts ...Option) (int, *Response, error) {
	// Only where and trash: §2 ground truth line 128 measured that /count
	// IGNORES draft (`/pages/count` = 11, `?draft=true` = 11), and everything
	// else (depth, select, sort, pagination, locale) is a projection that
	// cannot change a total. Callers may therefore hand Count their full write
	// scope — it takes the two parameters that select which documents exist
	// and drops the rest. Where phase 1 and phase 3 can still disagree (a
	// --where over a localised field), the bulk_resolve_gap warning reports it
	// rather than this function guessing.
	countParams := query.Params{Where: p.Where, WhereStyle: p.WhereStyle, WhereRaw: p.WhereRaw, Trash: p.Trash}
	q, err := countParams.Encode()
	if err != nil {
		return 0, nil, err
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(collection) + "/count",
		Query:  q,
	}, opts)
	resp, err := c.Do(ctx, req)
	if err != nil {
		return 0, resp, err
	}
	var out struct {
		TotalDocs int `json:"totalDocs"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return 0, resp, err
	}
	return out.TotalDocs, resp, nil
}

// FindPages walks every page of a query, calling fn once per page. It stops
// after max documents (max <= 0 means no client-side cap) or when fn returns
// an error. Paging is explicit rather than limit=0 because limit=0 means
// *unlimited* to Payload, which is exactly the footgun this avoids.
func (c *Client) FindPages(ctx context.Context, collection string, p query.Params, max int, fn func(*ListResult) error, opts ...Option) error {
	page := p.Page
	if page <= 0 {
		page = 1
	}
	seen := 0
	for {
		pageParams := p.Clone()
		pageParams.Page = page
		result, err := c.Find(ctx, collection, pageParams, opts...)
		if err != nil {
			return err
		}
		if max > 0 && seen+len(result.Docs) > max {
			result.Docs = result.Docs[:max-seen]
		}
		seen += len(result.Docs)
		if err := fn(result); err != nil {
			return err
		}
		if !result.HasNextPage || result.NextPage == nil || len(result.Docs) == 0 {
			return nil
		}
		if max > 0 && seen >= max {
			return nil
		}
		page = *result.NextPage
	}
}

// ResolveIDs is the where-only form of §12.3 phase 3, kept for callers that
// genuinely have no other scope to apply.
//
// PREFER ResolveIDsScoped. Taking a bare Where made it easy to resolve on a
// different population than the one that was counted and than the one the
// write then touches — the trash/draft/locale scope simply had nowhere to go
// in this signature, so callers dropped it. Delegating rather than duplicating
// the paging logic means the two can no longer drift.
func (c *Client) ResolveIDs(ctx context.Context, collection string, where query.Where, count int, opts ...Option) ([]any, error) {
	return c.ResolveIDsScoped(ctx, collection, query.Params{Where: where}, count, opts...)
}
