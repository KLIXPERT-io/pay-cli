package payload

import (
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// Multipart part names (§13). Both are verified: `-F upload=@…` answers 400
// "No files were uploaded.", and a JSON body with a `url` key answers the same
// — the URL form is an admin-client feature, not a REST one.
const (
	UploadPartFile   = "file"
	UploadPartFields = "_payload"
)

// DefaultMaxUploadSize is --max-size's default (§13).
const DefaultMaxUploadSize int64 = 100 << 20

// UploadInput describes one multipart upload.
type UploadInput struct {
	Collection string
	// Filename is the name sent in the Content-Disposition. Payload
	// auto-renames on a collision (paycli-probe.png -> paycli-probe-1.png), so
	// the caller must read the returned filename rather than assume this one.
	Filename string
	// ContentType is the part's own Content-Type, sniffed or --content-type.
	ContentType string
	// Body streams the bytes. It is never buffered whole.
	Body io.Reader
	// Fields are the sibling document fields, already merged per §9.10.2.
	// They travel as a JSON string in the _payload part.
	Fields map[string]any
	// ReplaceID turns the upload into PATCH /{coll}/{id}. Note that Payload
	// mints a NEW filename rather than overwriting in place, so any hardcoded
	// URL to the old file breaks.
	ReplaceID string

	Params query.Params
}

// CheckUploadCollection is §13's preflight. It follows the tri-state rule: a
// nil isUpload means the manifest never learned the fact, so the request is
// sent rather than rejected on a guess.
func CheckUploadCollection(collection string, isUpload *bool) error {
	if isUpload == nil || *isUpload {
		return nil
	}
	return apierr.New(apierr.CodeNotUploadCollection,
		"%s is not an upload collection, so it has no file field", collection).
		WithHint("`pay collections --capability upload` lists the collections that accept a file")
}

// Upload builds the multipart body of §13 and sends it.
func (c *Client) Upload(ctx context.Context, in UploadInput, opts ...Option) (*WriteResult, error) {
	if in.Collection == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "no target collection was named")
	}
	if in.Body == nil {
		return nil, apierr.New(apierr.CodeFileMissing, "no file bytes were supplied")
	}
	if strings.TrimSpace(in.Filename) == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "the upload needs a filename").
			WithHint("pass --filename NAME (it is required when the bytes come from stdin)")
	}

	var fieldsJSON []byte
	if len(in.Fields) > 0 {
		b, err := jsonBody(in.Fields)
		if err != nil {
			return nil, err
		}
		fieldsJSON = b
	}

	q, err := in.Params.Encode()
	if err != nil {
		return nil, err
	}

	method := http.MethodPost
	path := "/" + url.PathEscape(in.Collection)
	if in.ReplaceID != "" {
		method = http.MethodPatch
		path += "/" + url.PathEscape(in.ReplaceID)
	}

	req := &Request{
		Method: method,
		Path:   path,
		Query:  q,
		Multipart: &Multipart{
			Write: func(w io.Writer, boundary string) error {
				return writeUploadParts(w, boundary, in, fieldsJSON)
			},
		},
		Timeout:    c.cfg.UploadTimeout,
		NoOverride: true,
	}
	applyOptions(req, opts)
	req.Classify.Collection = in.Collection
	req.Classify.UploadCollection = true
	if req.Classify.SentPaths == nil && in.Fields != nil {
		req.Classify.SentPaths = SentPaths(in.Fields)
	}

	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{Raw: resp.Body, HTTP: resp}
	if err := decodeJSON(resp, out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeUploadParts(w io.Writer, boundary string, in UploadInput, fieldsJSON []byte) error {
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "invalid multipart boundary")
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition",
		`form-data; name="`+UploadPartFile+`"; filename="`+escapeQuotes(in.Filename)+`"`)
	if in.ContentType != "" {
		header.Set("Content-Type", in.ContentType)
	} else {
		header.Set("Content-Type", "application/octet-stream")
	}
	part, err := mw.CreatePart(header)
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot start the file part")
	}
	if _, err := io.Copy(part, in.Body); err != nil {
		return apierr.Wrap(err, apierr.CodeFileMissing, "the file could not be read")
	}
	if len(fieldsJSON) > 0 {
		if err := mw.WriteField(UploadPartFields, string(fieldsJSON)); err != nil {
			return apierr.Wrap(err, apierr.CodeInternal, "cannot write the _payload part")
		}
	}
	return mw.Close()
}

// escapeQuotes keeps a filename containing a quote or backslash from breaking
// the Content-Disposition header.
func escapeQuotes(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "")
	return r.Replace(s)
}

// LimitReader wraps an upload source with --max-size (default 100 MB),
// failing with request_too_large rather than streaming gigabytes at a dev
// server. A max of 0 or less means unlimited.
func LimitReader(r io.Reader, max int64) io.Reader {
	if max <= 0 {
		return r
	}
	return &limitedReader{r: r, max: max}
}

type limitedReader struct {
	r    io.Reader
	read int64
	max  int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.read += int64(n)
	if l.read > l.max {
		// Report only once the source actually produced more than the limit,
		// so a file of exactly --max-size bytes still uploads.
		return n, apierr.New(apierr.CodeRequestTooLarge,
			"the file exceeds the %d-byte --max-size limit", l.max).
			WithHint("raise --max-size, or upload the file to storage and reference it by URL")
	}
	return n, err
}

// FilenameChanged reports whether the server renamed the file, which it does
// silently on a collision.
func FilenameChanged(sent string, doc Doc) (string, bool) {
	got, _ := doc["filename"].(string)
	if got == "" || got == sent {
		return got, false
	}
	return got, true
}

// Download streams GET /{collection}/file/{filename} into w.
//
// This route is served unauthenticated in a stock project and answers 500 —
// not 404 — for a missing file, so the caller resolves sizes.<name>.filename
// from the document first rather than guessing a name.
func (c *Client) Download(ctx context.Context, collection, filename string, w io.Writer, opts ...Option) (*Response, error) {
	if w == nil {
		return nil, apierr.New(apierr.CodeInternal, "no download sink")
	}
	req := applyOptions(&Request{
		Method: http.MethodGet,
		Path:   "/" + url.PathEscape(collection) + "/file/" + url.PathEscape(filename),
		Accept: "*/*",
		Sink:   w,
	}, opts)
	req.Classify.Collection = collection
	resp, err := c.Do(ctx, req)
	if err != nil {
		return resp, err
	}
	return resp, nil
}
