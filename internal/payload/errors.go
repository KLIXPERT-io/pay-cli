package payload

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// ClassifyContext is the per-request context §11.2 and §11.5 need to fold a
// Payload error body into exactly one code. Every field is a fact PayCLI
// already knows; none of them is a translated string.
type ClassifyContext struct {
	// Collection is the target slug, used in messages.
	Collection string
	// UploadCollection is the manifest's flags.upload for that slug. A 400 on
	// an upload collection with no errors[0].name is file_missing — the
	// structural signal, not the English "No files were uploaded.".
	UploadCollection bool
	// SentPaths is the leaf-path set PayCLI actually sent, which decides
	// error.fields[].sent and therefore which validation hint is shown
	// (Payload re-validates the whole document on PATCH).
	SentPaths map[string]bool
	// IncludeRaw is false for --no-raw; NoRedact is true for --no-redact.
	IncludeRaw bool
	NoRedact   bool
}

// classify turns a non-2xx response into the one error type.
func (c *Client) classify(req *Request, resp *Response) *apierr.Error {
	ctx := req.Classify
	return apierr.Classify(apierr.Response{
		Status:           resp.Status,
		Method:           resp.Method,
		URL:              resp.URL,
		ContentType:      resp.ContentType,
		Body:             resp.Body,
		Attempts:         resp.Attempts,
		RetryAfter:       resp.Header.Get(HeaderRetryAfter),
		AuthMode:         c.cfg.AuthMode,
		IdentityVerified: c.cfg.IdentityVerified,
		UploadCollection: ctx.UploadCollection,
		Collection:       ctx.Collection,
		SentPaths:        ctx.SentPaths,
		IncludeRaw:       ctx.IncludeRaw,
		NoRedact:         ctx.NoRedact,
	})
}

// ErrorFor exposes the classifier for a response a caller obtained itself
// (a discovery probe that wants the code for a status it already has).
func (c *Client) ErrorFor(req *Request, resp *Response) *apierr.Error {
	if resp == nil || resp.OK() {
		return nil
	}
	return c.classify(req, resp)
}

// decodeJSON unmarshals a 2xx body into v, refusing a non-JSON body before
// json.Unmarshal can produce a useless `invalid character '<'` (§11.5).
func decodeJSON(resp *Response, v any) error {
	if err := requireJSON(resp); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber() // ids and counts must round-trip verbatim, never via float64
	if err := dec.Decode(v); err != nil {
		return apierr.Wrap(err, apierr.CodeNonJSONResponse,
			"the server returned a body PayCLI could not parse as JSON").
			WithHTTP(&apierr.HTTP{Status: resp.Status, Method: resp.Method, URL: resp.URL,
				BodyExcerpt: excerpt(resp.Body)})
	}
	return nil
}

func requireJSON(resp *Response) error {
	ct := strings.ToLower(strings.TrimSpace(resp.ContentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/json" || strings.HasSuffix(ct, "+json") {
		return nil
	}
	if ct == "" && looksLikeJSON(resp.Body) {
		// Some proxies strip Content-Type on a 304-shaped response; trust the
		// bytes when they are unambiguous.
		return nil
	}
	e := apierr.New(apierr.CodeNonJSONResponse,
		"the server answered %s with %s, not JSON", resp.Method, describeContentType(resp.ContentType)).
		WithHTTP(&apierr.HTTP{Status: resp.Status, Method: resp.Method, URL: resp.URL,
			BodyExcerpt: excerpt(resp.Body)})
	if bytes.Contains(resp.Body, []byte(`id="__next_error__"`)) {
		return e.WithHint("Next.js's error boundary answered, not Payload: the path is outside " +
			"routes.api. Check --base-url and --api-path")
	}
	return e
}

func looksLikeJSON(b []byte) bool {
	t := bytes.TrimLeft(b, " \t\r\n")
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

func describeContentType(ct string) string {
	if strings.TrimSpace(ct) == "" {
		return "no Content-Type"
	}
	return ct
}

func excerpt(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// jsonBody marshals a request body without HTML escaping, so a `&` in a
// document value survives the round trip.
func jsonBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, apierr.Wrap(err, apierr.CodeBadRequestBody, "the request body could not be encoded as JSON")
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SentPaths returns the leaf-path set of a document body, which the error
// classifier uses to mark which invalid fields the caller actually sent.
func SentPaths(data map[string]any) map[string]bool {
	out := map[string]bool{}
	collectPaths("", data, out)
	return out
}

func collectPaths(prefix string, v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			out[path] = true
			collectPaths(path, child, out)
		}
	case []any:
		for _, child := range t {
			collectPaths(prefix, child, out)
		}
	}
}
