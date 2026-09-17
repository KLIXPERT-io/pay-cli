package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

// Outcome is §15.3's three-state verification result. There are exactly three:
// collapsing "no evidence" and "evidence that disagrees" into one boolean is
// what makes a compromised release installable.
type Outcome string

const (
	// OutcomeVerified — a signature or attestation is present and matches.
	OutcomeVerified Outcome = "verified"
	// OutcomeUnverifiable — no evidence either way: cosign is absent and the
	// attestation API returned non-200 or timed out. Proceed with a loud
	// warning unless PAY_UPDATE_STRICT=1.
	OutcomeUnverifiable Outcome = "unverifiable"
	// OutcomeFailed — a checksum, signature or attestation IS present and does
	// not match. Always abort; PAY_UPDATE_STRICT cannot relax this.
	OutcomeFailed Outcome = "verification_failed"
)

// Verification is one verification step's result.
type Verification struct {
	Outcome Outcome `json:"outcome"`
	// Method is "checksum", "cosign" or "attestation".
	Method string `json:"method,omitempty"`
	// Reason explains an unverifiable or failed outcome in one line.
	Reason string `json:"reason,omitempty"`
	// Expected and Computed are the digests, printed on a mismatch.
	Expected string `json:"expected,omitempty"`
	Computed string `json:"computed,omitempty"`
}

// Combine folds several steps into the overall outcome: any failure dominates;
// otherwise every step must be verified for the whole to be verified.
func Combine(results ...Verification) Verification {
	worst := Verification{Outcome: OutcomeVerified}
	for _, r := range results {
		switch r.Outcome {
		case OutcomeFailed:
			return r
		case OutcomeUnverifiable:
			if worst.Outcome != OutcomeUnverifiable {
				worst = r
			}
		}
	}
	return worst
}

// Enforce turns the outcome into the §15.3 behaviour. It returns nil when the
// update may proceed.
//
// A failed verification always aborts with exit 1 update_verification_failed,
// strict or not: a mismatching signature is positive evidence of exactly the
// compromised release this check exists to defend against.
func (v Verification) Enforce(strict bool) error {
	switch v.Outcome {
	case OutcomeFailed:
		e := apierr.New(apierr.CodeUpdateVerificationFailed,
			"the downloaded release failed %s verification: %s", methodOr(v.Method, "signature"), v.Reason)
		if v.Expected != "" || v.Computed != "" {
			e = e.WithHint("expected %s, computed %s. The artefact was deleted. Re-run `pay update-self --check`; if it persists, do NOT install this release and report it.",
				orUnknown(v.Expected), orUnknown(v.Computed))
		} else {
			e = e.WithHint("the artefact was deleted. Do NOT install this release manually; report it.")
		}
		return e
	case OutcomeUnverifiable:
		if strict {
			return apierr.New(apierr.CodeUpdateVerificationFailed,
				"the release could not be verified (%s) and PAY_UPDATE_STRICT is set.", v.Reason).
				WithHint("install cosign, or unset PAY_UPDATE_STRICT to accept an unverified update.")
		}
		return nil
	default:
		return nil
	}
}

// WarnLine is the loud stderr line printed for an unverifiable outcome.
func (v Verification) WarnLine() string {
	if v.Outcome != OutcomeUnverifiable {
		return ""
	}
	return fmt.Sprintf("pay: WARNING: this release could not be verified (%s). "+
		"The checksum matched, but nothing proves the checksum file itself is genuine. "+
		"Install cosign for a real signature check, or set PAY_UPDATE_STRICT=1 to refuse unverified updates.", v.Reason)
}

