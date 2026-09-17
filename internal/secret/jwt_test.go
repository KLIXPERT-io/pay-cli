package secret

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// fakeJWT mints an unsigned, structurally valid JWT with the given exp. The
// signature is never checked: PayCLI is not the issuer (see ParseJWT).
func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := map[string]any{"id": "1", "collection": "users"}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".c2ln"
}

func TestParseJWT(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	exp := now.Add(2 * time.Hour)

	got, err := ParseJWT(fakeJWT(t, exp))
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if !got.Exp.Equal(exp) {
		t.Errorf("exp = %v, want %v", got.Exp, exp)
	}

	// No exp claim: unknown expiry, still usable — the server decides.
	got, err = ParseJWT(fakeJWT(t, time.Time{}))
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if !got.Exp.IsZero() || !got.Usable(now) {
		t.Errorf("a token without exp must be usable with a zero Exp, got %v", got.Exp)
	}

	// Structurally not a JWT.
	if _, err := ParseJWT("not-a-jwt"); !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Errorf("err = %v, want auth_invalid", err)
	}

	// Three segments but an undecodable payload: not an error, just unknown.
	if got, err := ParseJWT("aaa.!!!.ccc"); err != nil || !got.Exp.IsZero() {
		t.Errorf("got %v, %v; want a zero expiry and no error", got.Exp, err)
	}
}

func TestJWTUsable(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		token      JWT
		wantUsable bool
	}{
		{"fresh", JWT{Token: "t", Exp: now.Add(time.Hour)}, true},
		{"expired", JWT{Token: "t", Exp: now.Add(-time.Second)}, false},
		{"inside the 60s skew", JWT{Token: "t", Exp: now.Add(30 * time.Second)}, false},
		{"just outside the skew", JWT{Token: "t", Exp: now.Add(61 * time.Second)}, true},
		{"unknown expiry", JWT{Token: "t"}, true},
		{"no token", JWT{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.Usable(now); got != tc.wantUsable {
				t.Errorf("Usable() = %v, want %v", got, tc.wantUsable)
			}
		})
	}

	if d := (JWT{Token: "t", Exp: now.Add(time.Hour)}).ExpiresIn(now); d != time.Hour {
		t.Errorf("ExpiresIn = %v", d)
	}
	if d := (JWT{Token: "t", Exp: now.Add(-time.Hour)}).ExpiresIn(now); d != 0 {
		t.Errorf("ExpiresIn on an expired token = %v, want 0", d)
	}
	if !(JWT{Token: "t", Exp: now.Add(-time.Second)}).Expired(now) {
		t.Error("Expired() = false for a past expiry")
	}
	if (JWT{Token: "t"}).Expired(now) {
		t.Error("an unknown expiry must not count as expired")
	}
}

func TestNewJWTPrefersTheServerExp(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	serverExp := now.Add(3 * time.Hour)
	token := fakeJWT(t, now.Add(time.Hour))

	got := NewJWT(token, serverExp.Unix())
	if !got.Exp.Equal(serverExp) {
		t.Errorf("exp = %v, want the server's %v", got.Exp, serverExp)
	}

	got = NewJWT(token, 0)
	if !got.Exp.Equal(now.Add(time.Hour)) {
		t.Errorf("exp = %v, want the claim's %v", got.Exp, now.Add(time.Hour))
	}
}
