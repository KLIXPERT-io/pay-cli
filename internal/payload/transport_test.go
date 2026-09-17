package payload

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestEveryRequestCarriesTheStandardHeaders(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	h := s.last().Header
	// Accept-Language: en is load-bearing, not cosmetic (§6): it pins the
	// server's translated strings so the English-only signals PayCLI still
	// uses behave the same on every project.
	if got := h.Get(HeaderAcceptLanguage); got != "en" {
		t.Errorf("Accept-Language = %q, want en", got)
	}
	if got := h.Get(HeaderRequestID); got == "" {
		t.Error("X-Request-Id was not sent")
	}
	if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "pay/") || !strings.Contains(ua, "(") {
		t.Errorf("User-Agent = %q, want pay/<version> (<os>/<arch>)", ua)
	}
	if got := h.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestAcceptLanguageIsOverridable(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.AcceptLanguage = "de" })
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := s.last().Header.Get(HeaderAcceptLanguage); got != "de" {
		t.Fatalf("Accept-Language = %q, want de", got)
	}
}

func TestAuthorizationHeaderPerMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(*Config)
		req  Request
		want string
	}{
		{
			"api-key embeds the auth collection slug",
			nil, Request{Path: "/pages"}, "users API-Key test-key-0123456789",
		},
		{
			"jwt defaults to the JWT scheme",
			func(c *Config) { c.AuthMode = AuthModeJWT; c.Credential = "tok" },
			Request{Path: "/pages"}, "JWT tok",
		},
		{
			"jwt honours --auth-header-scheme Bearer",
			func(c *Config) { c.AuthMode = AuthModeJWT; c.Credential = "tok"; c.AuthScheme = SchemeBearer },
			Request{Path: "/pages"}, "Bearer tok",
		},
		{
			"anonymous sends nothing",
			func(c *Config) { c.AuthMode = AuthModeAnonymous; c.Credential = "" },
			Request{Path: "/pages"}, "",
		},
		{
			"no-auth requests send nothing",
			nil, Request{Path: "/users/init", NoAuth: true}, "",
		},
		{
			"a candidate slug can be overridden per request",
			nil, Request{Path: "/admins/me", AuthCollectionOverride: "admins"},
			"admins API-Key test-key-0123456789",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(200, `{}`))
			c, _ := newTestClient(t, s, tc.cfg)
			req := tc.req
			if _, err := c.Do(context.Background(), &req); err != nil {
				t.Fatalf("Do: %v", err)
			}
			if got := s.last().Header.Get(HeaderAuthorization); got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProfileHeadersAreSentAndCannotOverrideAuthorization(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, func(cfg *Config) {
		cfg.Headers = map[string]string{"X-Tenant": "acme"}
	})
	req := &Request{Path: "/pages"}
	WithHeader(HeaderAuthorization, "Bearer sneaky")(req)
	WithHeader("X-Extra", "1")(req)
	if _, err := c.Do(context.Background(), req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	h := s.last().Header
	if h.Get("X-Tenant") != "acme" || h.Get("X-Extra") != "1" {
		t.Fatalf("extra headers missing: %v", h)
	}
	if got := h.Get(HeaderAuthorization); got != "users API-Key test-key-0123456789" {
		t.Fatalf("a call site overrode Authorization: %q", got)
	}
}

func TestRequestIDsAreUniqueAndSortable(t *testing.T) {
	c, err := New(Config{BaseURL: "http://h"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := map[string]bool{}
	var prev string
	for i := 0; i < 50; i++ {
		id := c.newRequestID()
		if len(id) != 26 {
			t.Fatalf("request id %q is not 26 characters", id)
		}
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
		if prev != "" && id[:10] < prev[:10] {
			t.Fatalf("request ids are not sortable: %s then %s", prev, id)
		}
		prev = id
	}
}

func TestNonJSONResponseIsNotParsed(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<!DOCTYPE html><html id="__next_error__">boom</html>`)
	})
	c, _ := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	var doc Doc
	err = decodeJSON(resp, &doc)
	if !apierr.HasCode(err, apierr.CodeNonJSONResponse) {
		t.Fatalf("err = %v, want non_json_response", err)
	}
	e, _ := apierr.As(err)
	if e == nil || !strings.Contains(e.Hint, "Next.js") {
		t.Fatalf("the Next.js error boundary was not named: %v", err)
	}
}

func TestRedirectSameHostIsFollowed(t *testing.T) {
	// Payload answers a trailing slash with a 308 to the canonical path
	// (verified live: GET /api/pages/ -> Location: /api/pages). The client
	// normalises the slash away itself, so this stub redirects a stale path
	// to exercise the same hop.
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/api/old" {
			http.Redirect(w, r, "/api/pages", http.StatusPermanentRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	c, _ := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/old"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("status = %d", resp.Status)
	}
	if s.count() != 2 {
		t.Fatalf("the 308 was not followed: %d requests", s.count())
	}
}

func TestRedirectToAnotherHostIsRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"stolen":true}`)
	}))
	defer other.Close()

	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, other.URL+"/api/pages", http.StatusTemporaryRedirect)
	})
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if !apierr.HasCode(err, apierr.CodeBaseURLInvalid) {
		t.Fatalf("err = %v, want base_url_invalid", err)
	}
	if !strings.Contains(err.Error(), "different host") {
		t.Fatalf("message = %v", err)
	}
}

