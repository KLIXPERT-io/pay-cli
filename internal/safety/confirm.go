package safety

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Request is one confirmation question.
type Request struct {
	// Op is the operation being confirmed.
	Op Op
	// Target is the collection or global slug, used in the summary line.
	Target string
	// Affected is the resolved match count for a bulk verb, or 1 for a scoped
	// one. A negative value means "not resolved".
	Affected int
	// Summary overrides the generated one-line description. It must never
	// contain a credential; callers pass slugs and counts, not request bodies.
	Summary string
}

// Line is the one-line description printed before prompting and, for L3,
// printed even when --yes was passed (§12.1: bulk "always prints the resolved
// match count before acting").
func (r Request) Line() string {
	if r.Summary != "" {
		return r.Summary
	}
	verb := strings.TrimSpace(r.Op.Command)
	if r.Op.SoftDelete() {
		verb = "trash"
	}
	target := r.Target
	if target == "" {
		target = "the target"
	} else {
		target = fmt.Sprintf("%q", target)
	}
	switch {
	case r.Op.Selector == SelectorBulk && r.Affected >= 0:
		return fmt.Sprintf("%s will affect %d document(s) in %s", verb, r.Affected, target)
	case r.Op.Selector == SelectorBulk:
		return fmt.Sprintf("%s will affect every document matching --where in %s", verb, target)
	case r.Op.Global:
		return fmt.Sprintf("%s will modify the global %s", verb, target)
	default:
		return fmt.Sprintf("%s will affect 1 document in %s", verb, target)
	}
}

// Confirmer applies the §12.1 prompting policy. It writes only to Out (stderr
// in the real CLI) so the stdout envelope stays parseable, and it reads only
// from In.
type Confirmer struct {
	// In is the confirmation input stream (stdin).
	In io.Reader
	// Out receives prompts, the bulk match count and the irreversibility
	// warning. This is stderr — never stdout.
	Out io.Writer
	// TTY is true when both In and Out are a terminal. It is computed by the
	// caller (only app.go may look at the real file descriptors).
	TTY bool
	// AssumeYes is --yes or PAY_YES=1.
	AssumeYes bool
	// ConfirmWrites is defaults.confirm_writes: opt in to prompting on L1.
	ConfirmWrites bool
	// Quiet suppresses the informational lines. It never suppresses a prompt,
	// because a prompt with no question is unanswerable.
	Quiet bool
	// DryRun short-circuits every prompt: --dry-run performs the read half
	// only, so there is nothing to confirm (§12.2).
	DryRun bool

	reader *bufio.Reader
}

// Confirm returns nil when the operation may proceed, and a
// confirmation_required error (exit 11) when it may not.
func (c *Confirmer) Confirm(req Request) error {
	level := req.Op.Level()
	if level == L0 {
		return nil
	}

	// A dry run skips the QUESTION, not the disclosure (§12.2 suppresses the
	// prompt; §12.4 says an irreversible delete is announced "whether or not
	// it also prompts"). Returning early here meant the preview an agent is
	// told to read before passing --yes was the one place the irreversibility
	// note and the resolved match count never appeared.
	prefix := "pay:"
	if c.DryRun {
		prefix = "pay: (dry run)"
	}
	if req.Op.Irreversible() {
		c.notef("%s %s is IRREVERSIBLE — %s.", prefix, strings.TrimSpace(req.Op.Command), req.Op.IrreversibleReason())
	}

	needs := level >= L2 || (level == L1 && c.ConfirmWrites)
	if level == L3 {
		// Always printed, even when --yes skips the question.
		c.notef("%s %s", prefix, req.Line())
	}
	if c.DryRun || !needs {
		return nil
	}
	if c.AssumeYes {
		return nil
	}
	if !c.TTY {
		return c.required(req, level)
	}
	ok, err := c.ask(req)
	if err != nil {
		return c.required(req, level)
	}
	if !ok {
		return apierr.New(apierr.CodeConfirmationRequired,
			"aborted: %s was not confirmed.", strings.TrimSpace(req.Op.Command)).
			WithHint("nothing was sent. Re-run with --yes to skip the prompt, or --dry-run to see exactly what would change.")
	}
	return nil
}

// required builds the non-TTY refusal.
func (c *Confirmer) required(req Request, level Level) error {
	return apierr.New(apierr.CodeConfirmationRequired,
		"%s is a %s (%s) operation and stdin is not a terminal: %s.",
		strings.TrimSpace(req.Op.Command), level.String(), level.Name(), req.Line()).
		WithHint("pass --yes (or set PAY_YES=1) to confirm non-interactively, or --dry-run to see exactly what would change first.")
}

// ask prints the question and reads one line.
func (c *Confirmer) ask(req Request) (bool, error) {
	if c.Out != nil {
		fmt.Fprintf(c.Out, "pay: %s\nProceed? [y/N] ", req.Line())
	}
	if c.In == nil {
		return false, io.EOF
	}
	if c.reader == nil {
		c.reader = bufio.NewReader(c.In)
	}
	line, err := c.reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// notef writes an informational line to Out unless --quiet.
func (c *Confirmer) notef(format string, args ...any) {
	if c.Quiet || c.Out == nil {
		return
	}
	fmt.Fprintf(c.Out, format+"\n", args...)
}
