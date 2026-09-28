package discovery

import "github.com/KLIXPERT-io/pay-cli/internal/redact"

// withholdValue reports that a scalar value must not be sampled because the
// field's NAME says it holds a credential (hash, salt, password, apiKey,
// sessions, anything matching token|secret — redact.Key, §5.3).
//
// Such a field is still profiled (kind, JSON types, presence), so a block with
// a `hubspotFormToken` field is fully described; only its values stay out of
// the census, which is persisted, and §5.3 forbids a secret from reaching
// disk. Objects and arrays under such a name are walked as usual: their keys
// are schema, not secrets.
func withholdValue(key string, v any) bool {
	switch v.(type) {
	case map[string]any, []any, nil:
		return false
	}
	return redact.Key(key)
}
