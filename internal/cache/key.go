// Package cache implements PayCLI's on-disk discovery cache (§8) and the
// in-process memo layer that sits in front of it (§8.6).
//
// Three invariants shape everything in this package:
//
//   - The cache is scoped by *credential*, not just by URL (§8.1). `/api/access`
//     returns 10 collections anonymously and 49 authenticated, so a scope key
//     that did not bind the credential would serve a truncated inventory in
//     which 39 collections appear not to exist.
//   - Exactly one predicate, Cacheable, may authorise a disk write (§8.3).
//     Document data, write responses, non-200s and non-JSON never touch disk.
//   - Any failure on the read path is a *miss*, never a command failure (§8.3),
//     so `rm -rf $(pay cache path)` only ever costs latency (§4.1).
//
// Nothing in this package calls time.Now: every entry point that needs the
// clock takes it as an argument, per §3.1.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

const (
	// AnonKeyFingerprint is the §8.1 sentinel used in anonymous mode. It is a
	// literal, not a hash, so an anonymous scope can never collide with a
	// credentialled one (§8.3 rule 6 relies on this).
	AnonKeyFingerprint = "anon"

	// NoHeadersFingerprint is the §8.1 sentinel for "no extra headers".
	NoHeadersFingerprint = "none"

	// FingerprintLen is the §5 "16 hex everywhere" rule: credentials.json,
	// manifest.meta.key_fingerprint, `pay auth status`, `pay cache ls` and the
	// §8.1 scope input all use the same 16 characters.
	FingerprintLen = 16
)

// KeyFingerprint is the §5 credential fingerprint: the first 16 hex characters
// of sha256("paycli-key-v1\x00" + credential). An empty credential (anonymous
// mode) yields the literal "anon".
//
// The credential itself is hashed and immediately discarded; it is never stored
// in a scope, a manifest, a log line or an error message (§5.3).
func KeyFingerprint(credential string) string {
	if credential == "" {
		return AnonKeyFingerprint
	}
	return redact.Fingerprint(credential)
}

func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:FingerprintLen]
}

// ScopeInput is everything §8.1 feeds into the scope key. Exactly one of
// Credential / KeyFingerprint is needed; Credential is hashed on the spot.
//
// authCollectionSlug is deliberately absent: §8.1 states it is a derived
// function of (normURL, credential) and including it would make the scope path
// uncomputable until after the answer it stores was already known.
// The profile *name* is absent for the same stated reason — two profiles
// pointing at the same server with the same key share one discovery.
type ScopeInput struct {
	// BaseURL is the profile's resolved base URL. Userinfo is stripped by
	// config.Resolve (§4.2); it is stripped again here, defensively.
	BaseURL string
	// APIPath is the Payload REST prefix, e.g. "/api". It is part of normURL
	// because two Payload projects on one host can use different routes.api.
	APIPath string
	// GraphQLPath is the resolved GraphQL endpoint, e.g. "/api/graphql". It is
	// an input because it determines capabilities.graphql.mode and every
	// GraphQL-derived field schema.
	GraphQLPath string
	// Credential is the API key (api-key mode) or the JWT (jwt mode), empty in
	// anonymous mode. Hashed immediately, never retained.
	Credential string
	// KeyFingerprint short-circuits Credential when the caller already holds
	// the §5 fingerprint (internal/secret computes it once per process).
	KeyFingerprint string
	// Headers is the [profiles.X.headers] table AFTER ${ENV} interpolation.
	// Values are hashed and discarded; only the sorted lowercase names survive,
	// into Scope.HeaderNames and manifest meta.header_names (§5.3).
	Headers map[string]string
}

// Scope is a computed cache scope. It holds no secret material: the credential
// and every header value have already been reduced to 16-hex fingerprints.
type Scope struct {
	// Key is the 16-hex scope directory name.
	Key string
	// NormURL is lowercase(scheme)://lowercase(host)[:port]+api_path.
	NormURL string
	// GraphQLPath is the normalised GraphQL endpoint path.
	GraphQLPath string
	// KeyFingerprint is the 16-hex credential fingerprint, or "anon".
	KeyFingerprint string
	// HeaderNames are the sorted, de-duplicated, lowercased extra-header names.
	// Values are never present, here or anywhere else on disk.
	HeaderNames []string
	// HeadersFingerprint is the 16-hex hash of the sorted name:value pairs, or
	// the literal "none".
	HeadersFingerprint string
}

