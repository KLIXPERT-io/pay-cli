package payload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Retry constants from §6.1.
const (
	RetryBase          = 250 * time.Millisecond
	RetryCap           = 8 * time.Second
	RetryAfterCap      = 120 * time.Second
	DefaultMaxAttempts = 4
)

// RetryPolicy is §6.1's decorrelated-jitter backoff.
type RetryPolicy struct {
	MaxAttempts   int
	Base          time.Duration
	Cap           time.Duration
	RetryAfterCap time.Duration
	Rand          func() float64
}

func (c *Client) policy() RetryPolicy {
	attempts := c.cfg.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	if attempts > DefaultMaxAttempts {
		// §6.1 caps the total at 4 attempts however generous --max-retries is;
		// a shared budget is the other half of that guarantee.
		attempts = DefaultMaxAttempts
	}
	return RetryPolicy{
		MaxAttempts:   attempts,
		Base:          RetryBase,
		Cap:           RetryCap,
		RetryAfterCap: RetryAfterCap,
		Rand:          c.cfg.Rand,
	}
}

// next implements sleep₀ = base, sleepₙ = min(cap, rand(base, prev×3)).
func (p RetryPolicy) next(prev time.Duration) time.Duration {
	base := p.Base
	if base <= 0 {
		base = RetryBase
	}
	capped := p.Cap
	if capped <= 0 {
		capped = RetryCap
	}
	if prev <= 0 {
		return base
	}
	high := prev * 3
	if high > capped {
		high = capped
	}
	if high <= base {
		return base
	}
	r := 0.5
	if p.Rand != nil {
		r = p.Rand()
	}
	span := float64(high - base)
	return base + time.Duration(r*span)
}

// Budget is the shared retry budget of §6.1: parallel operations draw from one
// pool of 3 × concurrency so a dead server cannot turn a 500-item batch into
// 1,500 doomed requests.
type Budget struct {
	remaining int64
}

// NewBudget creates a budget with n retries in it.
func NewBudget(n int) *Budget {
	if n < 0 {
		n = 0
	}
	return &Budget{remaining: int64(n)}
}

// Take consumes one retry, reporting whether any was left.
func (b *Budget) Take() bool {
	if b == nil {
		return true
	}
	for {
		cur := atomic.LoadInt64(&b.remaining)
		if cur <= 0 {
			return false
		}
		if atomic.CompareAndSwapInt64(&b.remaining, cur, cur-1) {
			return true
		}
	}
}

// Remaining reports the budget left, for diagnostics.
func (b *Budget) Remaining() int64 {
	if b == nil {
		return 0
	}
	return atomic.LoadInt64(&b.remaining)
}

type decisionKind int

const (
	stop decisionKind = iota
	// retryFree is the one free immediate retry for a connection the server
	// closed while it sat in our pool: the request never left the client, so
	// it costs no budget and is safe even for a write.
	retryFree
	retryBackoff
	// retryPromote rewrites the request through §6.2's method-override
	// fallback after a 413/414/431 and resends once.
	retryPromote
)

type decision struct {
	kind decisionKind
	// after is a server-dictated delay (Retry-After); zero means "use the
	// backoff schedule".
	after time.Duration
}

