// Package logging builds PayCLI's slog logger.
//
// Two rules shape it:
//
//   - The destination is INJECTED. Only internal/cli/app.go may touch the
//     process's standard streams (§3.1), so a logger is constructed with an
//     io.Writer and every log line a command produces is capturable by a test.
//   - Every attribute passes through internal/redact before it is formatted.
//     `[logging] level = "debug"` / PAY_LOG_LEVEL=debug is the documented way
//     to see what PayCLI sent, and the Authorization header is synthesised by
//     the transport rather than read from configured headers, so the
//     "configured headers are secret" rule never covered it (§5.3).
package logging

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Format values for [logging] format / PAY_LOG_FORMAT.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Level names for [logging] level / PAY_LOG_LEVEL.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Levels and Formats are the closed sets, for validation and help.
var (
	Levels  = []string{LevelDebug, LevelInfo, LevelWarn, LevelError}
	Formats = []string{FormatText, FormatJSON}
)

// Options configures New.
type Options struct {
	// Output is where log lines go. A nil Output discards everything, which is
	// what tests and `--quiet` rely on.
	Output io.Writer
	// Level is one of Levels. Empty means info.
	Level string
	// Format is one of Formats. Empty means text.
	Format string
	// Verbose forces debug and Quiet forces error; Quiet wins, because
	// --quiet is an explicit request for silence.
	Verbose bool
	Quiet   bool
	// Secrets are literal credential values to scrub from every line, in
	// addition to the structural rules. The CLI passes the resolved API key
	// and any minted JWT.
	Secrets []string
	// AddSource includes file:line. Off by default: it doubles the size of a
	// debug transcript and helps nobody but a PayCLI developer.
	AddSource bool
}

// ParseLevel validates a level name.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", LevelInfo:
		return slog.LevelInfo, nil
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelWarn, "warning":
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return 0, apierr.New(apierr.CodeInvalidOption,
			"%q is not a valid log level. Valid values: %s.", s, strings.Join(Levels, ", ")).
			WithDidYouMean(apierr.DidYouMean(s, Levels)...)
	}
}

// ParseFormat validates a log format name.
func ParseFormat(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	default:
		return "", apierr.New(apierr.CodeInvalidOption,
			"%q is not a valid log format. Valid values: %s.", s, strings.Join(Formats, ", ")).
			WithDidYouMean(apierr.DidYouMean(s, Formats)...)
	}
}

// New builds the logger. It never returns nil: an invalid level or format
// degrades to info/text, because failing to log is not a reason to fail a
// command. Validate with ParseLevel / ParseFormat when you want the error.
func New(opts Options) *slog.Logger {
	w := opts.Output
	if w == nil {
		w = io.Discard
	}
	level, err := ParseLevel(opts.Level)
	if err != nil {
		level = slog.LevelInfo
	}
	switch {
	case opts.Quiet:
		level = slog.LevelError
	case opts.Verbose:
		level = slog.LevelDebug
	}

	scrubber := redact.Scrubber{Literals: opts.Secrets}
	handlerOpts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   opts.AddSource,
		ReplaceAttr: replacer(scrubber),
	}
	format, err := ParseFormat(opts.Format)
	if err != nil {
		format = FormatText
	}
	if format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, handlerOpts))
	}
	return slog.New(slog.NewTextHandler(w, handlerOpts))
}

// Discard is a logger that writes nothing. Packages take a *slog.Logger rather
// than reaching for slog.Default, so tests hand them this.
func Discard() *slog.Logger { return New(Options{}) }

// replacer redacts every attribute on its way to the handler. It is the last
// line of defence: a caller that logs a whole header map or a URL with
// userinfo still cannot leak a credential.
func replacer(scrubber redact.Scrubber) func([]string, slog.Attr) slog.Attr {
	return func(groups []string, a slog.Attr) slog.Attr {
		// Message text and levels are scrubbed as free text.
		if len(groups) == 0 && (a.Key == slog.MessageKey || a.Key == slog.SourceKey) {
			if a.Value.Kind() == slog.KindString {
				return slog.String(a.Key, scrubber.Text(a.Value.String()))
			}
			return a
		}
		if redact.Key(a.Key) || redact.Header(a.Key) {
			return slog.String(a.Key, maskFor(a.Key, a.Value))
		}
		return slog.Attr{Key: a.Key, Value: redactValue(scrubber, a.Value)}
	}
}

// maskFor keeps the Authorization fingerprint form (§5.3) while masking every
// other secret attribute outright.
func maskFor(key string, v slog.Value) string {
	if strings.EqualFold(key, "authorization") || strings.EqualFold(key, "proxy-authorization") {
		return redact.AuthorizationValue(v.String())
	}
	return redact.Mask
}

func redactValue(scrubber redact.Scrubber, v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(scrubber.Text(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]slog.Attr, 0, len(attrs))
		for _, a := range attrs {
			if redact.Key(a.Key) || redact.Header(a.Key) {
				out = append(out, slog.String(a.Key, maskFor(a.Key, a.Value)))
				continue
			}
			out = append(out, slog.Attr{Key: a.Key, Value: redactValue(scrubber, a.Value)})
		}
		return slog.GroupValue(out...)
	case slog.KindAny:
		switch t := v.Any().(type) {
		case http.Header:
			return slog.AnyValue(redact.Headers(t))
		case error:
			return slog.StringValue(scrubber.Text(t.Error()))
		case []string:
			out := make([]string, len(t))
			for i, s := range t {
				out[i] = scrubber.Text(s)
			}
			return slog.AnyValue(out)
		case map[string]any:
			red, _ := scrubber.Value(t)
			return slog.AnyValue(red)
		}
		return v
	default:
		return v
	}
}

// URL is the helper every call site should use when logging a URL, so
// redaction cannot be forgotten (§5.3 requires it on every URL that crosses a
// boundary).
func URL(key, raw string) slog.Attr { return slog.String(key, redact.URL(raw)) }
