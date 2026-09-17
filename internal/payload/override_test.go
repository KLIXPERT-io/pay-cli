package payload

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func TestOverrideContentTypeIsExact(t *testing.T) {
	// Payload's check is `=== 'application/x-www-form-urlencoded'`. Verified
	// live: the same request with `; charset=utf-8` made the server discard
	// the body and return its default 10 documents instead of the 2 asked for.
	if FormURLEncoded != "application/x-www-form-urlencoded" {
		t.Fatalf("the override Content-Type drifted: %q", FormURLEncoded)
	}
	if HeaderMethodOverride != "X-Payload-HTTP-Method-Override" {
		t.Fatalf("the override header drifted: %q", HeaderMethodOverride)
	}
	if URLBudget != 8000 {
		t.Fatalf("the URL budget drifted: %d", URLBudget)
	}
}

func TestAssertOverride(t *testing.T) {
	tests := []struct {
		name string
		plan requestPlan
		ok   bool
	}{
		{"valid", requestPlan{method: http.MethodPost, override: http.MethodGet, contentType: FormURLEncoded}, true},
		{"charset suffix", requestPlan{method: http.MethodPost, override: http.MethodGet, contentType: FormURLEncoded + "; charset=utf-8"}, false},
		{"json content type", requestPlan{method: http.MethodPost, override: http.MethodGet, contentType: "application/json"}, false},
		{"patch", requestPlan{method: http.MethodPatch, override: http.MethodGet, contentType: FormURLEncoded}, false},
		{"delete", requestPlan{method: http.MethodDelete, override: http.MethodGet, contentType: FormURLEncoded}, false},
		{"override delete", requestPlan{method: http.MethodPost, override: http.MethodDelete, contentType: FormURLEncoded}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.plan
			err := assertOverride(&plan)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, wantOK=%v", err, tc.ok)
			}
		})
	}
}

func TestPromotionIsRefusedForWrites(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodDelete, http.MethodPost, http.MethodPut} {
		plan := &requestPlan{method: method, url: "http://h/api/pages?" + strings.Repeat("x", URLBudget)}
		if canPromote(plan, &Request{Method: method}) {
			t.Fatalf("%s must never use the read-method override", method)
		}
		err := promotePlan(plan, &Request{Method: method})
		if !apierr.HasCode(err, apierr.CodeRequestTooLarge) {
			t.Fatalf("%s: err = %v, want request_too_large", method, err)
		}
	}
}

func TestLongGetIsPromotedBeforeSending(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[],"totalDocs":0}`)
	})
	c, _ := newTestClient(t, s, nil)

	ids := make([]any, 3000)
	for i := range ids {
		ids[i] = i
	}
	p := query.Params{Where: query.Term("id", query.OpIn, ids)}
	q, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(q) <= URLBudget {
		t.Fatalf("the test query is only %d bytes; it must exceed the budget", len(q))
	}

	resp, err := c.Do(context.Background(), &Request{Path: "/pages", Query: q})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.Promoted {
		t.Fatal("the request was not promoted")
	}
	seen := s.last()
	if seen.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", seen.Method)
	}
	if seen.RawQuery != "" {
		t.Fatalf("the query string must move into the body; got %q", seen.RawQuery)
	}
	if got := seen.Header.Get("Content-Type"); got != FormURLEncoded {
		t.Fatalf("Content-Type = %q, want exactly %q", got, FormURLEncoded)
	}
	if got := seen.Header.Get(HeaderMethodOverride); got != http.MethodGet {
		t.Fatalf("%s = %q, want GET", HeaderMethodOverride, got)
	}
	if seen.Body != q {
		t.Fatalf("the body is not the encoded query string")
	}
	// The body must still decode to the same where clause.
	values, parseErr := url.ParseQuery(seen.Body)
	if parseErr != nil || values.Get("where") == "" {
		t.Fatalf("the promoted body is not a form-encoded query: %v", parseErr)
	}
}

func TestLengthRejectionTriggersThePromotionOnce(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(431)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[]}`)
	})
	c, slept := newTestClient(t, s, nil)
	resp, err := c.Do(context.Background(), &Request{Path: "/pages", Query: "limit=2"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if s.count() != 2 {
		t.Fatalf("made %d requests, want 2 (the original plus the promoted rewrite)", s.count())
	}
	if !resp.Promoted {
		t.Fatal("the retry was not the promoted form")
	}
	if len(*slept) != 0 {
		t.Fatalf("the rewrite must not back off: %v", *slept)
	}
	if c.Budget().Remaining() != int64(DefaultMaxRetries*DefaultConcurrency) {
		t.Fatalf("the rewrite consumed retry budget: %d left", c.Budget().Remaining())
	}
	if got := s.last().Header.Get(HeaderMethodOverride); got != http.MethodGet {
		t.Fatalf("the rewrite did not set the override header: %q", got)
	}
}

func TestNoOverrideOptOut(t *testing.T) {
	s := newStub(t, jsonHandler(431, ``))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Do(context.Background(), &Request{Path: "/pages", Query: "limit=2", NoOverride: true})
	if err == nil {
		t.Fatal("want an error")
	}
	if s.count() != 1 {
		t.Fatalf("made %d requests, want 1", s.count())
	}
	if !apierr.HasCode(err, apierr.CodeRequestTooLarge) {
		t.Fatalf("err = %v, want request_too_large", err)
	}
}

func TestPromotedRequestStaysRetriable(t *testing.T) {
	// §6.1 lists POST with X-Payload-HTTP-Method-Override: GET as
	// idempotency-safe, so a promoted read may still be retried.
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n <= 2 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[]}`)
	})
	c, _ := newTestClient(t, s, nil)
	plan := &requestPlan{method: http.MethodGet, url: "http://h/api/pages?x=1", readOnly: true, retriable: true}
	if err := promotePlan(plan, &Request{Method: http.MethodGet}); err != nil {
		t.Fatalf("promotePlan: %v", err)
	}
	if !plan.retriable || !plan.readOnly {
		t.Fatal("a promoted read must stay retriable")
	}
	if plan.method != http.MethodPost || plan.override != http.MethodGet {
		t.Fatalf("plan = %+v", plan)
	}
	_ = c
}
