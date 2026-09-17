package secret

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// ExpirySkew is §5.0's "within 60 s counts as expired" window. A token this
// close to its expiry is treated as absent, so PayCLI re-logs-in *before*
// sending — which is a fresh request and therefore safe for writes too.
const ExpirySkew = 60 * time.Second

// JWT is the credential record of §5.2: the minted token and its expiry, and
// nothing else. The password that produced it is never stored, anywhere, in
// any form.
type JWT struct {
	Token string
	Exp   time.Time
}

// NewJWT builds a record from Payload's login response, whose `exp` is Unix
// seconds. A zero or absent exp falls back to the token's own claim.
func NewJWT(token string, exp int64) JWT {
	j := JWT{Token: token}
	if exp > 0 {
		j.Exp = time.Unix(exp, 0).UTC()
		return j
	}
	if parsed, err := ParseJWT(token); err == nil {
		j.Exp = parsed.Exp
	}
	return j
}

// ParseJWT reads the `exp` claim out of a JWT without verifying the signature
// — PayCLI is not the issuer and cannot verify it; the server remains the only
// authority. The claim is used purely to decide whether it is worth sending.
//
// A token whose payload cannot be decoded is returned with a zero Exp rather
// than an error, because an opaque token is still worth sending: the server,
// not PayCLI, decides whether it is valid. A structurally non-JWT string is an
// error, since that is a configuration mistake.
func ParseJWT(token string) (JWT, error) {
	j := JWT{Token: token}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return j, apierr.New(apierr.CodeAuthInvalid,
			"stored token is not a JWT (expected three dot-separated segments)").
			WithHint("pay auth login --jwt --password-stdin")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// An undecodable payload means the expiry is unknown, not that the
		// token is unusable: only the server can reject it. Exp stays zero.
		return j, nil //nolint:nilerr // unknown expiry is not a failure
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return j, nil //nolint:nilerr // same: no parseable exp claim, expiry unknown
	}
	if claims.Exp > 0 && claims.Exp < math.MaxInt64 {
		sec, frac := math.Modf(claims.Exp)
		j.Exp = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	return j, nil
}

// Usable reports whether the token is worth sending at now: it must be
// non-empty and either have no known expiry or expire more than ExpirySkew
// from now (§5.0).
func (j JWT) Usable(now time.Time) bool {
	if j.Token == "" {
		return false
	}
	if j.Exp.IsZero() {
		return true
	}
	return j.Exp.After(now.Add(ExpirySkew))
}

// Expired is the negation of "not yet within the skew window", for reporting.
func (j JWT) Expired(now time.Time) bool {
	return !j.Exp.IsZero() && !j.Exp.After(now)
}

// ExpiresIn is the remaining lifetime, for `pay auth status`. It is zero when
// the expiry is unknown or already past.
func (j JWT) ExpiresIn(now time.Time) time.Duration {
	if j.Exp.IsZero() {
		return 0
	}
	d := j.Exp.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Fingerprint is the §4.4 fingerprint of the token itself.
func (j JWT) Fingerprint() string { return Fingerprint(j.Token) }
