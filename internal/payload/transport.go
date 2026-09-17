package payload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Header names PayCLI sets or reads.
const (
	HeaderRequestID      = "X-Request-Id"
	HeaderAcceptLanguage = "Accept-Language"
	HeaderAuthorization  = "Authorization"
	HeaderPoweredBy      = "X-Powered-By"
	HeaderRetryAfter     = "Retry-After"
)

// maxRedirects is §6's CheckRedirect budget.
const maxRedirects = 3

// newHTTPClient builds the one transport described in §6.
func newHTTPClient(cfg Config) *http.Client {
	rt := cfg.RoundTripper
	if rt == nil {
		rt = &http.Transport{
			Proxy: http.ProxyFromEnvironment, // §3.1: every transport honours the proxy env
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32, // Go's default of 2 silently serialises every fan-out
			MaxConnsPerHost:       cfg.Concurrency,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}, //nolint:gosec // opt-in via --insecure-skip-verify
		}
	}
	return &http.Client{
		Transport: rt,
		// No Client.Timeout: ResponseHeaderTimeout above bounds a stalled
		// server while letting a slow-but-progressing download finish (§6).
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect implements §6: at most 3 hops, same host, and never an
// https -> http downgrade. A host change aborts rather than silently dropping
// the Authorization header, because a redirect to another host that then
// answers 200 would look like success.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return apierr.New(apierr.CodeNetworkUnreachable,
			"the server redirected more than %d times", maxRedirects).
			WithHint("check base_url — a redirect loop usually means the URL is missing or has an extra path segment")
	}
	first := via[0]
	prev := via[len(via)-1]
	// url.URL.Host carries no scheme and omits an implicit port, so
	// https://cms.example.com and http://cms.example.com have the *same* Host:
	// the host check below cannot see a downgrade. Go's own
	// shouldCopyHeaderOnRedirect compares hostnames only too, so without this
	// the API key would be re-sent in cleartext on the next hop. Checking prev
	// as well as first closes https -> http -> http.
	if isHTTPS(first.URL.Scheme) || isHTTPS(prev.URL.Scheme) {
		if !isHTTPS(req.URL.Scheme) {
			req.Header.Del(HeaderAuthorization)
			return apierr.New(apierr.CodeBaseURLInvalid,
				"the server redirected from https to %s (%s -> %s); PayCLI will not forward credentials over plaintext",
				req.URL.Scheme, redact.URL(prev.URL.String()), redact.URL(req.URL.String())).
				WithHint("point --base-url at an https endpoint, or fix the server's redirect")
		}
	}
	if !sameHost(first.URL.Host, req.URL.Host) {
		req.Header.Del(HeaderAuthorization)
		return apierr.New(apierr.CodeBaseURLInvalid,
			"the server redirected to a different host (%s -> %s); PayCLI will not forward credentials across hosts",
			first.URL.Hostname(), req.URL.Hostname()).
			WithHint("point --base-url at the final host directly")
	}
	return nil
}

// isHTTPS is scheme comparison the way RFC 3986 requires it: case-insensitive.
// An http -> https *upgrade* is deliberately still allowed; a proxy that
// upgrades a plaintext base_url is a normal, safe setup.
func isHTTPS(scheme string) bool { return strings.EqualFold(scheme, "https") }

func sameHost(a, b string) bool { return strings.EqualFold(a, b) }

// requestPlan is the fully resolved wire form of a Request: after this point
// nothing else decides a method, a URL or a Content-Type.
type requestPlan struct {
	method      string
	url         string
	body        []byte
	contentType string
	// override is the value of X-Payload-HTTP-Method-Override, set only by
	// §6.2's promotion.
	override  string
	promoted  bool
	multipart *Multipart
	readOnly  bool
	retriable bool
}

// plan resolves a Request into its wire form, including §6.2's promotion.
func (c *Client) plan(req *Request) (*requestPlan, error) {
	if req.Body != nil && req.Multipart != nil {
		return nil, apierr.New(apierr.CodeInternal, "a request cannot have both a body and a multipart body")
	}
	p := &requestPlan{
		method:      strings.ToUpper(req.Method),
		url:         c.URLFor(req),
		body:        req.Body,
		contentType: req.ContentType,
		multipart:   req.Multipart,
		readOnly:    isReadOnly(req),
	}
	if err := promoteIfTooLong(p, req); err != nil {
		return nil, err
	}
	p.retriable = planIsRetriable(p)
	return p, nil
}

