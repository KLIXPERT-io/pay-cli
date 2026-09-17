package payload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// recorded is one request the stub server saw.
type recorded struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     string
}

// stub is a loopback Payload stand-in. Nothing in these tests touches the
// network beyond 127.0.0.1.
type stub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	handler  func(w http.ResponseWriter, r *http.Request, attempt int)
}

func newStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, attempt int)) *stub {
	t.Helper()
	s := &stub{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recorded{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     string(body),
		})
		n := len(s.requests)
		s.mu.Unlock()
		s.handler(w, r, n)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) seen() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

func (s *stub) last() recorded {
	all := s.seen()
	if len(all) == 0 {
		return recorded{}
	}
	return all[len(all)-1]
}

func (s *stub) count() int { return len(s.seen()) }

// jsonHandler answers every request with one status and body.
func jsonHandler(status int, body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// testClock is a deterministic clock; every read advances it by a millisecond
// so durations are non-zero but reproducible.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

// newTestClient builds a client against the stub with no real sleeping.
func newTestClient(t *testing.T, s *stub, tweak func(*Config)) (*Client, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	var mu sync.Mutex
	cfg := Config{
		BaseURL:          s.URL,
		APIPath:          "/api",
		AuthMode:         AuthModeAPIKey,
		AuthCollection:   "users",
		Credential:       "test-key-0123456789",
		IdentityVerified: true,
		Now:              newClock().Now,
		Rand:             func() float64 { return 0.5 },
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			slept = append(slept, d)
			mu.Unlock()
			return nil
		},
		RequestID: func() string { return "01TESTTESTTESTTESTTESTTEST" },
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &slept
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		code apierr.Code
	}{
		{"no base url", Config{}, apierr.CodeBaseURLInvalid},
		{"bad scheme", Config{BaseURL: "ftp://x"}, apierr.CodeBaseURLInvalid},
		{"no host", Config{BaseURL: "http://"}, apierr.CodeBaseURLInvalid},
		{"unknown auth mode", Config{BaseURL: "http://h", AuthMode: "token"}, apierr.CodeInvalidOption},
		{"api-key without a collection", Config{BaseURL: "http://h", AuthMode: AuthModeAPIKey}, apierr.CodeAuthCollectionUnknown},
		{"bad scheme flag", Config{BaseURL: "http://h", AuthMode: AuthModeJWT, AuthScheme: "Token"}, apierr.CodeInvalidOption},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(Config{BaseURL: "http://localhost:3900/", APIPath: "api/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.APIPath() != "/api" {
		t.Errorf("APIPath = %q, want /api", c.APIPath())
	}
	if c.GraphQLPath() != "/api/graphql" {
		t.Errorf("GraphQLPath = %q, want /api/graphql", c.GraphQLPath())
	}
	if c.AuthMode() != AuthModeAnonymous {
		t.Errorf("AuthMode = %q, want anonymous", c.AuthMode())
	}
	if c.cfg.AcceptLanguage != "en" {
		t.Errorf("AcceptLanguage = %q, want en", c.cfg.AcceptLanguage)
	}
	if c.cfg.Timeout != DefaultTimeout || c.cfg.UploadTimeout != DefaultUploadTimeout {
		t.Errorf("timeouts = %v/%v", c.cfg.Timeout, c.cfg.UploadTimeout)
	}
	if c.Concurrency() != DefaultConcurrency {
		t.Errorf("Concurrency = %d", c.Concurrency())
	}
	if c.Budget().Remaining() != int64(DefaultMaxRetries*DefaultConcurrency) {
		t.Errorf("budget = %d, want 3 x concurrency", c.Budget().Remaining())
	}
}

func TestNewClampsConcurrency(t *testing.T) {
	c, err := New(Config{BaseURL: "http://h", Concurrency: 1000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Concurrency() != MaxConcurrency {
		t.Fatalf("Concurrency = %d, want the hard cap %d", c.Concurrency(), MaxConcurrency)
	}
}

func TestBaseURLStripsUserinfo(t *testing.T) {
	c, err := New(Config{BaseURL: "https://user:hunter2@example.test/base/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.Contains(c.BaseURL(), "hunter2") || strings.Contains(c.BaseURL(), "user") {
		t.Fatalf("BaseURL leaked userinfo: %s", c.BaseURL())
	}
	if c.BaseURL() != "https://example.test/base" {
		t.Fatalf("BaseURL = %s", c.BaseURL())
	}
}

func TestURLFor(t *testing.T) {
	c, err := New(Config{BaseURL: "http://h:3900", APIPath: "/api"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		req  Request
		want string
	}{
		{Request{Path: "/pages"}, "http://h:3900/api/pages"},
		{Request{Path: "pages/1"}, "http://h:3900/api/pages/1"},
		{Request{Path: "/pages", Query: "limit=2"}, "http://h:3900/api/pages?limit=2"},
		{Request{Path: "/api/graphql", Absolute: true}, "http://h:3900/api/graphql"},
		{Request{Path: "/pages/"}, "http://h:3900/api/pages"},
	}
	for _, tc := range tests {
		if got := c.URLFor(&tc.req); got != tc.want {
			t.Errorf("URLFor(%+v) = %s, want %s", tc.req, got, tc.want)
		}
	}
}

func TestStatsCountRequests(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[],"totalDocs":0}`))
	c, _ := newTestClient(t, s, nil)
	for i := 0; i < 3; i++ {
		if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	if got := c.Stats().Requests; got != 3 {
		t.Fatalf("Requests = %d, want 3", got)
	}
	if c.Stats().Bytes == 0 {
		t.Fatal("Bytes was not counted")
	}
}

func TestWithCredentialSharesState(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	clone := c.WithCredential(AuthModeJWT, "users", "jwt-token-value")
	if _, err := clone.Do(context.Background(), &Request{Path: "/x"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if c.Stats().Requests != 1 {
		t.Fatalf("the clone did not share the stats: %+v", c.Stats())
	}
	if got := s.last().Header.Get(HeaderAuthorization); got != "JWT jwt-token-value" {
		t.Fatalf("Authorization = %q", got)
	}
}