func TestRedirectLoopIsBounded(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		http.Redirect(w, r, "/api/pages?n="+strings.Repeat("x", n), http.StatusTemporaryRedirect)
	})
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("a redirect loop must fail")
	}
	if s.count() > maxRedirects+1 {
		t.Fatalf("followed %d redirects, want at most %d", s.count(), maxRedirects)
	}
}

func TestSinkStreamsTheBody(t *testing.T) {
	payload := strings.Repeat("a", 4096)
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, payload)
	})
	c, _ := newTestClient(t, s, nil)
	var sink strings.Builder
	resp, err := c.Do(context.Background(), &Request{Path: "/media/file/x.png", Sink: &sink, Accept: "*/*"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sink.String() != payload {
		t.Fatalf("streamed %d bytes, want %d", sink.Len(), len(payload))
	}
	if len(resp.Body) != 0 {
		t.Fatal("a streamed response must not also buffer the body")
	}
	if resp.Bytes != int64(len(payload)) {
		t.Fatalf("Bytes = %d", resp.Bytes)
	}
}

func TestPerRequestTimeout(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	})
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxRetries = 1 })
	_, err := c.Do(context.Background(), &Request{Path: "/pages", Timeout: 20 * time.Millisecond})
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !apierr.HasCode(err, apierr.CodeTimeout) && !apierr.HasCode(err, apierr.CodeNetworkUnreachable) {
		t.Fatalf("err = %v (code %s), want a network-class error", err, apierr.CodeOf(err))
	}
}

func TestNonOKStatusIsClassified(t *testing.T) {
	s := newStub(t, jsonHandler(404, `{"errors":[{"message":"Not Found"}]}`))
	c, _ := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/pages/999"})
	if resp == nil {
		t.Fatal("the response must be returned alongside the error")
	}
	if !apierr.HasCode(err, apierr.CodeDocNotFound) {
		t.Fatalf("err = %v, want doc_not_found", err)
	}
	if apierr.ExitCode(err) != 4 {
		t.Fatalf("exit = %d, want 4", apierr.ExitCode(err))
	}
}

func TestResponseURLIsRedacted(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/pages", Query: "api_key=supersecretvalue&limit=2"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if strings.Contains(resp.URL, "supersecretvalue") {
		t.Fatalf("the response URL leaked a secret: %s", resp.URL)
	}
	if !strings.Contains(resp.URL, "limit=2") {
		t.Fatalf("redaction ate the useful parameters: %s", resp.URL)
	}
}

func TestTruncatedBodyIsReportedNotSilentlyAccepted(t *testing.T) {
	// A body cut short mid-read must not look like a successful short answer:
	// half a JSON document decodes to nothing useful.
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"docs":[{"id":1}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxRetries = 1 })
	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("a truncated body must surface as an error")
	}
	if resp == nil {
		t.Fatal("the partial response must still be returned")
	}
	if apierr.ExitCode(err) != 6 {
		t.Fatalf("exit = %d, want the network class", apierr.ExitCode(err))
	}
	if s.count() < 2 {
		t.Fatalf("a read cut short must be retried; made %d attempts", s.count())
	}
}