// isReadOnly implements §6.1's idempotency predicate at the request level.
func isReadOnly(req *Request) bool {
	switch strings.ToUpper(req.Method) {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		return req.ReadOnly
	default:
		return false
	}
}

func planIsRetriable(p *requestPlan) bool {
	if p.multipart != nil {
		// A streaming body cannot be replayed; never retry it.
		return false
	}
	return p.readOnly
}

// send runs the attempt loop.
func (c *Client) send(ctx context.Context, req *Request, plan *requestPlan) (*Response, error) {
	policy := c.policy()
	started := c.now()

	var (
		last      *Response
		lastErr   error
		sleep     time.Duration
		attempts  int
		retries   int
		freeRetry = true // §6: one zero-backoff retry for a dead pooled connection
	)

	for attempts < policy.MaxAttempts {
		attempts++
		resp, err := c.attempt(ctx, req, plan, attempts)
		atomic.AddInt64(&c.state.stats.Requests, 1)
		if resp != nil {
			resp.Attempts = attempts
			resp.Retries = retries
			resp.Duration = c.since(started)
			atomic.AddInt64(&c.state.stats.Bytes, resp.Bytes)
		}
		// A non-nil error alongside a non-nil response means the body was cut
		// short mid-read; both are carried so the caller sees what arrived.
		last, lastErr = resp, err

		decision := classifyAttempt(plan, resp, err, !freeRetry)
		switch decision.kind {
		case retryFree:
			freeRetry = false
			retries++
			atomic.AddInt64(&c.state.stats.Retries, 1)
			c.log.Debug("retrying a closed idle connection with no backoff",
				logging.URL("url", plan.url), "attempt", attempts)
			continue
		case retryPromote:
			// §6.2: a 413/414/431 triggers the method-override fallback once.
			// It is a rewrite, not a retry, so it costs no retry budget.
			if err := promotePlan(plan, req); err != nil {
				return c.finish(last, lastErr, attempts, retries, started)
			}
			c.log.Debug("promoting to a method-override POST after a header/URI length rejection",
				logging.URL("url", plan.url))
			continue
		case retryBackoff:
			if attempts >= policy.MaxAttempts || !c.budget.Take() {
				return c.finish(last, lastErr, attempts, retries, started)
			}
			wait := decision.after
			if wait <= 0 {
				sleep = policy.next(sleep)
				wait = sleep
			} else {
				sleep = wait
			}
			if !c.fits(ctx, wait) {
				// The wall-clock deadline is authoritative over the attempt
				// count (§6.1): a sleep that would outlive it is skipped.
				return c.finish(last, lastErr, attempts, retries, started)
			}
			retries++
			atomic.AddInt64(&c.state.stats.Retries, 1)
			c.log.Debug("retrying",
				logging.URL("url", plan.url),
				"attempt", attempts,
				"sleep_ms", wait.Milliseconds())
			if err := c.sleep(ctx, wait); err != nil {
				return c.finish(last, lastErr, attempts, retries, started)
			}
			continue
		default:
			return c.finish(last, lastErr, attempts, retries, started)
		}
	}
	return c.finish(last, lastErr, attempts, retries, started)
}

func (c *Client) finish(resp *Response, err error, attempts, retries int, started time.Time) (*Response, error) {
	if resp != nil {
		resp.Attempts = attempts
		resp.Retries = retries
		resp.Duration = c.since(started)
		return resp, err
	}
	if err == nil {
		err = apierr.New(apierr.CodeInternal, "the request produced neither a response nor an error")
	}
	return nil, err
}

