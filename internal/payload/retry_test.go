package payload

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestClassifyAttemptStatuses(t *testing.T) {
	read := &requestPlan{method: http.MethodGet, readOnly: true, retriable: true}
	write := &requestPlan{method: http.MethodPatch}

	tests := []struct {
		name   string
		plan   *requestPlan
		status int
		header http.Header
		want   decisionKind
	}{
		{"502 on a read", read, 502, nil, retryBackoff},
		{"503 on a read", read, 503, nil, retryBackoff},
		{"504 on a read", read, 504, nil, retryBackoff},
		{"429 on a read", read, 429, http.Header{HeaderRetryAfter: {"7"}}, retryBackoff},
		// 500 is verified deterministic (uncastable id, /versions on a
		// non-versioned collection, dangling relationship): retrying it just
		// costs another round trip.
		{"500 is never retried", read, 500, nil, stop},
		{"400 is never retried", read, 400, nil, stop},
		{"401 is never retried", read, 401, nil, stop},
		{"403 is never retried", read, 403, nil, stop},
		{"404 is never retried", read, 404, nil, stop},
		{"409 is never retried", read, 409, nil, stop},
		{"423 is never retried here", read, 423, nil, stop},
		{"501 is never retried", read, 501, nil, stop},
		{"200 stops", read, 200, nil, stop},
		{"413 promotes a read", read, 413, nil, retryPromote},
		{"414 promotes a read", read, 414, nil, retryPromote},
		{"431 promotes a read", read, 431, nil, retryPromote},
		// A write is never retried once any response byte has been read:
		// Payload has no idempotency key, so a retried POST duplicates a doc.
		{"502 on a write", write, 502, nil, stop},
		{"429 on a write", write, 429, http.Header{HeaderRetryAfter: {"1"}}, stop},
		{"413 never promotes a write", write, 413, nil, stop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.header
			if h == nil {
				h = http.Header{}
			}
			got := classifyAttempt(tc.plan, &Response{Status: tc.status, Header: h}, nil, false)
			if got.kind != tc.want {
				t.Fatalf("kind = %v, want %v", got.kind, tc.want)
			}
		})
	}
}

func TestClassifyAttemptErrors(t *testing.T) {
	read := &requestPlan{method: http.MethodGet, readOnly: true, retriable: true}
	write := &requestPlan{method: http.MethodPost}
	dial := &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}

	tests := []struct {
		name string
		plan *requestPlan
		err  error
		want decisionKind
	}{
		{"idle connection is a free retry", write, errors.New("http: server closed idle connection"), retryFree},
		{"dns failure on a read", read, &net.DNSError{Err: "no such host", IsNotFound: true}, retryBackoff},
		{"connection reset on a read", read, errors.New("read: connection reset by peer"), retryBackoff},
		{"unexpected EOF on a read", read, io.ErrUnexpectedEOF, retryBackoff},
		{"x509 is never retried", read, errors.New("x509: certificate signed by unknown authority"), stop},
		{"SIGINT is never retried", read, context.Canceled, stop},
		{"pre-flight dial failure retries a write", write, dial, retryBackoff},
		{"a mid-flight error never retries a write", write, io.ErrUnexpectedEOF, stop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAttempt(tc.plan, nil, tc.err, false)
			if got.kind != tc.want {
				t.Fatalf("kind = %v, want %v", got.kind, tc.want)
			}
		})
	}
}

func TestIdleConnectionRetryIsFreeOnlyOnce(t *testing.T) {
	read := &requestPlan{method: http.MethodGet, readOnly: true, retriable: true}
	write := &requestPlan{method: http.MethodPost}
	idle := errors.New("http: server closed idle connection")

	if got := classifyAttempt(read, nil, idle, false); got.kind != retryFree {
		t.Fatalf("first idle failure = %v, want retryFree", got.kind)
	}
	// Once the free reconnect is spent the normal rules apply: a read backs
	// off, a write is still safe because the request never left the client.
	if got := classifyAttempt(read, nil, idle, true); got.kind != retryBackoff {
		t.Fatalf("second idle failure on a read = %v, want retryBackoff", got.kind)
	}
	if got := classifyAttempt(write, nil, idle, true); got.kind != retryBackoff {
		t.Fatalf("second idle failure on a write = %v, want retryBackoff", got.kind)
	}
}

func TestDecorrelatedJitter(t *testing.T) {
	p := RetryPolicy{Base: RetryBase, Cap: RetryCap, Rand: func() float64 { return 1 }}
	if got := p.next(0); got != RetryBase {
		t.Fatalf("sleep0 = %v, want %v", got, RetryBase)
	}
	// sleepn = min(cap, rand(base, prev*3)); with rand()==1 it is the top of
	// the range.
	if got := p.next(250 * time.Millisecond); got != 750*time.Millisecond {
		t.Fatalf("sleep1 = %v, want 750ms", got)
	}
	if got := p.next(5 * time.Second); got != RetryCap {
		t.Fatalf("the cap was exceeded: %v", got)
	}
	low := RetryPolicy{Base: RetryBase, Cap: RetryCap, Rand: func() float64 { return 0 }}
	if got := low.next(2 * time.Second); got != RetryBase {
		t.Fatalf("the floor was breached: %v", got)
	}
	for i := 0; i < 200; i++ {
		got := RetryPolicy{Base: RetryBase, Cap: RetryCap, Rand: cryptoFloat}.next(3 * time.Second)
		if got < RetryBase || got > RetryCap {
			t.Fatalf("sleep %v outside [%v, %v]", got, RetryBase, RetryCap)
		}
	}
}

