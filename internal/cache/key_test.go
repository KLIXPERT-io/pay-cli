package cache

import (
	"strings"
	"testing"
)

func mustScope(t *testing.T, in ScopeInput) Scope {
	t.Helper()
	sc, err := NewScope(in)
	if err != nil {
		t.Fatalf("NewScope(%+v): %v", in, err)
	}
	return sc
}

// base is the live instance from GROUNDING.md.
func base() ScopeInput {
	return ScopeInput{
		BaseURL:     "http://localhost:3900",
		APIPath:     "/api",
		GraphQLPath: "/api/graphql",
		Credential:  "paycli-dev-key-deadbeefdeadbeef",
	}
}

// TestScopeEqualityTable is §8.1's mandated table: which inputs share a scope
// and which must not.
func TestScopeEqualityTable(t *testing.T) {
	withHeaders := base()
	withHeaders.Headers = map[string]string{"X-Vercel-Protection-Bypass": "s3cret"}

	otherHeaders := base()
	otherHeaders.Headers = map[string]string{"X-Vercel-Protection-Bypass": "different"}

	otherGraphQL := base()
	otherGraphQL.GraphQLPath = "/graphql"

	otherAPI := base()
	otherAPI.APIPath = "/cms-api"

	otherKey := base()
	otherKey.Credential = "another-key-that-is-long-enough"

	anon := base()
	anon.Credential = ""

	tests := []struct {
		name  string
		a, b  ScopeInput
		equal bool
	}{
		{"same key, different header set", withHeaders, otherHeaders, false},
		{"same key, headers vs none", withHeaders, base(), false},
		{"same key, different graphql_path", base(), otherGraphQL, false},
		{"same key, different api_path", base(), otherAPI, false},
		{"different key", base(), otherKey, false},
		{"credentialled vs anonymous", base(), anon, false},
		// The profile name is deliberately not in the hash: two profiles on the
		// same server with the same key share one 0.95 s discovery.
		{"same everything, different profile name", base(), base(), true},
		// auth_collection is not an input at all, so "auto" and an explicit
		// slug that resolves to the same thing are the same scope by
		// construction — there is no field to differ in.
		{"auto vs explicit auth_collection", base(), base(), true},
		{"header name case differs", headersOf("X-Bypass", "v"), headersOf("x-bypass", "v"), true},
		{"trailing slash on api_path", base(), apiPathOf("/api/"), true},
		{"explicit default port", base(), baseURLOf("http://localhost:3900"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustScope(t, tc.a)
			b := mustScope(t, tc.b)
			if got := a.Key == b.Key; got != tc.equal {
				t.Fatalf("equal=%v, want %v (a=%s b=%s)", got, tc.equal, a.Key, b.Key)
			}
		})
	}
}

func headersOf(name, value string) ScopeInput {
	in := base()
	in.Headers = map[string]string{name: value}
	return in
}

func apiPathOf(p string) ScopeInput {
	in := base()
	in.APIPath = p
	return in
}

func baseURLOf(u string) ScopeInput {
	in := base()
	in.BaseURL = u
	return in
}

func TestScopeKeyShape(t *testing.T) {
	sc := mustScope(t, base())
	if !sc.Valid() {
		t.Fatalf("scope key %q is not 16 lowercase hex", sc.Key)
	}
	if len(sc.KeyFingerprint) != FingerprintLen {
		t.Fatalf("key fingerprint %q is not %d chars", sc.KeyFingerprint, FingerprintLen)
	}
	if sc.Anonymous() {
		t.Fatal("a credentialled scope reports Anonymous")
	}
}