// attempt performs exactly one round trip and fully drains the body.
//
// attemptNo is 1-based and is threaded down from send() purely so the §6 debug
// line can report which attempt produced the status (attemptNo-1 is the retry
// count at that point).
func (c *Client) attempt(ctx context.Context, req *Request, plan *requestPlan, attemptNo int) (out *Response, err error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = c.cfg.Timeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	requestID := c.newRequestID()
	c.state.lastRequestID.Store(requestID)
	httpReq, err := c.buildHTTPRequest(attemptCtx, req, plan, requestID)
	if err != nil {
		return nil, err
	}

	// §6: exactly one debug line per attempt, on every return path — this is
	// what `-v` / `--log-level debug` turns on. Named results are what make one
	// deferred call cover the four returns below.
	started := c.now()
	defer func() { c.logRequest(req, plan, requestID, attemptNo, c.since(started), out, err) }()

	httpResp, doErr := c.httpc.Do(httpReq)
	if doErr != nil {
		return nil, c.transportError(doErr, plan, requestID)
	}
	defer func() {
		// Drain so the connection can go back to the pool (§6) — but never
		// without a bound. Go's transport gunzips transparently, so an
		// unbounded drain is what turns a 1 MB gzip bomb into ~1 GiB of
		// inflation *after* the buffered read already stopped at the cap.
		// Closing short deliberately keeps that connection out of the idle
		// pool, which is the right outcome for a hostile body.
		_, _ = io.CopyN(io.Discard, httpResp.Body, maxDrainBytes)
		_ = httpResp.Body.Close()
	}()

	out = &Response{
		Status:      httpResp.StatusCode,
		Header:      httpResp.Header.Clone(),
		ContentType: httpResp.Header.Get("Content-Type"),
		Method:      plan.method,
		URL:         redact.URL(plan.url),
		RequestID:   requestID,
		Promoted:    plan.promoted,
	}

	if req.Sink != nil && out.OK() {
		n, copyErr := io.Copy(req.Sink, httpResp.Body)
		out.Bytes = n
		if copyErr != nil {
			return out, apierr.Wrap(copyErr, apierr.CodeNetworkUnreachable,
				"the response body was cut short after %d bytes", n)
		}
		return out, nil
	}

	// MaxBodyBytes bounds the *decompressed* body: no Accept-Encoding is set
	// here and Transport.DisableCompression is unset, so Go gunzips for us and
	// a ~1 MB gzip response can expand far past the cap. Reading one byte past
	// it is what tells a complete body from a bomb — io.LimitReader alone
	// hands back a silently shortened body that then parses as a smaller,
	// wrong document (or as `ok: true` with no data at all).
	limit := c.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, limit+1))
	// Bytes stays the number of bytes that arrived, even when the retained
	// slice below is shorter: meta.bytes is then still the truthful wire size
	// next to a deliberately short error.raw.
	out.Bytes = int64(len(body))
	oversized := int64(len(body)) > limit
	switch {
	case oversized:
		// Keep only enough to diagnose with. error.raw and `--output raw`
		// inline this slice verbatim, and tens of MB of attacker-controlled
		// bytes on the stdout an agent is parsing is the real damage of a
		// decompression bomb (§11.1's byte-faithful rule applies to a body
		// PayCLI actually accepted, not to one it refused).
		body = retainBody(body, fmt.Sprintf(
			"the body exceeded the %d-byte buffer cap and was not read to the end; at most %d bytes are kept",
			limit, maxRetainedBodyBytes))
	case !out.OK() && len(body) > maxRetainedBodyBytes && !parsesAsJSON(out.ContentType, body):
		// The cap above only fires on a body that OVERFLOWED MaxBodyBytes
		// (64 MiB), so every error body under it — a 20 MiB uncompressed
		// Next.js error page, say — was still inlined whole into error.raw and
		// into `--output raw`, i.e. onto the stdout an LLM agent has to read.
		// That cap is therefore applied here too, independently of the
		// gzip-bomb path, and retainBody's notice is what tells the agent that
		// what it is reading is a prefix.
		//
		// An unparseable error body is exactly the case where truncating costs
		// nothing: §11.5 reduces it to a 200-character body_excerpt and a code
		// derived from the status, so every byte past this cap is noise that
		// only an agent's context window pays for. A body that DOES parse is
		// left whole on purpose — the classifier reads errors[0].name for
		// §11.2, and a bulk 400 carries the committed documents in docs[]
		// alongside the failures (§12.5), so a truncated JSON error would
		// silently demote a precise partial_failure to a by-status guess.
		// Bounding what apierr then INLINES as error.raw is that layer's job
		// (§11.1), not the transport's.
		body = retainBody(body, fmt.Sprintf(
			"this %s error body was %d bytes and is not parseable JSON; PayCLI keeps at most %d bytes of one",
			describeContentType(out.ContentType), len(body), maxRetainedBodyBytes))
	}
	out.Body = body
	if readErr != nil {
		return out, apierr.Wrap(readErr, apierr.CodeNetworkUnreachable, "the response body could not be read")
	}
	if oversized {
		// Never return a truncated body as if it were whole: the caller would
		// decode a prefix, or hand it to the classifier as the server's error.
		size := fmt.Sprintf("%d MiB", limit>>20)
		if limit < 1<<20 {
			size = fmt.Sprintf("%d bytes", limit)
		}
		tooLarge := apierr.New(apierr.CodeNetworkUnreachable,
			"the response body is larger than the %s PayCLI will buffer and was not read to the end", size).
			WithHTTP(&apierr.HTTP{Status: out.Status, Method: out.Method, URL: out.URL}).
			WithHint("narrow the request (--limit, --select, --depth), or stream the bytes with `pay download`")
		// Retrying re-downloads the same oversized (or deliberately
		// compressed) body, so this one is not worth another attempt.
		tooLarge.Retriable = false
		return out, tooLarge
	}
	return out, nil
}

