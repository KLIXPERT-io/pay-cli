package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// BulkChunk is the id-set size of one bulk request (§12.3 step 3). Chunking
// keeps each URL well inside the 8 KB budget, which matters because the
// method-override fallback is forbidden for PATCH and DELETE.
const BulkChunk = 100

// DefaultMaxBulk is defaults.max_bulk (§12.3 step 2).
const DefaultMaxBulk = 100

// WriteResult is a single-document write response.
type WriteResult struct {
	Doc     Doc    `json:"doc"`
	Message string `json:"message"`

	Raw  []byte    `json:"-"`
	HTTP *Response `json:"-"`
}

// BulkFailure is one per-id failure from a bulk verb.
type BulkFailure struct {
	ID      any    `json:"id"`
	Message string `json:"message"`
}

// BulkResult is a bulk write response. Payload answers HTTP 400 whenever
// errors[] is non-empty even though the documents in docs[] were committed,
// so a caller must read Docs even when the error is non-nil.
type BulkResult struct {
	Docs    []Doc         `json:"docs"`
	Errors  []BulkFailure `json:"errors"`
	Message string        `json:"message"`

	// Raw and HTTP are the response of ONE request. A bulk write is chunked
	// (BulkChunk ids per request), so when Chunks > 1 they belong to the chunk
	// named by FailedChunk — the FIRST chunk that failed, because that is the
	// request whose diagnostics the caller has to read. With no failure they
	// are the last chunk's.
	Raw  []byte    `json:"-"`
	HTTP *Response `json:"-"`

	// Chunks is how many bulk requests were issued for this result.
	Chunks int `json:"-"`
	// FailedChunk is the 1-based index of the first chunk that failed (a
	// transport/HTTP error, or a non-empty errors[] under any status). It is 0
	// when every chunk succeeded.
	FailedChunk int `json:"-"`
}

// IDs returns the ids of the committed documents.
func (b *BulkResult) IDs() []any {
	out := make([]any, 0, len(b.Docs))
	for _, d := range b.Docs {
		out = append(out, d.ID())
	}
	return out
}

// Create issues POST /{collection}.
func (c *Client) Create(ctx context.Context, collection string, data map[string]any, p query.Params, opts ...Option) (*WriteResult, error) {
	return c.writeDoc(ctx, http.MethodPost, "/"+url.PathEscape(collection), data, p, opts)
}

// Update issues PATCH /{collection}/{id}.
//
// Payload re-validates the WHOLE document on a PATCH: a one-field update can
// return 400 naming fields the caller never sent (verified). That is why the
// classification context carries SentPaths.
func (c *Client) Update(ctx context.Context, collection, id string, data map[string]any, p query.Params, opts ...Option) (*WriteResult, error) {
	return c.writeDoc(ctx, http.MethodPatch, "/"+url.PathEscape(collection)+"/"+url.PathEscape(id), data, p, opts)
}

// Delete issues DELETE /{collection}/{id}. On a trash-enabled collection this
// is a HARD delete (verified); §12.4's soft delete is Trash.
//
// Hard-deleting an already-trashed document requires trash=true, otherwise
// Payload answers 404.
func (c *Client) Delete(ctx context.Context, collection, id string, p query.Params, opts ...Option) (*WriteResult, error) {
	return c.writeDoc(ctx, http.MethodDelete, "/"+url.PathEscape(collection)+"/"+url.PathEscape(id), nil, p, opts)
}

// Trash implements §12.4's soft delete: PATCH {"deletedAt": now}. The instant
// is supplied by the caller so this package never reads a clock.
func (c *Client) Trash(ctx context.Context, collection, id, deletedAt string, p query.Params, opts ...Option) (*WriteResult, error) {
	if deletedAt == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "a soft delete needs the deletedAt instant")
	}
	return c.Update(ctx, collection, id, map[string]any{"deletedAt": deletedAt}, p, opts...)
}

// Restore un-trashes a document: PATCH ?trash=true {"deletedAt": null}. The
// null is exactly why `data` is sent as JSON and not bracket notation.
func (c *Client) Restore(ctx context.Context, collection, id string, p query.Params, opts ...Option) (*WriteResult, error) {
	p = p.Clone()
	p.Trash = query.BoolPtr(true)
	return c.Update(ctx, collection, id, map[string]any{"deletedAt": nil}, p, opts...)
}

