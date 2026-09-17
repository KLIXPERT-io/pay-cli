package payloadtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Recorded is one request the harness actually received, so a test can assert
// on what PayCLI sent — the bulk-write "no limit= parameter" rule (§12.3) is
// exactly this kind of assertion.
type Recorded struct {
	Method string
	Path   string
	Query  string
	Body   string
	Header http.Header
}

// Server is a fixture-backed Payload instance.
type Server struct {
	*httptest.Server

	t     testing.TB
	mu    sync.Mutex
	seen  []Recorded
	miss  []Recorded
	index Index

	byPath   map[string][]Route
	handlers map[string]http.HandlerFunc
	strict   bool
}

// Option configures NewServer.
type Option func(*Server)

// Strict makes an unmatched request fail the test immediately. The default is
// to answer 404 with Payload's own "Route not found" body and record the miss,
// because discovery legitimately probes routes that do not exist.
func Strict() Option { return func(s *Server) { s.strict = true } }

// WithHandler overrides one route with a live handler — for a 429 with a
// Retry-After, a truncated body, or anything else no fixture can express.
func WithHandler(method, path string, h http.HandlerFunc) Option {
	return func(s *Server) { s.handlers[method+" "+path] = h }
}

// NewServer starts a server that answers from testdata/fixtures. It is closed
// automatically when the test ends.
func NewServer(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		t:        t,
		byPath:   map[string][]Route{},
		handlers: map[string]http.HandlerFunc{},
	}
	if Has(t, IndexName) {
		s.index = LoadIndex(t)
	}
	for _, r := range s.index.Requests {
		pk := r.Method + " " + r.Path
		s.byPath[pk] = append(s.byPath[pk], r)
	}
	for _, opt := range opts {
		opt(s)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// PayloadVersion is the version the fixtures were recorded from.
func (s *Server) PayloadVersion() string { return s.index.PayloadVersion }

// Requests returns every request the server received, in order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Recorded, len(s.seen))
	copy(out, s.seen)
	return out
}

// Misses returns the requests no fixture and no handler answered.
func (s *Server) Misses() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Recorded, len(s.miss))
	copy(out, s.miss)
	return out
}

// Reset clears the recorded traffic between sub-tests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen, s.miss = nil, nil
}

// Count returns how many requests matched a method and path prefix.
func (s *Server) Count(method, pathPrefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := Recorded{
		Method: r.Method, Path: r.URL.Path,
		Query: canonicalQuery(r.URL.RawQuery), Body: string(body),
		Header: r.Header.Clone(),
	}
	s.mu.Lock()
	s.seen = append(s.seen, rec)
	s.mu.Unlock()

	if h, ok := s.handlers[r.Method+" "+r.URL.Path]; ok {
		h(w, r)
		return
	}
	route, ok := s.match(rec)
	if !ok {
		s.mu.Lock()
		s.miss = append(s.miss, rec)
		s.mu.Unlock()
		if s.strict {
			s.t.Fatalf("payloadtest: no fixture for %s %s?%s\nrun \"make fixtures\" to record it",
				r.Method, r.URL.Path, rec.Query)
		}
		// Payload's own catch-all answer, so classification code under test
		// sees the real shape rather than an httptest default.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Powered-By", "Payload")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": fmt.Sprintf("Route not found \"%s\"", r.URL.Path)}},
		})
		return
	}

	data, err := os.ReadFile(FixturePath(s.t, route.File))
	if err != nil {
		s.t.Fatalf("payloadtest: %v", err)
	}
	ct := route.ContentType
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	for k, v := range route.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", ct)
	if w.Header().Get("X-Powered-By") == "" {
		w.Header().Set("X-Powered-By", "Payload")
	}
	status := route.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// match resolves a request to a fixture. Candidates must first satisfy their
// own body/header/anonymous matchers; among the survivors the one whose
// recorded query is the most specific subset of the request's wins, and ties
// break towards the more constrained fixture.
//
// The two-stage shape matters: a fixture recorded without depth=0 must still
// answer a request that adds it, or every harmless encoder change would need a
// re-record.
func (s *Server) match(rec Recorded) (Route, bool) {
	candidates := s.byPath[rec.Method+" "+rec.Path]
	best, found := Route{}, false
	bestScore := -1
	want := parseQuery(rec.Query)
	for _, c := range candidates {
		if !c.matches(rec.Method, rec.Path, rec.Query, rec.Body, rec.Header) {
			continue
		}
		have := parseQuery(c.Query)
		score := c.specificity() * 100
		ok := true
		for k, v := range have {
			if want[k] != v {
				ok = false
				break
			}
			score += 10
		}
		if !ok {
			continue
		}
		if c.Query == rec.Query {
			score += 5
		}
		if score > bestScore {
			best, bestScore, found = c, score, true
		}
	}
	return best, found
}

// canonicalQuery sorts parameters so a fixture matches regardless of the order
// the encoder emitted them in.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func parseQuery(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		key, _ := url.QueryUnescape(k)
		val, _ := url.QueryUnescape(v)
		out[key] = val
	}
	return out
}
