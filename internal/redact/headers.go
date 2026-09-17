package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

// headerPattern is §5.3's header-name matcher. Header names are matched
// separately from value keys because the Authorization header is synthesised by
// the transport and never appears as a configured key, so the "configured
// headers are secret" rule never covered it.
var headerPattern = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api[-_]?key|x-payload-.*-token)$`)

// Header reports whether a header's value must never be emitted.
func Header(name string) bool {
	return headerPattern.MatchString(strings.TrimSpace(name)) || Key(name)
}

// HeaderValue returns the value that may be printed for a header.
// Authorization is special-cased into the fingerprint form mandated by §5.3's
// hard transport rule, so logs can prove *which* credential was used without
// revealing it.
func HeaderValue(name, value string) string {
	if !Header(name) {
		return Text(value)
	}
	if strings.EqualFold(strings.TrimSpace(name), "authorization") ||
		strings.EqualFold(strings.TrimSpace(name), "proxy-authorization") {
		return AuthorizationValue(value)
	}
	return Mask
}

// Headers returns a copy of h safe to log or embed in a dry-run envelope.
// alsoSecret names additional headers to mask; the transport passes the
// profile's configured [profiles.X.headers] keys, all of which are secret.
func Headers(h http.Header, alsoSecret ...string) http.Header {
	if h == nil {
		return nil
	}
	extra := make(map[string]struct{}, len(alsoSecret))
	for _, n := range alsoSecret {
		extra[http.CanonicalHeaderKey(n)] = struct{}{}
	}
	out := make(http.Header, len(h))
	for name, values := range h {
		_, forced := extra[http.CanonicalHeaderKey(name)]
		masked := make([]string, len(values))
		for i, v := range values {
			if forced && !Header(name) {
				masked[i] = Mask
				continue
			}
			masked[i] = HeaderValue(name, v)
		}
		out[name] = masked
	}
	return out
}

// AuthorizationValue turns any Authorization header value into
// "<redacted:fp=a1b2c3d4e5f60718>" — the exact literal §5.3 requires the
// transport to log. The fingerprint covers the credential only (the last
// whitespace-separated field), so "users API-Key K" and "JWT K" fingerprint
// identically for the same K.
func AuthorizationValue(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return Mask
	}
	cred := v
	if i := strings.LastIndexByte(v, ' '); i >= 0 {
		cred = strings.TrimSpace(v[i+1:])
	}
	// A value that is nothing but a scheme name carries no credential; do not
	// publish a fingerprint of the word "JWT".
	if cred == "" || isAuthScheme(cred) {
		return Mask
	}
	return MaskSecret(cred)
}

// MaskSecret renders a secret as "<redacted:fp=...>". An empty secret yields
// the plain mask so the output never implies a credential existed.
func MaskSecret(secret string) string {
	if secret == "" {
		return Mask
	}
	return "<redacted:fp=" + Fingerprint(secret) + ">"
}

// FingerprintDomain is §4.4's domain separator. It exists so a PayCLI
// fingerprint can never be confused with a bare sha256 of the same key
// computed elsewhere.
const FingerprintDomain = "paycli-key-v1\x00"

// FingerprintLen is §4.4's "16 hex everywhere" rule.
const FingerprintLen = 16

// Fingerprint is §4.4's stable, non-reversible tag for a secret: the first 16
// hex characters of sha256("paycli-key-v1\x00" + secret). It is safe to print,
// log, store in the manifest (identity.key_fingerprint) and compare across
// runs.
//
// This is THE formula. internal/secret.Fingerprint and cache.KeyFingerprint
// both delegate here, because §4.4 requires every producer — credentials.json,
// the manifest, `pay auth status`, `pay cache ls`, the §8.1 scope input and the
// transport's `Authorization: <redacted:fp=…>` log line — to emit the identical
// 16 characters for the same credential.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(FingerprintDomain + secret))
	return hex.EncodeToString(sum[:])[:FingerprintLen]
}

// authSchemes are the scheme keywords PayCLI and Payload use; see §5.0.
var authSchemes = map[string]struct{}{
	"jwt": {}, "bearer": {}, "basic": {}, "api-key": {}, "apikey": {}, "digest": {},
}

func isAuthScheme(s string) bool {
	_, ok := authSchemes[strings.ToLower(s)]
	return ok
}
