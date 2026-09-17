package payload

import (
	"context"
	"net/http"
	"net/url"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// VersionTarget names either a collection or a global. Exactly one of the two
// fields is set.
type VersionTarget struct {
	Collection string
	Global     string
}

// CollectionTarget builds a target for a collection.
func CollectionTarget(slug string) VersionTarget { return VersionTarget{Collection: slug} }

// GlobalTarget builds a target for a global.
func GlobalTarget(slug string) VersionTarget { return VersionTarget{Global: slug} }

// Slug is the entity slug, whichever kind it is.
func (t VersionTarget) Slug() string {
	if t.Global != "" {
		return t.Global
	}
	return t.Collection
}

// IsGlobal reports whether the target is a global.
func (t VersionTarget) IsGlobal() bool { return t.Global != "" }

func (t VersionTarget) basePath() (string, error) {
	switch {
	case t.Global != "" && t.Collection != "":
		return "", apierr.New(apierr.CodeInvalidArgs, "a version target is either a collection or a global, not both")
	case t.Global != "":
		return GlobalPath(t.Global), nil
	case t.Collection != "":
		return "/" + url.PathEscape(t.Collection), nil
	default:
		return "", apierr.New(apierr.CodeInvalidArgs, "no collection or global was named")
	}
}

// VersionsList issues GET /{target}/versions.
//
// A 200 is not proof the collection has versions: payload-preferences answers
// 200 with {"message":"Not Found","value":null} because a custom GET /:key
// route shadows the versions route (verified). The `docs` array is the only
// trustworthy signal, so this returns HasDocs alongside the result.
func (c *Client) VersionsList(ctx context.Context, target VersionTarget, p query.Params, opts ...Option) (*ListResult, bool, error) {
	base, err := target.basePath()
	if err != nil {
		return nil, false, err
	}
	q, err := p.Encode()
	if err != nil {
		return nil, false, err
	}
	req := applyOptions(&Request{Method: http.MethodGet, Path: base + "/versions", Query: q}, opts)
	req.Classify.Collection = target.Slug()
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, false, err
	}
	var probe struct {
		Docs []Doc `json:"docs"`
	}
	if err := decodeJSON(resp, &probe); err != nil {
		return nil, false, err
	}
	if probe.Docs == nil {
		return &ListResult{Raw: resp.Body, HTTP: resp}, false, nil
	}
	out := &ListResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// VersionGet issues GET /{target}/versions/{id}.
func (c *Client) VersionGet(ctx context.Context, target VersionTarget, versionID string, p query.Params, opts ...Option) (Doc, *Response, error) {
	base, err := target.basePath()
	if err != nil {
		return nil, nil, err
	}
	q, err := p.Encode()
	if err != nil {
		return nil, nil, err
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   base + "/versions/" + url.PathEscape(versionID),
		Query:  q,
	}, opts)
	req.Classify.Collection = target.Slug()
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

// VersionRestore issues POST /{target}/versions/{id}, which promotes that
// version back to the live document.
func (c *Client) VersionRestore(ctx context.Context, target VersionTarget, versionID string, p query.Params, opts ...Option) (*WriteResult, error) {
	base, err := target.basePath()
	if err != nil {
		return nil, err
	}
	return c.writeDoc(ctx, http.MethodPost, base+"/versions/"+url.PathEscape(versionID), nil, p, opts)
}
