package update

import (
	"os/exec"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// ApplyArgs is the command line the detached child runs.
var ApplyArgs = []string{"update-self", "--apply", "--quiet"}

// SpawnDetached starts exe fully detached from this process: no shared stdio,
// its own session (unix) or process group (Windows), and the handle released
// immediately.
//
// This is §15.1's implicit apply. It is bounded at the fork — no network in the
// foreground, no syscall.Exec, no fall-through — and the swapped binary takes
// effect on the NEXT invocation. An in-process swap would leave the running
// image executing the old command surface while reporting the new version.
func SpawnDetached(exe string, args, env []string) error {
	if exe == "" {
		return apierr.New(apierr.CodeInternal, "cannot spawn the update child: the binary path is unknown.")
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	// Detached means detached: the child must not hold this process's stdio
	// open, or a shell pipeline would block waiting for it.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot start the background update: %v", err)
	}
	if cmd.Process != nil {
		// Release, never Wait: the parent returns immediately.
		return cmd.Process.Release()
	}
	return nil
}

// SpawnApply starts the detached `pay update-self --apply` child for the
// implicit path. env must already contain PAY_NO_UPDATE=1 so the child cannot
// recurse into spawning another one.
func (u *Updater) SpawnApply(env []string) error {
	u.normalise()
	exe := u.BinaryPath
	if exe == "" {
		resolved, err := ResolveBinary()
		if err != nil {
			return apierr.Wrap(err, apierr.CodeInternal, "cannot resolve the running binary: %v", err)
		}
		exe = resolved
	}
	return SpawnDetached(exe, ApplyArgs, env)
}

// NoRecurseEnv is the variable the spawned child must see so it does not spawn
// a child of its own.
const NoRecurseEnv = "PAY_NO_UPDATE=1"