// --- finding 15: never forward the credential over plaintext ----------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// schemeClient talks to baseURL through rt, with a credential worth stealing.
func schemeClient(t *testing.T, baseURL string, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:        baseURL,
		APIPath:        "/api",
		AuthMode:       AuthModeAPIKey,
		AuthCollection: "users",
		Credential:     "test-key-0123456789",
		Now:            newClock().Now,
		Rand:           func() float64 { return 0.5 },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		RequestID:      func() string { return "01TESTTESTTESTTESTTESTTEST" },
		RoundTripper:   rt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestRedirectHTTPSToHTTPIsRefused is finding 15: url.URL.Host is identical for
// https://cms.example.com and http://cms.example.com (the implicit port is not
// in it), so the same-host check cannot see the downgrade — and Go copies the
// Authorization header across it because it compares hostnames only.
func TestRedirectHTTPSToHTTPIsRefused(t *testing.T) {
	var mu sync.Mutex
	var hops []*http.Request
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hops = append(hops, r.Clone(r.Context()))
		mu.Unlock()
		if r.URL.Scheme == "https" {
			return stubResponse(r, http.StatusFound,
				http.Header{"Location": {"http://cms.example.com/api/pages"}}, ""), nil
		}
		// The plaintext origin answers 200 so that a regression shows up as a
		// successful command rather than as a test that merely stops failing.
		return stubResponse(r, http.StatusOK,
			http.Header{"Content-Type": {"application/json"}}, `{"stolen":true}`), nil
	})

	c := schemeClient(t, "https://cms.example.com", rt)
	_, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if !apierr.HasCode(err, apierr.CodeBaseURLInvalid) {
		t.Fatalf("err = %v, want base_url_invalid", err)
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("the message must say why: %v", err)
	}
	if strings.Contains(err.Error(), "test-key-0123456789") {
		t.Errorf("the credential leaked into the error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hops) != 1 {
		t.Fatalf("%d hops, want 1 — the plaintext hop must never be attempted", len(hops))
	}
	for i, h := range hops {
		if h.URL.Scheme != "https" && h.Header.Get(HeaderAuthorization) != "" {
			t.Fatalf("hop %d (%s) carried the Authorization header in the clear", i, h.URL.Scheme)
		}
	}
}

// TestRedirectHTTPSToHTTPIsRefusedAfterAnUpgrade closes https -> http -> http:
// via[0] is plaintext there, so checking only the first hop would let it pass.
func TestRedirectHTTPSToHTTPIsRefusedAfterAnUpgrade(t *testing.T) {
	var mu sync.Mutex
	var plaintextWithAuth int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		if r.URL.Scheme == "http" && r.Header.Get(HeaderAuthorization) != "" {
			plaintextWithAuth++
		}
		mu.Unlock()
		switch {
		case r.URL.Scheme == "http" && r.URL.Path == "/api/pages":
			return stubResponse(r, http.StatusFound,
				http.Header{"Location": {"https://cms.example.com/api/pages"}}, ""), nil
		case r.URL.Scheme == "https":
			return stubResponse(r, http.StatusFound,
				http.Header{"Location": {"http://cms.example.com/api/leaked"}}, ""), nil
		default:
			return stubResponse(r, http.StatusOK,
				http.Header{"Content-Type": {"application/json"}}, `{"stolen":true}`), nil
		}
	})
	c := schemeClient(t, "http://cms.example.com", rt)
	_, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if !apierr.HasCode(err, apierr.CodeBaseURLInvalid) {
		t.Fatalf("err = %v, want base_url_invalid", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if plaintextWithAuth != 1 {
		// The one allowed plaintext request is the user's own base_url.
		t.Fatalf("%d plaintext requests carried the credential, want 1 (the initial one)", plaintextWithAuth)
	}
}

// TestRedirectHTTPToHTTPSIsFollowed: an upgrade is the common "base_url is http,
// the proxy redirects to https" setup and must keep working.
func TestRedirectHTTPToHTTPSIsFollowed(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "http" {
			return stubResponse(r, http.StatusMovedPermanently,
				http.Header{"Location": {"https://cms.example.com/api/pages"}}, ""), nil
		}
		if r.Header.Get(HeaderAuthorization) == "" {
			t.Error("the credential was dropped on an https upgrade")
		}
		return stubResponse(r, http.StatusOK,
			http.Header{"Content-Type": {"application/json"}}, `{"ok":true}`), nil
	})
	c := schemeClient(t, "http://cms.example.com", rt)
	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err != nil {
		t.Fatalf("an http -> https upgrade must still be followed: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("status = %d", resp.Status)
	}
}

