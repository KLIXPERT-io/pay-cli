package discovery

import (
	"context"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func newBootstrapDiscoverer(t *testing.T, f *fakeAPI, mutate func(*Options)) *Discoverer {
	t.Helper()
	// The client is built BEFORE the slug is known, which is the whole point
	// of Stage -1: in api-key mode the header embeds the slug.
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: f.apiPath,
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "auto", Credential: testAPIKey,
		RoundTripper: f, Now: fixedClock(), Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	opt := Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: f.apiPath,
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAPIKey,
		Concurrency: 4, Now: fixedClock(), Generation: "gen", CLIVersion: "0.1.0",
	}
	if mutate != nil {
		mutate(&opt)
	}
	d, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestAuthBootstrapResolvesTheSlug(t *testing.T) {
	f := newFakeAPI()
	d := newBootstrapDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AuthCollection != "users" {
		t.Fatalf("auth collection = %q", res.AuthCollection)
	}
	if res.AuthCollectionSource != SourceBootstrap || !res.AuthResolved {
		t.Errorf("source = %q resolved = %v", res.AuthCollectionSource, res.AuthResolved)
	}
	if got := res.Manifest.Identity.AuthCollection; got == nil || *got != "users" {
		t.Errorf("identity.auth_collection = %v", got)
	}
	if !res.Manifest.Identity.Verified {
		t.Error("the resolved slug must have produced a verified identity")
	}
	if len(res.Manifest.Diagnostics.AuthCandidates) == 0 {
		t.Error("the candidates tried must be recorded")
	}
	// Step 1's /access probe carries NO Authorization header: it exists only
	// to obtain a candidate slug set and its result is truncated by
	// construction.
	if f.count("GET /api/access") == 0 {
		t.Error("step 1 did not run")
	}
}

// TestAuthBootstrapNeverGuesses is §7.0's central hazard: a wrong slug returns
// 200 with the anonymous view, byte-indistinguishable from a low-privilege key.
func TestAuthBootstrapNeverGuesses(t *testing.T) {
	f := newFakeAPI()
	// The real auth collection is `admins`; `users` exists and answers /init
	// but never authenticates. A bootstrap that guessed `users` would produce
	// a truncated inventory in which most collections appear not to exist.
	f.authCollection = "admins"
	f.authCollections = map[string]bool{"users": true, "admins": true}
	f.accessCollections["admins"] = map[string]any{"read": true}
	f.labelFor["admins"] = "Admins"
	f.graphQLCollections = append(f.graphQLCollections, "admins")
	singularOf["admins"] = "Admin"
	pluralOf["admins"] = "Admins"
	f.types["Admin"] = &IntroType{Kind: KindObject, Name: "Admin", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
	}}
	defer func() {
		delete(singularOf, "admins")
		delete(pluralOf, "admins")
	}()

	d := newBootstrapDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AuthCollection != "admins" {
		t.Fatalf("auth collection = %q, want admins", res.AuthCollection)
	}
	// Both candidates must be recorded, so a human can see what was tried.
	if len(res.Manifest.Diagnostics.AuthCandidates) < 2 {
		t.Errorf("candidates = %v", res.Manifest.Diagnostics.AuthCandidates)
	}
}

func TestAuthBootstrapFailsLoudlyRatherThanGuessingUsers(t *testing.T) {
	f := newFakeAPI()
	f.authCollection = "nobody" // nothing authenticates
	d := newBootstrapDiscoverer(t, f, nil)
	_, err := d.Run(context.Background())
	if !apierr.HasCode(err, apierr.CodeAuthCollectionUnknown) {
		t.Fatalf("err = %v, want auth_collection_unknown", err)
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "--auth-collection") || !strings.Contains(e.Hint, "candidates tried") {
		t.Errorf("hint = %q", e.Hint)
	}
}

func TestAuthBootstrapIsSkippedWhenAlreadyKnown(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Options)
		wantSource string
	}{
		{"explicit slug", func(o *Options) {
			o.AuthCollection = "users"
			o.AuthCollectionSource = SourceConfigured
		}, SourceConfigured},
		{"cached resolution", func(o *Options) {
			o.CachedAuthCollection = "users"
		}, SourceCached},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI()
			d := newBootstrapDiscoverer(t, f, tt.mutate)
			res, err := d.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.AuthCollectionSource != tt.wantSource {
				t.Errorf("source = %q, want %q", res.AuthCollectionSource, tt.wantSource)
			}
			if res.AuthResolved {
				t.Error("nothing new was resolved, so nothing should be persisted")
			}
			// Stage -1's /init sweep must not have run.
			if n := f.count("/init"); n != 0 {
				t.Errorf("the /init filter ran %d times for an already-known slug", n)
			}
		})
	}
}

func TestAuthBootstrapIsSkippedInAnonymousMode(t *testing.T) {
	f := newFakeAPI()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: "/api",
		AuthMode: payload.AuthModeAnonymous, RoundTripper: f, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d, err := New(Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: "/api",
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAnonymous,
		CredentialAbsent: true, Concurrency: 2, Now: fixedClock(), Generation: "gen",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.needsBootstrap() {
		t.Fatal("anonymous mode must skip Stage -1")
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := f.count("/init"); n != 0 {
		t.Errorf("the /init filter ran %d times in anonymous mode", n)
	}
}

func TestProbeInitRequiresABoolean(t *testing.T) {
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	if !d.probeInit(context.Background(), "users") {
		t.Error("users must pass the /init filter")
	}
	// /init is access-gated, so a non-auth collection answers 403 — the filter
	// narrows the candidate set but can never widen it.
	if d.probeInit(context.Background(), "pages") {
		t.Error("pages must not pass the /init filter")
	}
}

func TestAuthCandidatesTruncatedIsDeclared(t *testing.T) {
	// With GraphQL unreachable and no project source, the candidate set is the
	// anonymous slug list alone.
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	d := newBootstrapDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasLimitation(res.Manifest, LimAuthCandidatesTruncated) {
		t.Fatalf("AUTH_CANDIDATES_TRUNCATED was not declared: %+v", res.Manifest.Limitations)
	}
	if res.AuthCollection != "users" {
		t.Errorf("the anonymous list still contained the answer here: %q", res.AuthCollection)
	}
}

func TestAuthBootstrapWidensFromProjectSource(t *testing.T) {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	f.authCollection = "admins"
	f.authCollections = map[string]bool{"admins": true}
	f.accessCollections["admins"] = map[string]any{"read": true}
	f.labelFor["admins"] = "Admins"
	// The anonymous /access view does not list admins, so without the project
	// source the candidate set would not contain the answer.
	d := newBootstrapDiscoverer(t, f, func(o *Options) {
		o.ProjectAuthSlugs = []string{"admins"}
	})
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AuthCollection != "admins" {
		t.Fatalf("auth collection = %q, want admins", res.AuthCollection)
	}
	if hasLimitation(res.Manifest, LimAuthCandidatesTruncated) {
		t.Error("the candidate set was widened, so it is not truncated")
	}
}
