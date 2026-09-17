package secret

import (
	"strings"
	"testing"
)

// TestFingerprintIsTheSpecFormula pins §4.4's formula with a value computed
// outside Go: sha256("paycli-key-v1\x00" + credential)[:16 hex chars]. If the
// domain separator, the hash or the truncation ever changes, every cache scope
// (§8.1) silently invalidates — so the constant is the test.
func TestFingerprintIsTheSpecFormula(t *testing.T) {
	tests := []struct {
		credential string
		want       string
	}{
		{"example-api-key", "8c1234bcdaa31797"},
		{"minted-token", "833c4b8543a60e91"},
	}
	for _, tc := range tests {
		if got := Fingerprint(tc.credential); got != tc.want {
			t.Errorf("Fingerprint(%q) = %q, want %q", tc.credential, got, tc.want)
		}
	}
}

// TestFingerprintLengthIsSixteen is §17.1's named test: every producer emits
// the identical 16 hex characters for a fixed input.
func TestFingerprintLengthIsSixteen(t *testing.T) {
	producers := map[string]string{
		"Fingerprint":          Fingerprint("example-api-key"),
		"FingerprintFor":       FingerprintFor(ModeAPIKey, "example-api-key"),
		"JWT.Fingerprint":      JWT{Token: "example-api-key"}.Fingerprint(),
		"recordFingerprint":    recordFingerprint(&Record{AuthMode: ModeAPIKey, APIKey: "example-api-key"}),
		"recordFingerprintJWT": recordFingerprint(&Record{AuthMode: ModeJWT, Token: "example-api-key"}),
		"chain":                apiKey("example-api-key", SourceFlag, nil).Fingerprint,
	}
	const want = "8c1234bcdaa31797"
	for name, got := range producers {
		if len(got) != FingerprintLen {
			t.Errorf("%s produced %d characters, want %d", name, len(got), FingerprintLen)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestFingerprintIsHexAndOpaque(t *testing.T) {
	const key = "paycli-example-key-0123456789"
	fp := Fingerprint(key)
	if strings.Contains(fp, key) || strings.Contains(key, fp) {
		t.Fatal("the fingerprint must not embed the credential")
	}
	for _, c := range fp {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex character %q in %q", c, fp)
		}
	}
	if Fingerprint(key) != Fingerprint(key) {
		t.Fatal("Fingerprint is not deterministic")
	}
	if Fingerprint(key) == Fingerprint(key+"x") {
		t.Fatal("distinct credentials collided")
	}
}

func TestAnonymousFingerprint(t *testing.T) {
	if got := Fingerprint(""); got != Anon {
		t.Errorf("Fingerprint(\"\") = %q, want %q", got, Anon)
	}
	if got := FingerprintFor(ModeAnonymous, "still-a-key"); got != Anon {
		t.Errorf("anonymous mode must ignore any credential, got %q", got)
	}
	if got := recordFingerprint(&Record{AuthMode: ModeAnonymous}); got != Anon {
		t.Errorf("recordFingerprint = %q", got)
	}
}