// Response-body bounds (§6, finding 17).
const (
	// maxDrainBytes is how much of an unwanted body is drained to keep the
	// connection reusable. Anything past it is not worth inflating.
	maxDrainBytes = 1 << 20
	// maxRetainedBodyBytes is how much of an over-cap or error body survives
	// into Response.Body, and therefore into error.raw and `--output raw`.
	maxRetainedBodyBytes = 64 << 10
)

// retainBody cuts a body PayCLI will not carry whole down to
// maxRetainedBodyBytes and appends why. The notice travels inside the bytes on
// purpose: these bytes ARE the envelope's error.raw (and `--output raw`'s
// stdout), so an agent that reads a prefix and no notice would treat a
// truncated error page — or a truncated JSON error document that now fails to
// parse — as the server's complete answer.
func retainBody(body []byte, why string) []byte {
	budget := maxRetainedBodyBytes
	if budget > len(body) {
		budget = len(body) // a caller may have set a cap smaller than this one
	}
	notice := "\n\n[pay: response body truncated — " + why + "]"
	keep := budget - len(notice)
	if keep <= 0 {
		// The retained slice is the budget: an explanation that pushed the
		// result back over it would defeat the cap it is explaining.
		return body[:budget:budget]
	}
	kept := trimPartialRune(body[:keep])
	out := make([]byte, 0, len(kept)+len(notice))
	out = append(out, kept...)
	return append(out, notice...)
}

// parsesAsJSON reports whether the body is a JSON document PayCLI's classifier
// can still read. The Content-Type gate mirrors requireJSON: a proxy that
// strips the header on an otherwise unambiguous JSON body is common enough
// that the bytes get the benefit of the doubt.
func parsesAsJSON(contentType string, body []byte) bool {
	// requireJSON is the one Content-Type predicate in this package (§11.5);
	// it is reused rather than restated so the two can never drift.
	if !isJSONResponse(&Response{ContentType: contentType, Body: body}) {
		return false
	}
	return json.Valid(body)
}

// trimPartialRune drops a multi-byte rune the cut landed inside, so the
// retained text never ends in bytes that are not valid UTF-8 on their own.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// buildHTTPRequest is the ONLY place in PayCLI where an *http.Request is
// constructed (§3.1, arch-lint rule c). Accept-Language, the request id and
// the Authorization header therefore cannot be bypassed.
func (c *Client) buildHTTPRequest(ctx context.Context, req *Request, plan *requestPlan, requestID string) (*http.Request, error) {
	var (
		httpReq *http.Request
		err     error
	)
	switch {
	case plan.multipart != nil:
		boundary := plan.multipart.Boundary
		if boundary == "" {
			boundary, err = randomBoundary()
			if err != nil {
				return nil, err
			}
		}
		pr, pw := io.Pipe()
		write := plan.multipart.Write
		go func() {
			werr := write(pw, boundary)
			_ = pw.CloseWithError(werr)
		}()
		httpReq, err = http.NewRequestWithContext(ctx, plan.method, plan.url, pr)
		if err == nil {
			plan.contentType = "multipart/form-data; boundary=" + boundary
		}
	case plan.body != nil:
		httpReq, err = http.NewRequestWithContext(ctx, plan.method, plan.url, bytes.NewReader(plan.body))
		if err == nil {
			body := plan.body
			httpReq.ContentLength = int64(len(body))
			httpReq.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	default:
		httpReq, err = http.NewRequestWithContext(ctx, plan.method, plan.url, nil)
	}
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInvalidArgs, "the request could not be built")
	}

	for k, vs := range req.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	for k, v := range c.cfg.Headers {
		httpReq.Header.Set(k, v)
	}

	httpReq.Header.Set("User-Agent", c.userAgent())
	httpReq.Header.Set(HeaderRequestID, requestID)
	// §6: pinning `en` keeps every English-only signal PayCLI still uses
	// behaving the same on every project. Classification never depends on it.
	httpReq.Header.Set(HeaderAcceptLanguage, c.cfg.AcceptLanguage)
	if accept := req.Accept; accept != "" {
		httpReq.Header.Set("Accept", accept)
	} else if req.Sink == nil {
		httpReq.Header.Set("Accept", "application/json")
	}
	if plan.contentType != "" {
		httpReq.Header.Set("Content-Type", plan.contentType)
	}
	if plan.override != "" {
		if err := assertOverride(plan); err != nil {
			return nil, err
		}
		httpReq.Header.Set(HeaderMethodOverride, plan.override)
	}
	if auth := c.authorization(req); auth != "" {
		httpReq.Header.Set(HeaderAuthorization, auth)
	}
	return httpReq, nil
}

