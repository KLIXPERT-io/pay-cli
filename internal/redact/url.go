package redact

import (
	"net/url"
	"strings"
)

// URL strips credentials from a URL before it crosses any boundary. §5.3 makes
// it mandatory on meta.base_url, error.http.url, §12.2's request.url,
// Event.BaseURL, Event.Path, the manifest's meta.base_url and source.base_url,
// and references/PROJECT.md.
//
// Three things are removed:
//
//   - userinfo ("https://user:pass@host" -> "https://host"), removed outright
//     rather than masked, because a masked userinfo re-encodes to
//     "%3Credacted%3E@host" and stops being a usable URL;
//   - any query parameter whose name matches the value or header matchers;
//   - any path segment shaped like a JWT (reset-password links).
//
// Percent-encoding, parameter order and the rest of the URL are preserved
// byte-for-byte: the raw query is rewritten in place, not re-encoded.
func URL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Not parseable; fall back to the text rules so a credential in a
		// malformed URL is still removed.
		return Text(raw)
	}
	if u.User != nil {
		u.User = nil
	}
	if u.RawQuery != "" {
		u.RawQuery = redactQuery(u.RawQuery)
	}
	if strings.Contains(u.Path, "eyJ") {
		u.Path = redactPath(u.Path)
		u.RawPath = ""
	}
	// url.URL.String percent-encodes the angle brackets of Mask when it sits in
	// the path; put the literal back so callers always see the same sentinel.
	return strings.ReplaceAll(u.String(), "%3Credacted%3E", Mask)
}

func redactQuery(rawQuery string) string {
	parts := strings.Split(rawQuery, "&")
	for i, p := range parts {
		if p == "" {
			continue
		}
		name, _, hasValue := strings.Cut(p, "=")
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if !Key(decoded) && !Header(decoded) {
			continue
		}
		if hasValue {
			parts[i] = name + "=" + Mask
		} else {
			parts[i] = name
		}
	}
	return strings.Join(parts, "&")
}

func redactPath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if IsJWT(s) {
			segs[i] = Mask
		}
	}
	return strings.Join(segs, "/")
}