// Duplicate issues POST /{collection}/{id}/duplicate.
//
// This is only safe for an id that casts to the collection's id_type: a
// non-castable id CREATES a document (verified), so callers must pre-validate
// the id with apierr.CheckID.
func (c *Client) Duplicate(ctx context.Context, collection, id string, p query.Params, opts ...Option) (*WriteResult, error) {
	return c.writeDoc(ctx, http.MethodPost, "/"+url.PathEscape(collection)+"/"+url.PathEscape(id)+"/duplicate", nil, p, opts)
}

func (c *Client) writeDoc(ctx context.Context, method, path string, data map[string]any, p query.Params, opts []Option) (*WriteResult, error) {
	q, err := p.Encode()
	if err != nil {
		return nil, err
	}
	req := &Request{Method: method, Path: path, Query: q}
	if data != nil {
		body, err := jsonBody(data)
		if err != nil {
			return nil, err
		}
		req.Body = body
		req.ContentType = "application/json"
	}
	applyOptions(req, opts)
	if req.Classify.SentPaths == nil && data != nil {
		req.Classify.SentPaths = SentPaths(data)
	}

	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, err
	}
	if out.Doc == nil {
		// A few endpoints answer with the document at the top level rather
		// than under `doc`; keep one shape for every caller.
		var doc Doc
		if json.Unmarshal(resp.Body, &doc) == nil {
			if _, hasDoc := doc["doc"]; !hasDoc && len(doc) > 0 {
				out.Doc = doc
			}
		}
	}
	return out, nil
}

// BulkPlan is the outcome of §12.3's phases 1 and 2.
type BulkPlan struct {
	// Count is the server's count for the where clause.
	Count int
	// IDs are the exact documents the write will touch.
	IDs []any
}

// PlanBulk implements §12.3 phases 1-3: count, enforce the blast-radius cap,
// then resolve the exact ids client-side. It is used for bulk PATCH exactly as
// for bulk DELETE, so both verbs touch precisely what --dry-run printed —
// bulk DELETE ignores `limit` entirely (verified: limit=1 deleted all 4).
func (c *Client) PlanBulk(ctx context.Context, collection string, where query.Where, maxDocs int, all bool, opts ...Option) (*BulkPlan, error) {
	if where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"a bulk write needs --where; Payload answers a missing one with 400 \"Missing 'where' query…\"").
			WithHint("pass --where 'field op value', or target one document by id")
	}
	if maxDocs <= 0 {
		maxDocs = DefaultMaxBulk
	}
	count, _, err := c.Count(ctx, collection, query.Params{Where: where}, opts...)
	if err != nil {
		return nil, err
	}
	if !all && count > maxDocs {
		return nil, apierr.New(apierr.CodeBulkLimitExceeded,
			"%d documents match, which exceeds --max-docs %d", count, maxDocs).
			WithHint("N=%d exceeds --max-docs %d; pass --all to write to everything matching, or narrow --where",
				count, maxDocs)
	}
	ids, err := c.ResolveIDs(ctx, collection, where, count, opts...)
	if err != nil {
		return nil, err
	}
	return &BulkPlan{Count: count, IDs: ids}, nil
}

// bulkResolvePageSize is the page size §12.3 phase 3 pages with. It is only a
// transport detail: the resolution always walks every match.
const bulkResolvePageSize = 200

