package secret

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// HelperTimeout bounds the credential helper (§5.1 step 9). Ten seconds is
// enough for `op read`, a Vault round trip or an AWS Secrets Manager call, and
// short enough that a hung helper does not hang an agent.
const HelperTimeout = 10 * time.Second

// helperExcerpt caps how much of a failed helper's stderr is quoted back. The
// excerpt goes through apierr's redaction, but a short excerpt also limits the
// blast radius of a helper that prints the secret on failure.
const helperExcerpt = 200

// HelperRunner executes the helper. It is a field so tests never fork a
// process.
type HelperRunner func(ctx context.Context, command string) (stdout, stderr []byte, err error)

// Helper is the `credential_helper` exec hook: a shell command whose stdout is
// the credential (§5.1 step 9). This is how 1Password, Vault and AWS Secrets
// Manager are supported with zero extra Go dependencies.
type Helper struct {
	Command string
	Timeout time.Duration
	Run     HelperRunner
}

// NewHelper builds a helper bound to the real shell.
func NewHelper(command string) *Helper {
	return &Helper{Command: command, Timeout: HelperTimeout, Run: shellRunner}
}

// Configured reports whether there is anything to run.
func (h *Helper) Configured() bool { return h != nil && strings.TrimSpace(h.Command) != "" }

// Resolve runs the helper and returns its trimmed stdout.
//
// A zero exit with empty stdout is "no credential here", not a failure: the
// chain continues to the next step. A non-zero exit is auth_helper_failed
// (exit 2), because a helper that was configured and broke is a real problem
// the caller must see.
func (h *Helper) Resolve(ctx context.Context) (string, error) {
	if !h.Configured() {
		return "", nil
	}
	runner := h.Run
	if runner == nil {
		runner = shellRunner
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = HelperTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, err := runner(ctx, h.Command)
	if err != nil {
		e := apierr.Wrap(err, apierr.CodeAuthHelperFailed,
			"credential_helper failed: %v", err).
			WithHint("check the command with: %s", shellName()+" -c '<credential_helper>'")
		if excerpt := excerpt(stderr); excerpt != "" {
			e = e.WithHint("credential_helper stderr: %s", excerpt)
		}
		return "", e
	}
	// Only the first line is the credential: helpers commonly append a
	// trailing newline, and some print a hint after it.
	out := strings.TrimSpace(string(stdout))
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = strings.TrimSpace(out[:i])
	}
	return out, nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > helperExcerpt {
		s = s[:helperExcerpt] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func shellName() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

// shellRunner runs the command through the platform shell: `sh -c` as §5.1
// specifies, `cmd /c` on Windows where sh does not exist.
func shellRunner(ctx context.Context, command string) ([]byte, []byte, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Stdin is never connected: a helper that wants to prompt must fail
	// rather than block an agent forever.
	cmd.Stdin = nil
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}