// authorization builds the Authorization header for the request's auth mode
// (§5.0). The returned string is never logged: transport.go is also the only
// place it exists.
func (c *Client) authorization(req *Request) string {
	if req.NoAuth || c.cfg.Credential == "" {
		return ""
	}
	switch c.cfg.AuthMode {
	case AuthModeAPIKey:
		coll := resolvedAuthCollection(c.cfg.AuthCollection)
		if req.AuthCollectionOverride != "" {
			coll = resolvedAuthCollection(req.AuthCollectionOverride)
		}
		if coll == "" {
			// The slug is still the §7.0 placeholder. Sending
			// `Authorization: auto API-Key <key>` would hand the credential to
			// a collection that does not exist and silently return the
			// anonymous view, so no header is sent at all.
			return ""
		}
		return coll + " API-Key " + c.cfg.Credential
	case AuthModeJWT:
		return c.cfg.AuthScheme + " " + c.cfg.Credential
	default:
		return ""
	}
}

func (c *Client) userAgent() string {
	if c.cfg.UserAgent != "" {
		return c.cfg.UserAgent
	}
	info := buildinfo.Current()
	return fmt.Sprintf("pay/%s (%s/%s)", info.Version, info.OS, info.Arch)
}

// transportError maps a net/http failure onto a §11.4 network code, keeping
// the cause in the chain.
func (c *Client) transportError(err error, plan *requestPlan, requestID string) error {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		// CheckRedirect's own refusal already is a classified error.
		return apiErr
	}
	e := apierr.Network(err)
	return e.WithHTTP(&apierr.HTTP{
		Method: plan.method,
		URL:    plan.url,
	}).WithHint("%s", e.Hint+" (request id "+requestID+")")
}

// newRequestID returns a ULID-shaped, monotonically increasing id. When no
// clock is injected the timestamp half is replaced by a process-local counter,
// so ids stay unique and sortable without this package ever reading a clock.
func (c *Client) newRequestID() string {
	if c.cfg.RequestID != nil {
		return c.cfg.RequestID()
	}
	var ts uint64
	if c.cfg.Now != nil {
		ts = uint64(c.cfg.Now().UTC().UnixMilli())
	} else {
		c.state.reqIDMu.Lock()
		c.state.reqSeq++
		ts = uint64(c.state.reqSeq)
		c.state.reqIDMu.Unlock()
	}
	var entropy [10]byte
	_, _ = rand.Read(entropy[:])
	return encodeULID(ts, entropy)
}

// crockford is Crockford's base32 alphabet, as used by ULID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func encodeULID(ms uint64, entropy [10]byte) string {
	out := make([]byte, 26)
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1f]
		ms >>= 5
	}
	// 80 bits of entropy -> 16 base32 characters.
	var bits uint
	var acc uint32
	pos := 10
	for _, b := range entropy {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[pos] = crockford[(acc>>bits)&0x1f]
			pos++
		}
	}
	for pos < 26 {
		out[pos] = crockford[0]
		pos++
	}
	return string(out)
}

func randomBoundary() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", apierr.Wrap(err, apierr.CodeInternal, "cannot generate a multipart boundary")
	}
	return fmt.Sprintf("paycli%x", buf), nil
}

// now/since/sleep funnel every clock read through the injected Config so that
// §3.1's "only app.go reads the wall clock" rule holds for this package.
func (c *Client) now() time.Time {
	if c.cfg.Now == nil {
		return time.Time{}
	}
	return c.cfg.Now()
}

func (c *Client) since(start time.Time) time.Duration {
	if c.cfg.Now == nil || start.IsZero() {
		return 0
	}
	return c.cfg.Now().Sub(start)
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.cfg.Sleep != nil {
		return c.cfg.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// fits reports whether a sleep of d leaves the context deadline intact.
func (c *Client) fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok || c.cfg.Now == nil {
		return true
	}
	return c.cfg.Now().Add(d).Before(deadline)
}