func TestDefaultPortsNormalise(t *testing.T) {
	tests := []struct{ a, b string }{
		{"http://example.com", "http://example.com:80"},
		{"https://example.com", "https://example.com:443"},
		{"http://EXAMPLE.com", "http://example.com"},
		{"HTTP://example.com", "http://example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if mustScope(t, baseURLOf(tc.a)).Key != mustScope(t, baseURLOf(tc.b)).Key {
				t.Fatalf("%s and %s produced different scopes", tc.a, tc.b)
			}
		})
	}
	// A non-default port must still separate two projects.
	if mustScope(t, baseURLOf("http://example.com")).Key == mustScope(t, baseURLOf("http://example.com:3900")).Key {
		t.Fatal("a non-default port did not change the scope")
	}
	// A base path prefix separates two projects on one host.
	if mustScope(t, baseURLOf("http://example.com/cms")).Key == mustScope(t, baseURLOf("http://example.com/shop")).Key {
		t.Fatal("two base paths on one host shared a scope")
	}
}

func TestScopeNeverStoresSecrets(t *testing.T) {
	in := base()
	in.Headers = map[string]string{"X-Vercel-Protection-Bypass": "SUPER-SECRET-VALUE"}
	sc := mustScope(t, in)

	blob := strings.Join(append([]string{sc.Key, sc.NormURL, sc.GraphQLPath, sc.KeyFingerprint, sc.HeadersFingerprint}, sc.HeaderNames...), "|")
	for _, secret := range []string{in.Credential, "SUPER-SECRET-VALUE"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("scope leaked %q: %s", secret, blob)
		}
	}
	if got, want := sc.HeaderNames, "x-vercel-protection-bypass"; len(got) != 1 || got[0] != want {
		t.Fatalf("header names = %v, want [%s]", got, want)
	}
}

func TestKeyFingerprintIsDomainSeparated(t *testing.T) {
	if got := KeyFingerprint(""); got != AnonKeyFingerprint {
		t.Fatalf("anonymous fingerprint = %q, want %q", got, AnonKeyFingerprint)
	}
	// §5: sha256("paycli-key-v1\x00" + credential)[:16]. Pinned so every
	// producer (secret, discovery, cache) can be asserted against one value.
	const cred = "paycli-dev-key-deadbeefdeadbeef"
	got := KeyFingerprint(cred)
	want := hash16("paycli-key-v1\x00" + cred)
	if got != want || len(got) != FingerprintLen {
		t.Fatalf("KeyFingerprint(%q) = %q, want %q", cred, got, want)
	}
	if got == hash16(cred) {
		t.Fatal("fingerprint is not domain-separated")
	}
}

func TestScopeInputKeyFingerprintShortCircuit(t *testing.T) {
	in := base()
	withCred := mustScope(t, in)

	in2 := base()
	in2.Credential = ""
	in2.KeyFingerprint = KeyFingerprint(base().Credential)
	if mustScope(t, in2).Key != withCred.Key {
		t.Fatal("a pre-computed fingerprint produced a different scope")
	}
}

func TestNewScopeRejectsBadBaseURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:3900", "ftp://example.com", "://nope"} {
		t.Run(raw, func(t *testing.T) {
			in := base()
			in.BaseURL = raw
			if _, err := NewScope(in); err == nil {
				t.Fatalf("NewScope accepted base_url %q", raw)
			}
		})
	}
}

func TestNewScopeStripsUserinfo(t *testing.T) {
	a := mustScope(t, baseURLOf("http://user:pw@example.com"))
	b := mustScope(t, baseURLOf("http://example.com"))
	if a.Key != b.Key {
		t.Fatal("userinfo changed the scope key")
	}
	if strings.Contains(a.NormURL, "pw") || strings.Contains(a.NormURL, "user") {
		t.Fatalf("normURL retained userinfo: %s", a.NormURL)
	}
}

func TestValidScopeKey(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"0df77681bf2d6bfc", true},
		{"0DF77681BF2D6BFC", false},
		{"0df77681bf2d6bf", false},
		{"0df77681bf2d6bfc0", false},
		{"", false},
		{"../../etc/passwd", false},
		{"0df77681bf2d6bfg", false},
	}
	for _, tc := range tests {
		if got := ValidScopeKey(tc.in); got != tc.want {
			t.Errorf("ValidScopeKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