// ResolveIDsScoped is §12.3 phase 3 with the write's OWN read scope.
//
// ResolveIDs takes a bare where clause, which silently resolves a DIFFERENT
// population than the one phase 1 counted whenever the write is scoped by
// anything else: `delete --permanent` sends trash=true (so its count covers
// live + trashed while a where-only resolution can never see a trashed
// document), and `update --trash` addresses soft-deleted documents that a
// where-only find excludes entirely. All three phases must address the same
// documents, so the scope is passed through rather than re-derived.
//
// p is the write's params. Only the paging/projection fields this function
// owns are overwritten (select, depth, limit, sort, page); where, trash, draft
// and locale are preserved. Extra is dropped: it carries write-only switches
// (autosave, overrideLock) that have no meaning on a GET.
func (c *Client) ResolveIDsScoped(ctx context.Context, collection string, p query.Params, count int, opts ...Option) ([]any, error) {
	if p.Where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"a bulk operation needs a --where clause; Payload's own bulk verbs reject a missing one with HTTP 400")
	}
	scope := p.Clone()
	scope.Select = []string{"id"}
	scope.Depth = query.IntPtr(0)
	scope.Limit = query.IntPtr(bulkResolvePageSize)
	scope.Sort = []string{"id"}
	scope.Page = 0
	scope.Extra = nil
	scope.SelectExclude = nil
	scope.Populate = nil
	scope.Joins = nil
	scope.Data = nil

	var ids []any
	err := c.FindPages(ctx, collection, scope, count, func(l *ListResult) error {
		ids = append(ids, l.IDs()...)
		return nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// UpdateByIDs issues chunked PATCH ?where={"id":{"in":[…]}} requests.
//
// No server-side `limit` is ever sent on a bulk write, even though bulk PATCH
// honours it: relying on that asymmetry would make PATCH and DELETE behave
// differently for the same flags.
func (c *Client) UpdateByIDs(ctx context.Context, collection string, ids []any, data map[string]any, p query.Params, opts ...Option) (*BulkResult, error) {
	body, err := jsonBody(data)
	if err != nil {
		return nil, err
	}
	return c.bulk(ctx, http.MethodPatch, collection, ids, body, "application/json", p, data, opts)
}

// DeleteByIDs issues chunked DELETE ?where={"id":{"in":[…]}} requests.
func (c *Client) DeleteByIDs(ctx context.Context, collection string, ids []any, p query.Params, opts ...Option) (*BulkResult, error) {
	return c.bulk(ctx, http.MethodDelete, collection, ids, nil, "", p, nil, opts)
}

// DeleteWhereUnsafe is --unsafe-passthrough-where: the raw server semantics,
// in which DELETE ignores `limit` and removes every match. It exists so the
// escape hatch is explicit and greppable, never the default path.
func (c *Client) DeleteWhereUnsafe(ctx context.Context, collection string, where query.Where, p query.Params, opts ...Option) (*BulkResult, error) {
	if where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired, "--unsafe-passthrough-where still needs a --where clause")
	}
	p = p.Clone()
	p.Where = where
	p.Limit = nil
	return c.bulkRequest(ctx, http.MethodDelete, collection, nil, "", p, nil, opts)
}

func (c *Client) bulk(ctx context.Context, method, collection string, ids []any, body []byte, contentType string, p query.Params, data map[string]any, opts []Option) (*BulkResult, error) {
	if len(ids) == 0 {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"no documents matched, so no bulk %s was sent", method).
			WithHint("`pay find` the same --where first to see what would be affected")
	}
	combined := &BulkResult{}
	var firstErr error
	chunk := 0
	for start := 0; start < len(ids); start += BulkChunk {
		end := start + BulkChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk++
		combined.Chunks = chunk
		chunkParams := p.Clone()
		chunkParams.Where = query.Term("id", query.OpIn, ids[start:end])
		chunkParams.Limit = nil // never send a limit on a bulk write (§12.3)
		res, err := c.bulkRequest(ctx, method, collection, body, contentType, chunkParams, data, opts)
		// The response kept for error.http/error.raw is LATCHED to the first
		// failing chunk: keeping the last one would hand the caller a
		// different — possibly successful — request as the diagnostic for a
		// failure that happened earlier in the id set. errors[] counts as a
		// failure even under HTTP 200, because that is how a bulk verb reports
		// per-document rejections.
		if res != nil {
			combined.Docs = append(combined.Docs, res.Docs...)
			combined.Errors = append(combined.Errors, res.Errors...)
			if combined.Message == "" {
				combined.Message = res.Message
			}
			failed := err != nil || len(res.Errors) > 0
			if combined.FailedChunk == 0 {
				combined.HTTP = res.HTTP
				combined.Raw = res.Raw
				if failed {
					combined.FailedChunk = chunk
				}
			}
		} else if err != nil && combined.FailedChunk == 0 {
			combined.FailedChunk = chunk
			combined.HTTP = nil
			combined.Raw = nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return combined, firstErr
}

func (c *Client) bulkRequest(ctx context.Context, method, collection string, body []byte, contentType string, p query.Params, data map[string]any, opts []Option) (*BulkResult, error) {
	q, err := p.Encode()
	if err != nil {
		return nil, err
	}
	req := &Request{
		Method:      method,
		Path:        "/" + url.PathEscape(collection),
		Query:       q,
		Body:        body,
		ContentType: contentType,
		// §6.2: the override is never used for a bulk write.
		NoOverride: true,
	}
	applyOptions(req, opts)
	if req.Classify.SentPaths == nil && data != nil {
		req.Classify.SentPaths = SentPaths(data)
	}

	resp, doErr := c.Do(ctx, req)
	if resp == nil {
		return nil, doErr
	}
	out := &BulkResult{Raw: resp.Body, HTTP: resp}
	// Parse the body even on a 400: a bulk verb reports committed documents in
	// docs[] alongside the failures in errors[].
	if len(resp.Body) > 0 && requireJSON(resp) == nil {
		_ = json.Unmarshal(resp.Body, out)
	}
	return out, doErr
}
