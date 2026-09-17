package output

import (
	"fmt"
	"io"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// ErrorsTo selects which stream carries the error envelope (§11.1). The
// default is stdout — an agent reads one stream and branches on .ok — and
// PAY_ERRORS_TO=stderr / --errors-to stderr moves it for people who script
// around the Unix convention.
type ErrorsTo string

const (
	ErrorsToStdout ErrorsTo = "stdout"
	ErrorsToStderr ErrorsTo = "stderr"
)

// ParseErrorsTo validates --errors-to.
func ParseErrorsTo(s string) (ErrorsTo, error) {
	switch ErrorsTo(s) {
	case "", ErrorsToStdout:
		return ErrorsToStdout, nil
	case ErrorsToStderr:
		return ErrorsToStderr, nil
	default:
		return "", apierr.New(apierr.CodeInvalidOption,
			"%q is not a valid --errors-to value. Valid values: stdout, stderr.", s)
	}
}

// Renderer writes one envelope to its destination and returns the process exit
// status implied by it. internal/cli/app.go is the only place that owns the
// process's real standard streams; everything else is handed a Renderer, which
// is what makes CLI-level golden testing possible at all.
type Renderer interface {
	Render(env *Envelope) (exitCode int, err error)
}

// Writer is the concrete Renderer.
type Writer struct {
	Stdout io.Writer
	Stderr io.Writer

	// Format is --output. Zero value renders as json.
	Format Format
	// ErrorsTo is --errors-to.
	ErrorsTo ErrorsTo
	// Columns overrides the default column selection for csv and table.
	Columns []string
	// Width is the terminal width for table output; 0 uses DefaultWidth.
	Width int
	// Human adds the one-line stderr summary on an error (§11.1). It is on by
	// default; --quiet turns it off.
	Quiet bool
	// NoRedact is --no-redact. The zero value redacts, so a Writer built
	// anywhere (including in a test) can never be the thing that spills a
	// credential (§5.3).
	NoRedact bool
	// StdoutIsData says the command has already written raw bytes to Stdout
	// and owns that stream (today: `pay download -o -`). The envelope is then
	// rendered to Stderr, because appending JSON after a PNG produces a file
	// that is neither: a 463-byte image came out as a 1570-byte hybrid. It is
	// deliberately a writer-level flag rather than a per-command dance, so
	// every future byte-streaming command gets the same protection.
	StdoutIsData bool
}

// envelopeOut is the stream the envelope itself belongs on.
func (w *Writer) envelopeOut() io.Writer {
	if w.StdoutIsData && w.Stderr != nil {
		return w.Stderr
	}
	return w.Stdout
}

var _ Renderer = (*Writer)(nil)

// Render writes env and returns the exit status it implies. Format/data_kind
// compatibility is checked here, so an unsupported combination fails loudly
// with format_unsupported (exit 5) instead of silently falling back to JSON.
func (w *Writer) Render(env *Envelope) (int, error) {
	if env == nil {
		return apierr.ExitInternal, fmt.Errorf("output: nil envelope")
	}
	format := w.Format
	if format == "" {
		format = FormatJSON
	}
	// §5.3: redaction applies to ALL output, in every format. It runs here,
	// once, so no renderer and no command can bypass it. The CLI redacts
	// earlier as well (before --path is evaluated); this pass is the backstop
	// and is a no-op when nothing is left to mask.
	if !w.NoRedact {
		if paths := env.RedactData(); len(paths) > 0 {
			env.AddWarning(DataRedactedWarning(paths))
		}
	}
	if env.OK {
		// A --path selection that left a bare scalar is renderable by id
		// whatever the command's data_kind was: there is no structure left for
		// the matrix to object to. Without this, the registry's own
		// `pay doctor --path .ok --output id` is an example that cannot run.
		if format == FormatID && env.NarrowedToScalar() {
			if err := w.renderSuccess(env, format); err != nil {
				return apierr.ExitInternal, err
			}
			return env.ExitCode(), nil
		}
		if err := CheckFormat(format, env.DataKind); err != nil {
			// The rendering of the failure itself must always work, so it goes
			// out as JSON regardless of what was asked for.
			return w.renderErrorEnvelope(NewError(env.Command, err), FormatJSON)
		}
	} else {
		// An error envelope is always JSON-shaped; csv/table/id cannot express
		// it and raw has no body to print.
		if format != FormatJSONL {
			format = FormatJSON
		}
	}

	if !env.OK {
		return w.renderErrorEnvelope(env, format)
	}
	if err := w.renderSuccess(env, format); err != nil {
		return apierr.ExitInternal, err
	}
	return env.ExitCode(), nil
}

func (w *Writer) renderSuccess(env *Envelope, format Format) error {
	out := w.envelopeOut()
	switch format {
	case FormatJSON:
		return renderJSON(out, env)
	case FormatJSONL:
		return renderJSONL(out, w.Stderr, env)
	case FormatID:
		return renderID(out, env)
	case FormatCSV:
		return renderCSV(out, env, w.Columns)
	case FormatTable:
		return renderTable(out, env, w.Columns, w.Width)
	case FormatRaw:
		return w.renderRaw(env)
	default:
		return renderJSON(out, env)
	}
}

// renderRaw prints Payload's body with no envelope — AFTER redaction. When
// redaction actually altered the bytes, a one-line stderr notice names
// --no-redact (§10.3). The escape hatch survives; it is no longer the default
// way to spill a credential into an agent transcript.
func (w *Writer) renderRaw(env *Envelope) error {
	body := env.RawBody
	if body == nil {
		// No wire body was captured (a locally-produced result): fall back to
		// the data value so --output raw is never empty.
		b, err := marshalIndent(env.Data)
		if err != nil {
			return err
		}
		body = b
	}
	if err := writeLine(w.envelopeOut(), body); err != nil {
		return err
	}
	if env.RawRedacted && !w.Quiet {
		_, err := io.WriteString(w.Stderr,
			"pay: this body was redacted before printing; re-run with --no-redact for the true bytes.\n")
		return err
	}
	return nil
}

func (w *Writer) renderErrorEnvelope(env *Envelope, format Format) (int, error) {
	env.PropagateRawRedaction()

	dst := w.envelopeOut()
	if w.ErrorsTo == ErrorsToStderr {
		dst = w.Stderr
	}
	var err error
	if format == FormatJSONL {
		// In jsonl the envelope already lives on stderr; keep that invariant so
		// the data file stays clean even on failure.
		dst = w.Stderr
		var b []byte
		b, err = env.Compact()
		if err == nil {
			err = writeLine(w.Stderr, b)
		}
	} else {
		err = renderJSON(dst, env)
	}
	if err != nil {
		return apierr.ExitInternal, err
	}
	// The one-line human summary always goes to stderr, never to the stream
	// carrying the envelope.
	if !w.Quiet && env.Error != nil && dst != w.Stderr {
		if _, werr := io.WriteString(w.Stderr, apierr.HumanLine(env.Error)+"\n"); werr != nil {
			return apierr.ExitInternal, werr
		}
	}
	return env.ExitCode(), nil
}
