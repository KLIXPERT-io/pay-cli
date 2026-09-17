// Package secret resolves, stores and fingerprints PayCLI's credentials
// (§5.1, §5.2, §4.4).
//
// Three rules govern everything here:
//
//   - Storage is file-first. credentials.json at 0600 is the default target;
//     the OS keychain is used only when the profile opted in or PAY_KEYRING
//     asks for it (§5.2).
//   - A resolved credential never reaches stdout, a log, an audit record, a
//     cache file or an error message. Only its 16-hex fingerprint does (§5.3).
//   - Nothing in this package reads the process environment or the clock
//     directly: both arrive as arguments, so the ten-step chain of §5.1 is a
//     table test (§3.1).
package secret

import (
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Domain is §4.4's fingerprint domain separator. It exists so that a
// fingerprint cannot be compared against a bare sha256 of the key computed
// elsewhere, and so two PayCLI-era hash schemes can never collide. It is
// redact.FingerprintDomain: there is exactly one formula (§4.4).
const Domain = redact.FingerprintDomain

// Anon is the fingerprint of the anonymous identity (§4.4, §8.1). It is a
// literal, not a hash: there is no credential to hash, and a fixed sentinel
// keeps anonymous discovery in its own cache scope.
const Anon = "anon"

// FingerprintLen is the number of hex characters every producer emits (§4.4).
// Sixteen everywhere — credentials.json, manifest.meta.key_fingerprint,
// manifest.identity.key_fingerprint, `pay auth status`, `pay cache ls`,
// `pay doctor` and §8.1's scope input — so a profile can always be matched to
// a cache scope by string equality.
const FingerprintLen = redact.FingerprintLen

// Fingerprint returns the first 16 hex characters of
// sha256("paycli-key-v1\x00" + credential) (§4.4).
//
// The raw credential is not derivable from the result: SHA-256 is preimage
// resistant and the output is truncated to 64 bits of a 256-bit digest. An
// empty credential yields Anon rather than the hash of the empty string, so a
// caller that forgot to check cannot publish a constant that looks like a real
// key fingerprint.
func Fingerprint(credential string) string {
	if credential == "" {
		return Anon
	}
	return redact.Fingerprint(credential)
}

// FingerprintFor is Fingerprint keyed by auth mode: the API key in api-key
// mode, the JWT in jwt mode, the literal "anon" in anonymous mode (§4.4).
func FingerprintFor(mode Mode, credential string) string {
	if mode == ModeAnonymous {
		return Anon
	}
	return Fingerprint(credential)
}