func methodOr(method, fallback string) string {
	if method == "" {
		return fallback
	}
	return method
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ParseChecksums parses a goreleaser checksums.txt ("<sha256>  <filename>").
func ParseChecksums(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*") // BSD-style marker
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

// Digest is the lowercase hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifyChecksum compares the artefact against checksums.txt.
//
// A missing entry is UNVERIFIABLE (no evidence), a mismatching one is FAILED
// (evidence that disagrees).
func VerifyChecksum(assetName string, artifact, checksums []byte) Verification {
	sums := ParseChecksums(checksums)
	want, ok := sums[assetName]
	got := Digest(artifact)
	switch {
	case !ok:
		return Verification{
			Outcome: OutcomeUnverifiable,
			Method:  "checksum",
			Reason:  "checksums.txt has no entry for " + assetName,
		}
	case want != got:
		return Verification{
			Outcome:  OutcomeFailed,
			Method:   "checksum",
			Reason:   fmt.Sprintf("the SHA-256 of %s does not match checksums.txt", assetName),
			Expected: want,
			Computed: got,
		}
	default:
		return Verification{Outcome: OutcomeVerified, Method: "checksum"}
	}
}

// Signer verifies checksums.txt itself: cosign when it is on PATH, otherwise
// the GitHub attestation API. Both hooks are injectable so tests never shell
// out or hit the network.
type Signer struct {
	// LookPath finds cosign. nil uses exec.LookPath.
	LookPath func(string) (string, error)
	// Run executes cosign. nil uses os/exec.
	Run func(ctx context.Context, name string, args ...string) error
	// HTTP is the client used for the attestation API.
	HTTP *http.Client
	// APIBase is the GitHub API base URL.
	APIBase string
	// Repo is "owner/name".
	Repo string
	// Token is GH_TOKEN, if any.
	Token string
	// Identity and Issuer pin the expected signing identity for cosign.
	Identity string
	Issuer   string
}

// DefaultIdentityRegexp matches this repository's GitHub Actions workflow
// identity, which is what §16.1's cosign step signs with.
const DefaultIdentityRegexp = `^https://github\.com/KLIXPERT-io/pay-cli/\.github/workflows/.+$`

// DefaultIssuer is GitHub's OIDC issuer.
const DefaultIssuer = "https://token.actions.githubusercontent.com"

// VerifySignature checks checksums.txt against its cosign signature, falling
// back to the GitHub attestation API.
//
// sig and pem may be empty: that is the "release was not signed" case, which is
// unverifiable rather than failed. A signature that IS present and does not
// verify is failed.
func (s Signer) VerifySignature(ctx context.Context, checksums, sig, pem []byte) Verification {
	if len(sig) > 0 && len(pem) > 0 {
		lookPath := s.LookPath
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		cosign, err := lookPath("cosign")
		if err == nil && cosign != "" {
			return s.cosign(ctx, cosign, checksums, sig, pem)
		}
		// cosign is absent: the signature exists but cannot be checked here.
		// Fall through to the attestation API, which needs no local tool.
	}
	return s.attestation(ctx, checksums)
}

// cosign runs `cosign verify-blob` against temp copies of the three files.
func (s Signer) cosign(ctx context.Context, cosignPath string, checksums, sig, pem []byte) Verification {
	dir, err := os.MkdirTemp("", "pay-cosign-")
	if err != nil {
		return Verification{Outcome: OutcomeUnverifiable, Method: "cosign", Reason: "cannot create a temp dir: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	blob := filepath.Join(dir, "checksums.txt")
	sigPath := filepath.Join(dir, "checksums.txt.sig")
	pemPath := filepath.Join(dir, "checksums.txt.pem")
	for path, data := range map[string][]byte{blob: checksums, sigPath: sig, pemPath: pem} {
		if err := fsatomic.Write(path, data, 0o600); err != nil {
			return Verification{Outcome: OutcomeUnverifiable, Method: "cosign", Reason: "cannot stage the signature files: " + err.Error()}
		}
	}

	identity := s.Identity
	if identity == "" {
		identity = DefaultIdentityRegexp
	}
	issuer := s.Issuer
	if issuer == "" {
		issuer = DefaultIssuer
	}
	run := s.Run
	if run == nil {
		run = runCommand
	}
	args := []string{
		"verify-blob",
		"--certificate", pemPath,
		"--signature", sigPath,
		"--certificate-identity-regexp", identity,
		"--certificate-oidc-issuer", issuer,
		blob,
	}
	if err := run(ctx, cosignPath, args...); err != nil {
		return Verification{
			Outcome: OutcomeFailed,
			Method:  "cosign",
			Reason:  "cosign verify-blob rejected checksums.txt: " + err.Error(),
		}
	}
	return Verification{Outcome: OutcomeVerified, Method: "cosign"}
}

// runCommand is the default Signer.Run.
func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		excerpt := strings.TrimSpace(string(out))
		if len(excerpt) > 200 {
			excerpt = excerpt[:200]
		}
		if excerpt != "" {
			return fmt.Errorf("%w: %s", err, excerpt)
		}
		return err
	}
	return nil
}

// attestation asks GitHub whether it has an attestation for the checksums file.
// Anything other than a 200 with at least one bundle is UNVERIFIABLE: a 404
// means the release was never attested, which is absence of evidence.
func (s Signer) attestation(ctx context.Context, checksums []byte) Verification {
	if s.HTTP == nil || s.APIBase == "" || s.Repo == "" {
		return Verification{Outcome: OutcomeUnverifiable, Method: "attestation", Reason: "cosign is not installed and no attestation endpoint is configured"}
	}
	digest := Digest(checksums)
	url := fmt.Sprintf("%s/repos/%s/attestations/sha256:%s", strings.TrimSuffix(s.APIBase, "/"), s.Repo, digest)

	// §3.1 note: this is the GitHub release API, not the Payload API. It
	// deliberately does not go through internal/payload's transport, which
	// injects Payload credentials and Accept-Language.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Verification{Outcome: OutcomeUnverifiable, Method: "attestation", Reason: "cannot build the attestation request"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return Verification{Outcome: OutcomeUnverifiable, Method: "attestation", Reason: "the attestation API is unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Verification{
			Outcome: OutcomeUnverifiable,
			Method:  "attestation",
			Reason:  fmt.Sprintf("cosign is not installed and the attestation API returned HTTP %d", resp.StatusCode),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Verification{Outcome: OutcomeUnverifiable, Method: "attestation", Reason: "the attestation response could not be read"}
	}
	var payload struct {
		Attestations []json.RawMessage `json:"attestations"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Attestations) == 0 {
		return Verification{Outcome: OutcomeUnverifiable, Method: "attestation", Reason: "the attestation API returned no attestation for this artefact"}
	}
	return Verification{Outcome: OutcomeVerified, Method: "attestation"}
}