// classifyAttempt implements §6.1's table. It is a pure function of the plan,
// the response, the error and whether the one free reconnect has been used,
// which is what makes the table a unit test.
func classifyAttempt(plan *requestPlan, resp *Response, err error, freeUsed bool) decision {
	if err != nil {
		// A response alongside the error means the body was cut short after
		// the headers arrived; §6.1's write rule still applies, because bytes
		// of the response were already read.
		switch {
		case errors.Is(err, context.Canceled):
			return decision{kind: stop} // the user's SIGINT is never retried
		case isIdleConnError(err) && !freeUsed:
			return decision{kind: retryFree}
		case isFatalTLSError(err):
			return decision{kind: stop}
		case plan.retriable && isRetriableNetError(err):
			return decision{kind: retryBackoff}
		case !plan.retriable && isPreflightError(err):
			// A write is retried only on a proven pre-flight failure: Payload
			// has no idempotency key, so a retried POST duplicates the doc.
			return decision{kind: retryBackoff}
		default:
			return decision{kind: stop}
		}
	}
	if resp == nil {
		return decision{kind: stop}
	}

	switch resp.Status {
	case http.StatusRequestEntityTooLarge, http.StatusRequestURITooLong, http.StatusRequestHeaderFieldsTooLarge:
		if plan.readOnly && !plan.promoted && plan.multipart == nil {
			return decision{kind: retryPromote}
		}
		return decision{kind: stop}
	}

	// Once any response byte is read, a write is never retried.
	if !plan.retriable {
		return decision{kind: stop}
	}

	switch resp.Status {
	case http.StatusTooManyRequests:
		// Payload 3.x has no rate limiter, so a 429 always came from a
		// proxy/WAF — and that proxy's Retry-After is worth honouring.
		d, _ := ParseRetryAfter(resp.Header.Get(HeaderRetryAfter), time.Time{})
		return decision{kind: retryBackoff, after: d}
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		d, _ := ParseRetryAfter(resp.Header.Get(HeaderRetryAfter), time.Time{})
		return decision{kind: retryBackoff, after: d}
	case http.StatusInternalServerError:
		// NEVER retried: verified deterministic (uncastable id, /versions on a
		// non-versioned collection, a dangling relationship). §11.3 diagnoses
		// it instead.
		return decision{kind: stop}
	default:
		return decision{kind: stop}
	}
}

// ParseRetryAfter reads both Retry-After forms, capped at 120 s. `now` is
// passed in for the HTTP-date form; a zero `now` makes the date form
// unusable and falls back to the backoff schedule.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return capDuration(time.Duration(secs) * time.Second), true
	}
	if now.IsZero() {
		return 0, false
	}
	for _, layout := range []string{http.TimeFormat, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, v); err == nil {
			d := t.Sub(now)
			if d <= 0 {
				return 0, true
			}
			return capDuration(d), true
		}
	}
	return 0, false
}

func capDuration(d time.Duration) time.Duration {
	if d > RetryAfterCap {
		return RetryAfterCap
	}
	return d
}

// isIdleConnError matches the one error §6 grants a free immediate retry: the
// server advertises Keep-Alive: timeout=5, so a pooled connection idle for
// longer fails its first reuse before the request leaves the client.
func isIdleConnError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "server closed idle connection") ||
		strings.Contains(msg, "http2: no cached connection was available")
}

// isFatalTLSError matches the x509 failures §6.1 forbids retrying: a bad
// certificate will be exactly as bad on the next attempt.
func isFatalTLSError(err error) bool {
	if err == nil {
		return false
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var recordHeader tls.RecordHeaderError
	return errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &certInvalid) ||
		errors.As(err, &verification) ||
		errors.As(err, &recordHeader) ||
		strings.Contains(err.Error(), "x509:")
}

// isRetriableNetError lists §6.1's retriable network errors.
func isRetriableNetError(err error) bool {
	if err == nil || isFatalTLSError(err) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A per-attempt timeout: the next attempt gets a fresh one, bounded by
		// the command deadline.
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.Temporary() || dnsErr.IsTimeout || dnsErr.IsNotFound
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if isPreflightError(err) {
		return true
	}
	msg := err.Error()
	for _, frag := range []string{
		"connection reset by peer",
		"broken pipe",
		"GOAWAY",
		"unexpected EOF",
		"server closed idle connection",
		"connection refused",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// isPreflightError reports a failure proven to have happened before any byte
// of the request reached the server, which is the only condition under which
// §6.1 retries a write.
func isPreflightError(err error) bool {
	if err == nil || isFatalTLSError(err) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	if isIdleConnError(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "host is unreachable")
}
