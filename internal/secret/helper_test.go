package secret

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestHelperResolve(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		stdout   string
		stderr   string
		runErr   error
		want     string
		wantCode apierr.Code
	}{
		{name: "not configured", command: "", want: ""},
		{name: "trimmed stdout", command: "op read op://v/payload/key", stdout: "  s3cr3t \n", want: "s3cr3t"},
		{name: "first line only", command: "helper", stdout: "s3cr3t\nnote: rotated\n", want: "s3cr3t"},
		{name: "empty output continues the chain", command: "helper", stdout: "\n", want: ""},
		{
			name:     "non-zero exit",
			command:  "helper",
			stderr:   "op: not signed in",
			runErr:   errors.New("exit status 1"),
			wantCode: apierr.CodeAuthHelperFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotCommand string
			h := &Helper{Command: tc.command, Timeout: time.Second,
				Run: func(_ context.Context, cmd string) ([]byte, []byte, error) {
					gotCommand = cmd
					return []byte(tc.stdout), []byte(tc.stderr), tc.runErr
				}}
			got, err := h.Resolve(context.Background())
			if tc.wantCode != "" {
				if !apierr.HasCode(err, tc.wantCode) {
					t.Fatalf("err = %v, want %s", err, tc.wantCode)
				}
				if hint := apierr.From(err).Hint; tc.stderr != "" && !strings.Contains(hint, tc.stderr) {
					t.Errorf("hint = %q, want the stderr excerpt", hint)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("Resolve() = %q, want %q", got, tc.want)
			}
			if tc.command != "" && gotCommand != tc.command {
				t.Errorf("ran %q, want %q", gotCommand, tc.command)
			}
		})
	}
}

func TestHelperTimeoutIsApplied(t *testing.T) {
	h := &Helper{Command: "sleep 10", Timeout: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ string) ([]byte, []byte, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}}
	_, err := h.Resolve(context.Background())
	if !apierr.HasCode(err, apierr.CodeAuthHelperFailed) {
		t.Fatalf("err = %v, want auth_helper_failed", err)
	}
}

func TestHelperExcerptIsBounded(t *testing.T) {
	long := strings.Repeat("x", helperExcerpt*3)
	if got := excerpt([]byte(long)); len(got) > helperExcerpt+4 {
		t.Errorf("excerpt is %d bytes, want <= %d", len(got), helperExcerpt+4)
	}
	if excerpt([]byte("  \n ")) != "" {
		t.Error("blank stderr must produce no excerpt")
	}
	if got := excerpt([]byte("a\nb")); got != "a b" {
		t.Errorf("excerpt = %q, want newlines folded", got)
	}
}

// TestShellRunnerRunsTheRealShell is the one test that forks a process. It
// uses a builtin only, so it needs nothing installed, and it is skipped on
// Windows where the command form differs.
func TestShellRunnerRunsTheRealShell(t *testing.T) {
	if testing.Short() {
		t.Skip("forks a process")
	}
	stdout, _, err := shellRunner(context.Background(), "printf 'from-shell'")
	if err != nil {
		t.Skipf("no usable shell: %v", err)
	}
	if strings.TrimSpace(string(stdout)) != "from-shell" {
		t.Errorf("stdout = %q", stdout)
	}
}
