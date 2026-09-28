package payload

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/logging"
)

// previewSecretFixture is long enough for the literal scrubber (8+ chars) and
// deliberately contains characters that change under query and path escaping.
const previewSecretFixture = "s3cr3t/with space+plus"

// redirectWithCookie is the Payload website template's /next/preview answer:
// draftMode().enable() sets __prerender_bypass, then redirect(path) is a 307.
func redirectWithCookie(w http.ResponseWriter, r *http.Request, _ int) {
	http.SetCookie(w, &http.Cookie{Name: "__prerender_bypass", Value: "bypass-value-123",
		Path: "/", HttpOnly: true, SameSite: http.SameSiteNoneMode, Secure: true})
	w.Header().Set("Location", r.URL.Query().Get("path"))
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func previewURL(base, secret string) string {
	return base + "/next/preview?path=" + url.QueryEscape("/about") + "&previewSecret=" + url.QueryEscape(secret)
}

// The handshake's result IS the redirect's Set-Cookie, so the redirect must
// come back to the caller instead of being followed (a followed redirect
// would discard the cookie and fetch the page instead).
func TestSiteGetDoesNotFollowTheRedirectAndReturnsItsCookies(t *testing.T) {
	var followed int
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/about" {
			followed++
			w.WriteHeader(http.StatusOK)
			return
		}
		redirectWithCookie(w, r, n)
	})
	c, _ := newTestClient(t, s, nil)
	res, err := c.SiteGet(context.Background(), SiteRequest{
		URL: previewURL(s.URL, previewSecretFixture), Secrets: []string{previewSecretFixture},
	})
	if err != nil {
		t.Fatalf("SiteGet: %v", err)
	}
	if followed != 0 {
		t.Fatalf("the redirect was followed %d time(s)", followed)
	}
	if res.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", res.Status)
	}
	if res.Location != "/about" {
		t.Errorf("location = %q", res.Location)
	}
	if len(res.Cookies) != 1 || res.Cookies[0].Name != "__prerender_bypass" || res.Cookies[0].Value != "bypass-value-123" {
		t.Fatalf("cookies = %+v", res.Cookies)
	}
	if !res.Cookies[0].HttpOnly || res.Cookies[0].SameSite != http.SameSiteNoneMode {
		t.Errorf("cookie attributes were lost: %+v", res.Cookies[0])
	}
	if res.CrossOrigin {
		t.Error("a same-origin site URL was reported cross-origin")
	}
	// Same origin: the preview route authenticates with payload.auth() over
	// the same headers the REST API reads, so the credential goes along.
	if got := s.last().Header.Get(HeaderAuthorization); got != "users API-Key test-key-0123456789" {
		t.Errorf("Authorization = %q, want the profile credential on the same origin", got)
	}
	if strings.Contains(res.HTTP.URL, "s3cr3t") {
		t.Errorf("Response.URL leaks the secret: %s", res.HTTP.URL)
	}
}

// A redirect is not an error, and neither is a 403: a Next.js route handler's
// 403 is the preview route refusing, not Payload's access_denied, so the
// status is handed back unclassified for the caller to interpret.
func TestSiteGetNeverClassifiesTheStatus(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "You are not allowed to preview this page; secret was "+previewSecretFixture)
	})
	c, _ := newTestClient(t, s, nil)
	res, err := c.SiteGet(context.Background(), SiteRequest{
		URL: previewURL(s.URL, previewSecretFixture), Secrets: []string{previewSecretFixture},
	})
	if err != nil {
		t.Fatalf("a 403 must not be an error here: %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Fatalf("status = %d", res.Status)
	}
	if !strings.Contains(res.BodyExcerpt, "not allowed to preview") {
		t.Errorf("excerpt = %q", res.BodyExcerpt)
	}
	if strings.Contains(res.BodyExcerpt, previewSecretFixture) {
		t.Errorf("the excerpt echoes the secret: %q", res.BodyExcerpt)
	}
}