// Anonymous reports whether the scope carries no credential.
func (s Scope) Anonymous() bool {
	return s.KeyFingerprint == AnonKeyFingerprint || s.KeyFingerprint == ""
}

// String returns the scope key, which is what every path and log line uses.
func (s Scope) String() string { return s.Key }

// Valid reports whether the scope key is a well-formed 16-hex directory name.
// Scope lookup only ever resolves an exact 16-hex name, which is what makes
// §8.7's rename-to-trash GC invisible to readers.
func (s Scope) Valid() bool { return ValidScopeKey(s.Key) }

// ValidScopeKey reports whether name is exactly 16 lowercase hex characters.
func ValidScopeKey(name string) bool {
	if len(name) != FingerprintLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewScope computes the §8.1 scope key.
//
//	scope = sha256hex(normURL \0 graphqlPath \0 keyFP \0 headersFP)[:16]
//
// A malformed base URL is the one hard error: it is a configuration problem
// (exit 9), not a cache miss.
func NewScope(in ScopeInput) (Scope, error) {
	normURL, err := normalizeBase(in.BaseURL, in.APIPath)
	if err != nil {
		return Scope{}, err
	}

	keyFP := in.KeyFingerprint
	if keyFP == "" {
		keyFP = KeyFingerprint(in.Credential)
	}

	names, headersFP := fingerprintHeaders(in.Headers)
	gql := normalizePath(in.GraphQLPath)

	sc := Scope{
		NormURL:            normURL,
		GraphQLPath:        gql,
		KeyFingerprint:     keyFP,
		HeaderNames:        names,
		HeadersFingerprint: headersFP,
	}
	sc.Key = hash16(strings.Join([]string{normURL, gql, keyFP, headersFP}, "\x00"))
	return sc, nil
}

// fingerprintHeaders reduces the extra-header table to (sorted names, hash).
//
// Names are lowercased before both the hash and the name list: HTTP header
// names are case-insensitive, so two profiles that differ only in the spelling
// of "X-Vercel-Protection-Bypass" address the same server the same way and must
// share a scope.
func fingerprintHeaders(headers map[string]string) ([]string, string) {
	if len(headers) == 0 {
		return nil, NoHeadersFingerprint
	}
	pairs := make([]string, 0, len(headers))
	seen := make(map[string]struct{}, len(headers))
	names := make([]string, 0, len(headers))
	for k, v := range headers {
		name := strings.ToLower(strings.TrimSpace(k))
		if name == "" {
			continue
		}
		pairs = append(pairs, name+":"+v)
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(pairs) == 0 {
		return nil, NoHeadersFingerprint
	}
	sort.Strings(pairs)
	sort.Strings(names)
	return names, hash16(strings.Join(pairs, "\n"))
}

// normalizeBase builds §8.1's normURL.
//
// Deviation, stated per the "pick the most consistent option" rule: §8.1 writes
// normURL as scheme://host[:port] + api_path, which drops any path prefix on
// base_url. A project served at https://example.com/cms and another at
// https://example.com/shop would then share one scope and poison each other's
// inventory — the exact failure §8.1 exists to prevent. The base path is
// therefore kept, immediately before api_path.
func normalizeBase(baseURL, apiPath string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", apierr.New(apierr.CodeBaseURLInvalid, "base_url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", apierr.Wrap(err, apierr.CodeBaseURLInvalid, "base_url %q is not a valid URL", redactedURL(raw))
	}
	if u.Scheme == "" || u.Host == "" {
		return "", apierr.New(apierr.CodeBaseURLInvalid, "base_url %q must be an absolute http(s) URL", redactedURL(raw))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", apierr.New(apierr.CodeBaseURLInvalid, "base_url scheme %q is not http or https", scheme)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", apierr.New(apierr.CodeBaseURLInvalid, "base_url %q has no host", redactedURL(raw))
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	hostport := host
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}

	return scheme + "://" + hostport + normalizePath(u.Path) + normalizePath(apiPath), nil
}

// normalizePath canonicalises a path segment for hashing: a leading slash, no
// trailing slash, empty stays empty. "/api" and "/api/" must not produce two
// scopes for one server.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// redactedURL is applied to every URL that reaches an error message or a cache
// file (§5.3, §8.3's "url is stored only after redact.URL").
func redactedURL(raw string) string { return redact.URL(raw) }
