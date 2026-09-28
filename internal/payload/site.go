package payload

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// SiteRequest is one GET against a route of the SITE rather than the REST API
// — today only F9's draft-preview handshake (`/next/preview?path=…&
// previewSecret=…` in the Payload website template).
//
// Such a route answers with a redirect whose Set-Cookie headers ARE the
// result, so the request is never allowed to follow it, and the caller — not
// §11.2's Payload error classifier — decides what a status means: a 403 from a
// Next.js route handler is "the preview secret or the user was refused", not
// Payload's access_denied.
type SiteRequest struct {
	// URL is the absolute http(s) URL to request.
	URL string
	// Secrets are literal values the URL carries. They never appear in the
	// result, in an error or in a log line, in any encoding.
	Secrets []string
}

// SiteResult is what a site route answered.
type SiteResult struct {
	// Status is the HTTP status, 3xx included.
	Status int
	// Location is the redirect target, scrubbed of the request's secrets.
	Location string
	// Cookies are the response's Set-Cookie headers, parsed. Their values are
	// credentials (a Next.js __prerender_bypass cookie IS draft access) and
	// must never be printed.
	Cookies []*http.Cookie
	// CrossOrigin reports that the URL's origin differs from base_url's, so
	// the request was sent without the Authorization header and without the
	// profile's headers (see Request.URL).
	CrossOrigin bool
	// BodyExcerpt is at most 200 characters of the body, scrubbed, for an
	// error message.
	BodyExcerpt string
	// HTTP is the underlying response; HTTP.URL is already scrubbed.
	HTTP *Response
}

// SiteGet sends one GET to a site route and returns the response WITHOUT
// following a redirect and without classifying the status. It returns an error
// only when no response arrived at all (DNS, TLS, timeout, a refused
// connection).
//
// The profile's credential is attached when, and only when, the URL shares
// base_url's scheme://host[:port]: the Payload website template's preview
// route authenticates the caller with payload.auth() over the same headers the
// REST API reads, and Payload 3 serves the site and the API from one Next.js
// app, so the same origin is the normal case.
func (c *Client) SiteGet(ctx context.Context, sr SiteRequest) (*SiteResult, error) {
	u, err := url.Parse(strings.TrimSpace(sr.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, apierr.New(apierr.CodeInvalidOption,
			"%q is not an absolute http(s) URL", redact.URLWithSecrets(sr.URL, sr.Secrets))
	}
	req := &Request{
		Method:     http.MethodGet,
		URL:        u.String(),
		Accept:     "text/html,application/xhtml+xml,*/*;q=0.8",
		NoRedirect: true,
		// The handshake URL carries a secret in its query; it must never be
		// rewritten into a method-override POST body (§6.2).
		NoOverride: true,
		Secrets:    sr.Secrets,
	}
	plan, err := c.plan(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, req, plan)
	if resp == nil {
		if err == nil {
			err = apierr.New(apierr.CodeInternal, "the site request produced neither a response nor an error")
		}
		return nil, err
	}
	out := &SiteResult{
		Status:      resp.Status,
		Cookies:     (&http.Response{Header: resp.Header}).Cookies(),
		CrossOrigin: c.crossOrigin(req),
		HTTP:        resp,
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		out.Location = redact.URLWithSecrets(loc, sr.Secrets)
	}
	out.BodyExcerpt = siteExcerpt(resp.Body, sr.Secrets)
	// A body cut short after the status arrived is still a usable answer
	// here: the status and the headers are what the caller needs.
	return out, nil
}

// crossOrigin reports whether a Request.URL points at a different origin than
// base_url. Scheme is part of the origin, so an https base_url and an http site
// URL on the same host are cross-origin: the credential is never sent in
// plaintext.
func (c *Client) crossOrigin(req *Request) bool {
	if req == nil || req.URL == "" {
		return false
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return true
	}
	return !SameOrigin(u, c.base)
}

// SameOrigin compares scheme and host[:port] the way a browser does, treating
// an explicit default port as equal to an omitted one.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// siteExcerpt keeps an error message short and free of the request's secrets.
func siteExcerpt(body []byte, secrets []string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	text = redact.Scrubber{Literals: secrets}.Text(text)
	text = strings.Join(strings.Fields(text), " ")
	const max = 200
	if r := []rune(text); len(r) > max {
		text = string(r[:max]) + "…"
	}
	return text
}