// --- finding 17: decompression bomb -----------------------------------------

// bombHandler serves `total` bytes of highly compressible data as gzip,
// counting how much of it the server actually managed to produce: that count is
// the amplification an unbounded drain pays for.
func bombHandler(status int, contentType string, total int64, served *int64) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		zw := gzip.NewWriter(w)
		chunk := bytes.Repeat([]byte("A"), 1<<20)
		for sent := int64(0); sent < total; sent += int64(len(chunk)) {
			n, err := zw.Write(chunk)
			atomic.AddInt64(served, int64(n))
			if err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_ = zw.Close()
	}
}

func TestGzipBombIsCappedAndNotInflated(t *testing.T) {
	const (
		capBytes = 1 << 20 // a small MaxBodyBytes keeps the test quick
		bomb     = 512 << 20
		slack    = 32 << 20 // cap + drain + gzip/TCP buffering
	)
	var served int64
	s := newStub(t, bombHandler(http.StatusInternalServerError, "text/html", bomb, &served))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxBodyBytes = capBytes })

	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("an over-cap body must not be reported as a usable response")
	}
	if resp == nil {
		t.Fatal("the response is needed for diagnosis")
	}
	if int64(len(resp.Body)) > maxRetainedBodyBytes {
		t.Fatalf("Response.Body kept %d bytes; error.raw and --output raw inline it, so it must stay under %d",
			len(resp.Body), maxRetainedBodyBytes)
	}
	if got := atomic.LoadInt64(&served); got > slack {
		t.Fatalf("the client pulled %d decompressed bytes out of the bomb, want under %d", got, slack)
	}
}

func TestOversizedJSONIsAnErrorNotASilentTruncation(t *testing.T) {
	const capBytes = 1 << 20
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"docs":["`)
		chunk := strings.Repeat("x", 1<<16)
		for sent := 0; sent < 4*capBytes; sent += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"]}`)
	})
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxBodyBytes = capBytes })

	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("a 200 whose body exceeded the cap must be an error, not a short body")
	}
	if resp != nil && int64(len(resp.Body)) > maxRetainedBodyBytes {
		t.Fatalf("Response.Body kept %d bytes, want at most %d", len(resp.Body), maxRetainedBodyBytes)
	}
	e, _ := apierr.As(err)
	if e == nil || e.Hint == "" {
		t.Fatalf("the error must tell the agent what to do next: %v", err)
	}
}

// TestSinkIsTheOnlyUnboundedBodyPath: a download legitimately exceeds any
// buffered cap, so the streaming branch stays uncapped — and only it.
func TestSinkIsTheOnlyUnboundedBodyPath(t *testing.T) {
	const capBytes = 1 << 16
	body := strings.Repeat("a", 4*capBytes)
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, body)
	})
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxBodyBytes = capBytes })
	var sink strings.Builder
	resp, err := c.Do(context.Background(), &Request{Path: "/media/file/x.png", Sink: &sink, Accept: "*/*"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sink.String() != body {
		t.Fatalf("streamed %d bytes, want %d", sink.Len(), len(body))
	}
	if resp.Bytes != int64(len(body)) {
		t.Fatalf("resp.Bytes = %d, want %d", resp.Bytes, len(body))
	}
}

// TestTinyBodyCapDoesNotPanic: maxRetainedBodyBytes is an upper bound, not an
// assumption — MaxBodyBytes is configurable and may be smaller than it.
func TestTinyBodyCapDoesNotPanic(t *testing.T) {
	s := newStub(t, jsonHandler(200, strings.Repeat("x", 4096)))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.MaxBodyBytes = 16 })
	resp, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("a body over a 16-byte cap must be an error")
	}
	if resp == nil || len(resp.Body) > 17 {
		t.Fatalf("Response.Body = %d bytes", len(resp.Body))
	}
	if !strings.Contains(err.Error(), "16 bytes") {
		t.Errorf("the message must name the cap it hit: %v", err)
	}
}