// §6's rule, applied to site URLs: a credential never crosses an origin. A
// site_url on another host (or the same host over plain http when base_url is
// https) gets neither the Authorization header nor the profile's headers.
func TestSiteGetWithholdsCredentialsAcrossOrigins(t *testing.T) {
	site := newStub(t, redirectWithCookie)
	api := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, api, func(cfg *Config) {
		cfg.Headers = map[string]string{"X-Vercel-Protection-Bypass": "vercel-secret-0123"}
	})
	res, err := c.SiteGet(context.Background(), SiteRequest{URL: previewURL(site.URL, previewSecretFixture)})
	if err != nil {
		t.Fatalf("SiteGet: %v", err)
	}
	if !res.CrossOrigin {
		t.Fatal("a different port must be a different origin")
	}
	h := site.last().Header
	if got := h.Get(HeaderAuthorization); got != "" {
		t.Errorf("Authorization crossed an origin: %q", got)
	}
	if got := h.Get("X-Vercel-Protection-Bypass"); got != "" {
		t.Errorf("a profile header crossed an origin: %q", got)
	}
	if got := h.Get(HeaderAcceptLanguage); got != "en" {
		t.Errorf("the standard headers must still be sent: Accept-Language = %q", got)
	}
}

// The -v debug line and Response.URL must never carry the secret, even when a
// custom preview_path puts it somewhere the query-name rule cannot see (a path
// segment, or a parameter not called *secret*).
func TestSiteGetScrubsTheSecretFromEveryURLItShows(t *testing.T) {
	s := newStub(t, redirectWithCookie)
	c, buf := debugClient(t, s, logging.Options{})
	raw := s.URL + "/preview/" + url.PathEscape(previewSecretFixture) + "?path=%2Fabout&k=" + url.QueryEscape(previewSecretFixture)
	res, err := c.SiteGet(context.Background(), SiteRequest{URL: raw, Secrets: []string{previewSecretFixture}})
	if err != nil {
		t.Fatalf("SiteGet: %v", err)
	}
	for where, text := range map[string]string{"debug log": buf.String(), "Response.URL": res.HTTP.URL} {
		for _, form := range []string{previewSecretFixture, url.QueryEscape(previewSecretFixture), url.PathEscape(previewSecretFixture)} {
			if strings.Contains(text, form) {
				t.Errorf("%s contains the secret (%q):\n%s", where, form, text)
			}
		}
	}
	if !strings.Contains(buf.String(), "redacted") {
		t.Errorf("the debug line shows no mask at all:\n%s", buf.String())
	}
}

func TestSiteGetRejectsARelativeURL(t *testing.T) {
	s := newStub(t, redirectWithCookie)
	c, _ := newTestClient(t, s, nil)
	if _, err := c.SiteGet(context.Background(), SiteRequest{URL: "/next/preview"}); err == nil {
		t.Fatal("a relative URL must be refused")
	}
	if s.count() != 0 {
		t.Fatal("a refused URL still reached the network")
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://x.dev", "https://x.dev/api", true},
		{"https://x.dev:443", "https://x.dev", true},
		{"http://x.dev", "https://x.dev", false},
		{"https://www.x.dev", "https://x.dev", false},
		{"http://localhost:3900", "http://localhost:3000", false},
		{"http://LOCALHOST:3900", "http://localhost:3900/cms", true},
	}
	for _, tc := range cases {
		a, _ := url.Parse(tc.a)
		b, _ := url.Parse(tc.b)
		if got := SameOrigin(a, b); got != tc.want {
			t.Errorf("SameOrigin(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// An ordinary API request must keep following same-host redirects: NoRedirect
// is per request, never ambient.
func TestNoRedirectIsPerRequest(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/api/pages", http.StatusPermanentRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[]}`)
	})
	c, _ := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/old", Absolute: true})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 200 || s.count() != 2 {
		t.Fatalf("status = %d after %d request(s); the 308 was not followed", resp.Status, s.count())
	}
}
