package discovery

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func TestNormalizeAPIPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api": "/api", "api": "/api", "/api/": "/api", "": "", "/": "",
		"/cms-api/": "/cms-api", "  /x  ": "/x",
	} {
		if got := normalizeAPIPath(in); got != want {
			t.Errorf("normalizeAPIPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeAccessEnforcesShapeNotStatus(t *testing.T) {
	tests := []struct {
		name string
		res  *restResult
		ok   bool
	}{
		{"real payload", &restResult{Status: 200, ContentType: "application/json",
			Body: []byte(`{"canAccessAdmin":true,"collections":{"pages":{}},"globals":{}}`)}, true},
		// A wrong base URL answers 200 text/html (verified live).
		{"html 200", &restResult{Status: 200, ContentType: "text/html", Body: []byte("<html>")}, false},
		{"json without collections", &restResult{Status: 200, ContentType: "application/json",
			Body: []byte(`{"hello":"world"}`)}, false},
		{"collections is not an object", &restResult{Status: 200, ContentType: "application/json",
			Body: []byte(`{"collections":[]}`)}, false},
		{"non-200", &restResult{Status: 404, ContentType: "application/json",
			Body: []byte(`{"collections":{}}`)}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := decodeAccess(tt.res)
			if ok != tt.ok {
				t.Fatalf("decodeAccess ok = %v, want %v", ok, tt.ok)
			}
		})
	}
}

func TestDecodeAccessDefaultsGlobalsToAnEmptyMap(t *testing.T) {
	v, ok := decodeAccess(&restResult{Status: 200, ContentType: "application/json",
		Body: []byte(`{"collections":{"pages":{}}}`)})
	if !ok {
		t.Fatal("must decode")
	}
	if v.Globals == nil {
		t.Fatal("globals must never be nil; a caller ranges over it without a check")
	}
}

func TestDecodeIdentityUsesUserNotStatus(t *testing.T) {
	tests := []struct {
		name     string
		res      *restResult
		verified bool
	}{
		{"real user", &restResult{Status: 200, ContentType: "application/json",
			Body: []byte(`{"user":{"id":66,"_strategy":"api-key","canAccessAdmin":true}}`)}, true},
		// A wrong key answers HTTP 200 with user:null; a wrong auth-collection
		// slug answers 200 with the anonymous view. Neither is a non-200.
		{"wrong key", &restResult{Status: 200, ContentType: "application/json",
			Body: []byte(`{"user":null}`)}, false},
		{"non-json", &restResult{Status: 200, ContentType: "text/html", Body: []byte("<html>")}, false},
		{"403", &restResult{Status: 403, ContentType: "application/json", Body: []byte(`{"user":{"id":1}}`)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := decodeIdentity(tt.res, "users")
			got := id != nil && id.Verified
			if got != tt.verified {
				t.Fatalf("verified = %v, want %v", got, tt.verified)
			}
		})
	}
	id := decodeIdentity(&restResult{Status: 200, ContentType: "application/json",
		Body: []byte(`{"user":{"id":66}}`)}, "users")
	// canAccessAdmin is absent when false, never literally false.
	if id.CanAccessAdmin {
		t.Error("an absent canAccessAdmin must read as false")
	}
}

// apiPathServer answers /api/access only at one configured path.
type apiPathServer struct {
	mu     sync.Mutex
	accept string
	hits   []string
}

func (s *apiPathServer) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hits = append(s.hits, r.URL.Path)
	s.mu.Unlock()
	if r.URL.Path == s.accept+"/access" {
		return jsonResponse(200, map[string]any{"collections": map[string]any{"pages": map[string]any{}}, "globals": map[string]any{}}), nil
	}
	// Everything else answers 200 text/html, the exact trap §7.2 names.
	return rawResponse(200, "text/html", []byte("<html></html>")), nil
}

func TestResolveAPIPathProbesTheCandidateList(t *testing.T) {
	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{"default", "/api", "/api"},
		{"cms-api", "/cms-api", "/cms-api"},
		{"payload-api", "/payload-api", "/payload-api"},
		{"root", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &apiPathServer{accept: tt.accept}
			cli, err := payload.New(payload.Config{
				BaseURL: "http://payload.test", APIPath: "/api",
				AuthMode: payload.AuthModeAnonymous, RoundTripper: srv, Now: fixedClock(),
			})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			d, err := New(Options{
				Client: cli, BaseURL: "http://payload.test", APIPath: "/api",
				APIPathSource: SourceProbed, AuthMode: payload.AuthModeAnonymous,
				CredentialAbsent: true, Concurrency: 4, Now: fixedClock(), Generation: "gen",
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, source, view := d.resolveAPIPath(context.Background())
			if got != tt.want {
				t.Fatalf("api_path = %q, want %q", got, tt.want)
			}
			if view == nil {
				t.Fatal("the accepted candidate must carry the decoded access view")
			}
			if source == "" {
				t.Error("the choice must be recorded")
			}
		})
	}
}

func TestGraphQLPathIsDerivedFromTheResolvedAPIPath(t *testing.T) {
	// Payload computes the GraphQL route as routes.api + routes.graphQL, so
	// the two are nested, never independent. A probed api_path must move it.
	d := &Discoverer{apiPath: "/cms-api", graphQLRoute: ""}
	d.recomputeGraphQLPath()
	if d.graphQLPath != "/cms-api/graphql" {
		t.Fatalf("graphql_path = %q", d.graphQLPath)
	}
	if d.graphQLPathSource != SourceDerivedGraphQLPath {
		t.Errorf("source = %q", d.graphQLPathSource)
	}

	// An explicitly configured path is never overwritten.
	d = &Discoverer{apiPath: "/cms-api", graphQLPath: "/gql", graphQLPathSource: SourceConfigured}
	d.recomputeGraphQLPath()
	if d.graphQLPath != "/gql" {
		t.Fatalf("a configured graphql_path was overwritten: %q", d.graphQLPath)
	}

	d = &Discoverer{apiPath: "/api", graphQLRoute: "custom"}
	d.recomputeGraphQLPath()
	if d.graphQLPath != "/api/custom" {
		t.Fatalf("graphql_path = %q", d.graphQLPath)
	}
}

func TestSlugFromProbePath(t *testing.T) {
	for in, want := range map[string]string{
		"/pages":              "pages",
		"/pages/versions":     "pages",
		"/crm-contacts/count": "crm-contacts",
		"/access":             "",
		"/graphql":            "",
		"/globals/header":     "",
		"/reorder":            "",
		"":                    "",
	} {
		if got := slugFromProbePath(in); got != want {
			t.Errorf("slugFromProbePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotPayloadErrorNamesProjectRoutes(t *testing.T) {
	d := &Discoverer{baseURL: "http://x", apiPath: "/api", projectRoutes: []string{"/cms-api"}}
	err := d.notPayloadError()
	e, ok := apierr.As(err)
	if !ok {
		t.Fatalf("not an *apierr.Error: %v", err)
	}
	if !strings.Contains(e.Hint, "/cms-api") {
		t.Fatalf("the hint must name the route found in payload.config.ts: %q", e.Hint)
	}

	plain := (&Discoverer{baseURL: "http://x", apiPath: "/api"}).notPayloadError()
	pe, _ := apierr.As(plain)
	if !strings.Contains(pe.Hint, "--api-path") {
		t.Errorf("without project source the hint must name the probe list: %q", pe.Hint)
	}
}
