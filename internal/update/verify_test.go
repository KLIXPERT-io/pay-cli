package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestParseChecksums(t *testing.T) {
	in := []byte("" +
		"aa11  pay_1.2.3_linux_amd64.tar.gz\n" +
		"BB22 *pay_1.2.3_windows_amd64.zip\n" +
		"\n" +
		"garbage line with three fields here\n")
	got := ParseChecksums(in)
	if got["pay_1.2.3_linux_amd64.tar.gz"] != "aa11" {
		t.Errorf("linux entry = %q", got["pay_1.2.3_linux_amd64.tar.gz"])
	}
	if got["pay_1.2.3_windows_amd64.zip"] != "bb22" {
		t.Errorf("windows entry = %q", got["pay_1.2.3_windows_amd64.zip"])
	}
	if len(got) != 2 {
		t.Errorf("entries = %v", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	artifact := []byte("archive bytes")
	digest := Digest(artifact)
	const asset = "pay_1.2.3_linux_amd64.tar.gz"

	tests := []struct {
		name      string
		checksums string
		want      Outcome
	}{
		{"match", digest + "  " + asset + "\n", OutcomeVerified},
		{"mismatch is positive evidence", strings.Repeat("0", 64) + "  " + asset + "\n", OutcomeFailed},
		{"no entry is absence of evidence", digest + "  other_asset.tar.gz\n", OutcomeUnverifiable},
		{"empty file", "", OutcomeUnverifiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyChecksum(asset, artifact, []byte(tc.checksums))
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%+v)", got.Outcome, tc.want, got)
			}
			if got.Outcome == OutcomeFailed && (got.Expected == "" || got.Computed == "") {
				t.Error("a failed checksum must report expected vs computed")
			}
		})
	}
}

func TestCombine(t *testing.T) {
	v := Verification{Outcome: OutcomeVerified}
	u := Verification{Outcome: OutcomeUnverifiable, Reason: "no cosign"}
	f := Verification{Outcome: OutcomeFailed, Reason: "mismatch"}

	tests := []struct {
		name string
		in   []Verification
		want Outcome
	}{
		{"all verified", []Verification{v, v}, OutcomeVerified},
		{"one unverifiable", []Verification{v, u}, OutcomeUnverifiable},
		{"failure dominates unverifiable", []Verification{u, f}, OutcomeFailed},
		{"failure dominates verified", []Verification{v, f}, OutcomeFailed},
		{"nothing at all", nil, OutcomeVerified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Combine(tc.in...); got.Outcome != tc.want {
				t.Fatalf("Combine() = %q, want %q", got.Outcome, tc.want)
			}
		})
	}
}

func TestEnforce(t *testing.T) {
	tests := []struct {
		name    string
		v       Verification
		strict  bool
		wantErr bool
	}{
		{"verified proceeds", Verification{Outcome: OutcomeVerified}, false, false},
		{"verified proceeds under strict", Verification{Outcome: OutcomeVerified}, true, false},
		{"unverifiable proceeds by default", Verification{Outcome: OutcomeUnverifiable, Reason: "no cosign"}, false, false},
		{"unverifiable aborts under strict", Verification{Outcome: OutcomeUnverifiable, Reason: "no cosign"}, true, true},
		{"failed always aborts", Verification{Outcome: OutcomeFailed, Reason: "mismatch"}, false, true},
		{"failed aborts under strict too", Verification{Outcome: OutcomeFailed, Reason: "mismatch"}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Enforce(tc.strict)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Enforce() = %v, want nil", err)
				}
				return
			}
			if apierr.CodeOf(err) != apierr.CodeUpdateVerificationFailed {
				t.Fatalf("code = %s, want update_verification_failed", apierr.CodeOf(err))
			}
			if apierr.ExitCode(err) != apierr.ExitInternal {
				t.Fatalf("exit = %d, want 1", apierr.ExitCode(err))
			}
		})
	}
}

func TestWarnLine(t *testing.T) {
	if line := (Verification{Outcome: OutcomeUnverifiable, Reason: "cosign is not installed"}).WarnLine(); !strings.Contains(line, "WARNING") {
		t.Fatalf("WarnLine() = %q", line)
	}
	if line := (Verification{Outcome: OutcomeVerified}).WarnLine(); line != "" {
		t.Fatalf("a verified release must not warn: %q", line)
	}
}

func TestSignerCosign(t *testing.T) {
	checksums := []byte("aa  pay.tar.gz\n")
	sig := []byte("signature")
	pem := []byte("certificate")

	tests := []struct {
		name    string
		hasCos  bool
		runErr  error
		want    Outcome
		wantWhy string
	}{
		{"cosign verifies", true, nil, OutcomeVerified, "cosign"},
		{"cosign rejects", true, errors.New("bad signature"), OutcomeFailed, "cosign"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			s := Signer{
				LookPath: func(string) (string, error) { return "/usr/bin/cosign", nil },
				Run: func(_ context.Context, name string, args ...string) error {
					gotArgs = append([]string{name}, args...)
					return tc.runErr
				},
			}
			got := s.VerifySignature(context.Background(), checksums, sig, pem)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%+v)", got.Outcome, tc.want, got)
			}
			if got.Method != tc.wantWhy {
				t.Errorf("method = %q, want %q", got.Method, tc.wantWhy)
			}
			joined := strings.Join(gotArgs, " ")
			for _, want := range []string{"verify-blob", "--certificate", "--signature", "--certificate-oidc-issuer"} {
				if !strings.Contains(joined, want) {
					t.Errorf("cosign args %q missing %q", joined, want)
				}
			}
		})
	}
}

func TestSignerAttestationFallback(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    Outcome
		noCheck bool
	}{
		{"attested", http.StatusOK, `{"attestations":[{"bundle":{}}]}`, OutcomeVerified, false},
		{"not attested", http.StatusNotFound, `{}`, OutcomeUnverifiable, false},
		{"rate limited", http.StatusForbidden, `{}`, OutcomeUnverifiable, false},
		{"empty list", http.StatusOK, `{"attestations":[]}`, OutcomeUnverifiable, false},
		{"no endpoint configured", 0, "", OutcomeUnverifiable, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Signer{
				LookPath: func(string) (string, error) { return "", errors.New("not found") },
				Repo:     "KLIXPERT-io/pay-cli",
			}
			if !tc.noCheck {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.Contains(r.URL.Path, "/attestations/sha256:") {
						t.Errorf("unexpected path %q", r.URL.Path)
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer srv.Close()
				s.HTTP = srv.Client()
				s.APIBase = srv.URL
			}
			got := s.VerifySignature(context.Background(), []byte("checksums"), []byte("sig"), []byte("pem"))
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%+v)", got.Outcome, tc.want, got)
			}
		})
	}
}

func TestVerificationJSON(t *testing.T) {
	b, err := json.Marshal(Verification{Outcome: OutcomeFailed, Method: "checksum", Reason: "mismatch"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"outcome":"verification_failed"`) {
		t.Fatalf("json = %s", b)
	}
}