func TestBudgetIsSharedAndAtomic(t *testing.T) {
	b := NewBudget(10)
	var wg sync.WaitGroup
	var taken int64
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Take() {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if taken != 10 {
		t.Fatalf("budget handed out %d retries, want 10", taken)
	}
	if b.Remaining() != 0 {
		t.Fatalf("remaining = %d", b.Remaining())
	}
	var nilBudget *Budget
	if !nilBudget.Take() {
		t.Fatal("a nil budget must not block")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"7", 7 * time.Second, true},
		{"0", 0, true},
		{"-3", 0, false},
		{"99999", RetryAfterCap, true},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"not-a-date", 0, false},
	}
	for _, tc := range tests {
		got, ok := ParseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := ParseRetryAfter(now.Format(http.TimeFormat), time.Time{}); ok {
		t.Error("without a clock the HTTP-date form must be unusable")
	}
}

func TestRetriesAreBoundedToFourAttempts(t *testing.T) {
	s := newStub(t, jsonHandler(503, `{"errors":[{"message":"nope"}]}`))
	c, slept := newTestClient(t, s, func(cfg *Config) { cfg.MaxRetries = 10 })
	_, err := c.Do(context.Background(), &Request{Path: "/pages"})
	if err == nil {
		t.Fatal("want an error")
	}
	if s.count() != DefaultMaxAttempts {
		t.Fatalf("made %d attempts, want %d", s.count(), DefaultMaxAttempts)
	}
	if len(*slept) != DefaultMaxAttempts-1 {
		t.Fatalf("slept %d times, want %d", len(*slept), DefaultMaxAttempts-1)
	}
	if c.Stats().Retries != int64(DefaultMaxAttempts-1) {
		t.Fatalf("Retries = %d", c.Stats().Retries)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set(HeaderRetryAfter, "9")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"errors":[{"message":"slow down"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[]}`)
	})
	c, slept := newTestClient(t, s, nil)
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != 9*time.Second {
		t.Fatalf("slept %v, want one 9s wait", *slept)
	}
}

func TestFiveHundredIsNeverRetried(t *testing.T) {
	s := newStub(t, jsonHandler(500, `{"errors":[{"message":"Something went wrong."}]}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Path: "/pages/abc"})
	if !apierr.HasCode(err, apierr.CodeServerError) {
		t.Fatalf("err = %v, want server_error", err)
	}
	if s.count() != 1 {
		t.Fatalf("a 500 was retried %d times", s.count()-1)
	}
}

func TestWriteIsNotRetriedAfterAResponse(t *testing.T) {
	s := newStub(t, jsonHandler(503, `{"errors":[{"message":"gateway"}]}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Method: http.MethodPost, Path: "/pages", Body: []byte(`{}`), ContentType: "application/json"})
	if err == nil {
		t.Fatal("want an error")
	}
	if s.count() != 1 {
		t.Fatalf("a write was retried %d times; Payload has no idempotency key", s.count()-1)
	}
}

func TestSharedBudgetStopsRetries(t *testing.T) {
	s := newStub(t, jsonHandler(503, `{}`))
	budget := NewBudget(1)
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Budget = budget })
	_, _ = c.Do(context.Background(), &Request{Path: "/pages"})
	first := s.count()
	if first != 2 {
		t.Fatalf("first call made %d attempts, want 2 (one retry from the budget)", first)
	}
	_, _ = c.Do(context.Background(), &Request{Path: "/pages"})
	if s.count()-first != 1 {
		t.Fatalf("the exhausted budget still allowed %d attempts", s.count()-first)
	}
}

func TestRetrySkippedWhenItWouldOutliveTheDeadline(t *testing.T) {
	// The wall-clock deadline is authoritative over the attempt count (§6.1):
	// a retry whose sleep would exceed the remaining budget is skipped.
	s := newStub(t, jsonHandler(503, `{}`))
	clock := newClock()
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Now = clock.Now })

	ctx, cancel := context.WithDeadline(context.Background(), clock.now.Add(time.Second))
	defer cancel()
	if c.fits(ctx, 5*time.Second) {
		t.Fatal("a 5s sleep must not fit inside a 1s deadline")
	}
	if !c.fits(ctx, 10*time.Millisecond) {
		t.Fatal("a 10ms sleep must fit inside a 1s deadline")
	}
	if !c.fits(context.Background(), time.Hour) {
		t.Fatal("without a deadline every sleep fits")
	}
}
