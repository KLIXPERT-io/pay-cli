package payload

import (
	"context"
	"net/http"
	"net/url"

	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// GlobalsPrefix is Payload's globals route prefix.
const GlobalsPrefix = "/globals/"

// GlobalPath renders the REST path of one global.
func GlobalPath(slug string) string { return GlobalsPrefix + url.PathEscape(slug) }

// GlobalGet issues GET /globals/{slug}. A global has no id argument — its
// GraphQL Query field takes only draft and select (verified) — so `--id` is
// meaningless here and the caller must not pass one.
func (c *Client) GlobalGet(ctx context.Context, slug string, p query.Params, opts ...Option) (Doc, *Response, error) {
	q, err := p.Encode()
	if err != nil {
		return nil, nil, err
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   GlobalPath(slug),
		Query:  q,
	}, opts)
	req.Classify.Collection = slug
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

// GlobalUpdate issues POST /globals/{slug} — Payload's update operation for a
// global is a POST, not a PATCH. The whole-document revalidation rule of
// §9.10.3 applies here too.
func (c *Client) GlobalUpdate(ctx context.Context, slug string, data map[string]any, p query.Params, opts ...Option) (*WriteResult, error) {
	res, err := c.writeDoc(ctx, http.MethodPost, GlobalPath(slug), data, p, opts)
	if res != nil && res.HTTP != nil {
		// Keep the slug in the classification context for the error message.
		res.HTTP.Method = http.MethodPost
	}
	return res, err
}
