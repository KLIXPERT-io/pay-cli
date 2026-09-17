// Package cli is PayCLI's command tree and the only place in the program that
// is allowed to see the process itself.
//
// Everything ambient — argv, the environment, the three standard streams, the
// wall clock, the HTTP transport and the on-disk directory layout — enters
// through [App] and is carried to every command by [Runtime]. That is what
// makes whole-command golden tests possible in-process (§17.3): a test builds
// an App over bytes.Buffers and a fixture server's RoundTripper and asserts on
// the captured stdout, stderr and the returned exit code.
//
// §3.1 makes this a CI-enforced rule: app.go is the only file outside cmd/
// that may reference os.Stdout, os.Stderr, os.Stdin, os.Args, os.Getenv,
// os.Exit or time.Now.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

// App is the injected process surface. A zero value is unusable; [Run]
// substitutes inert defaults (io.Discard, a fixed clock) for anything left
// nil so that a malformed App can never panic mid-command.
type App struct {
	// Args is os.Args[1:] — the program name is not included.
	Args []string
	// Env is os.Environ()'s "NAME=value" form.
	Env []string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Now is the only clock the program has. Tests pin it.
	Now func() time.Time

	// HTTP replaces the transport internal/payload would otherwise build. A
	// test points it at an httptest.Server; production leaves it nil.
	HTTP http.RoundTripper

	// Dirs overrides the §4.1 directory layout wholesale. A zero value is
	// derived from Env.
	Dirs config.Paths

	// WorkDir is the directory project discovery (§4.3) walks up from.
	WorkDir string

	// Terminal facts. False is the safe answer: PayCLI never changes output
	// shape based on a TTY (§10), it only uses these for the interactive
	// confirmation prompt (§12.2) and the "you typed a key on a terminal"
	// warning (§5.1 step 3).
	StdinIsTTY  bool
	StdoutIsTTY bool
	StderrIsTTY bool

	// NoSignals disables the SIGINT/SIGTERM handler. Tests set it; the
	// in-process handler would otherwise leak across `go test`.
	NoSignals bool
}

// Main is the entry point cmd/pay/main.go calls. It returns the exit status
// instead of taking it, so nothing below cmd/ ever calls os.Exit.
func Main() int {
	wd, err := os.Getwd()
	if err != nil {
		wd = ""
	}
	app := App{
		Args:        os.Args[1:],
		Env:         os.Environ(),
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Now:         time.Now,
		WorkDir:     wd,
		StdinIsTTY:  isTerminal(os.Stdin),
		StdoutIsTTY: isTerminal(os.Stdout),
		StderrIsTTY: isTerminal(os.Stderr),
	}
	// §15.4: a Windows self-update leaves pay.exe.old behind; sweep it before
	// anything else and never let a failure here affect the command.
	if exe, err := update.ResolveBinary(); err == nil {
		update.CleanupOldBinary(exe)
	}
	return Run(context.Background(), app)
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// normalise fills in inert defaults so no later code has to nil-check.
func (a App) normalise() App {
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Now == nil {
		// A fixed epoch rather than time.Now: a caller that forgot the clock
		// gets deterministic nonsense instead of an untestable program.
		a.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	}
	if a.Args == nil {
		a.Args = []string{}
	}
	return a
}

// Run executes one PayCLI invocation and returns its exit status. It never
// panics out to the caller: a panic in a command is converted to exit 1 with
// an `internal` envelope, because an agent parsing stdout must always get an
// envelope.
func Run(ctx context.Context, app App) (code int) {
	app = app.normalise()
	if ctx == nil {
		ctx = context.Background()
	}
	if !app.NoSignals {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}

	rt := newRuntime(app)
	defer func() {
		if r := recover(); r != nil {
			err := apierr.New(apierr.CodeInternal, "pay panicked: %v", r)
			code = rt.fail(err)
		}
	}()

	root := newRootCommand(rt)
	root.SetArgs(app.Args)
	root.SetIn(app.Stdin)
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		return rt.fail(err)
	}
	return rt.exit
}

// Runtime is the per-invocation world every command is handed. It owns the
// resolved configuration, the lazily built client, the cache session, the
// logger, the audit log and the renderer.
//
// Everything expensive is lazy and memoised: `pay version` must work on a
// machine with no config, no network and no cache, so a Runtime that never
// resolves a credential never touches the keychain.
type Runtime struct {
	App App

	// Env is the environment snapshot every layer reads from.
	Env config.Env
	// Paths is the resolved §4.1 layout.
	Paths config.Paths
	// Project is the detected Payload project (§4.3), never nil.
	Project *config.Project
	// UserFile and ProjectFile are the two config layers, never nil.
	UserFile    *config.File
	ProjectFile *config.File
	// Cfg is the resolved configuration. Nil until the persistent pre-run has
	// executed, which is why every command handler receives it already set.
	Cfg *config.Resolved

	// Command is the dotted command path of the command being run ("auth
	// login"), used for envelope.command and the audit log.
	Command string

	// Out renders envelopes. Commands must not write to App.Stdout directly.
	Out *output.Writer
	// Log is the structured logger; never nil.
	Log *slog.Logger

	// Flags is the parsed root flag state, exposed so a command can tell
	// "flag not given" from "flag given the default value".
	Flags *rootFlags

	// rootCmd is the assembled tree. fail() needs it to run a best-effort
	// setup for errors raised before cobra reaches the pre-run hook — an
	// unknown command or a bad flag must still honour --errors-to and --quiet.
	rootCmd *cobra.Command

	// cobraCmd is the command currently executing. `pay explain --section
	// commands` walks the tree from it; nothing else should need it.
	cobraCmd *cobra.Command

	// dataBytes overrides meta.bytes for commands whose payload size is the
	// thing worth reporting (§18: an agent budgets its own context from it).
	dataBytes int64

	start     time.Time
	exit      int
	rendered  bool
	setupDone bool
	requestID string

	mu       sync.Mutex
	warnings []output.Warning

	credOnce sync.Once
	cred     *secret.Credential
	credErr  error

	clientOnce sync.Once
	client     *payload.Client
	clientErr  error

	// authSlug is the auth-collection slug §7.0's ladder resolved for this
	// invocation ("" until it is known). authResolving guards the one
	// re-entrant step of that ladder: resolving through discovery needs a
	// client, and the client needs the slug.
	authSlug       string
	authSlugSource string
	authResolving  bool

	store   *secret.Store
	cache   *cache.Store
	session *cache.Session
	auditor *audit.Logger

	scopeOnce sync.Once
	scope     cache.Scope
	scopeErr  error

	discOnce sync.Once
	disc     *discoveryState
}

func newRuntime(app App) *Runtime {
	rt := &Runtime{
		App:   app,
		Env:   config.NewEnv(app.Env),
		start: app.Now(),
		Log:   logging.Discard(),
		Out: &output.Writer{
			Stdout:   app.Stdout,
			Stderr:   app.Stderr,
			Format:   output.FormatJSON,
			ErrorsTo: output.ErrorsToStdout,
		},
	}
	rt.Paths = app.Dirs
	if rt.Paths.ConfigDir == "" {
		rt.Paths = config.DefaultPaths(rt.Env)
	}
	return rt
}

// Now is the injected clock. Nothing below internal/cli calls time.Now (§3.1).
func (rt *Runtime) Now() time.Time { return rt.App.Now() }

// Elapsed is how long this invocation has been running.
func (rt *Runtime) Elapsed() time.Duration { return rt.Now().Sub(rt.start) }

// Warn records an envelope warning. Duplicates (same code and message) are
// collapsed so a fan-out cannot emit the same advice a hundred times.
func (rt *Runtime) Warn(w ...output.Warning) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, one := range w {
		if one.Code == "" {
			continue
		}
		dup := false
		for _, have := range rt.warnings {
			if have.Code == one.Code && have.Message == one.Message {
				dup = true
				break
			}
		}
		if !dup {
			rt.warnings = append(rt.warnings, one)
		}
	}
}

// Warnf records a warning built from a code and a formatted message.
func (rt *Runtime) Warnf(code, format string, args ...any) {
	rt.Warn(output.Warning{Code: code, Message: fmt.Sprintf(format, args...)})
}

// Warnings returns the warnings recorded so far.
func (rt *Runtime) Warnings() []output.Warning {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]output.Warning, len(rt.warnings))
	copy(out, rt.warnings)
	return out
}

// Secrets returns the literal credential values that must be scrubbed from
// every log line and every rendered byte (§5.3). It never resolves a
// credential on its own.
func (rt *Runtime) Secrets() []string {
	var out []string
	if rt.cred != nil && rt.cred.Value != "" {
		out = append(out, rt.cred.Value)
	}
	return out
}

// Stdin is the command's input stream.
func (rt *Runtime) Stdin() io.Reader { return rt.App.Stdin }

// Stderr is where human-facing notices and prompts go. Never the envelope.
func (rt *Runtime) Stderr() io.Writer { return rt.App.Stderr }

// Handler is the shape every PayCLI command implements. Returning an envelope
// and an error is deliberate: the envelope is rendered when err is nil, and
// the error is normalised into an error envelope otherwise, so no command has
// to know about exit codes, --path, --output or meta.
type Handler func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error)

// Handle adapts a Handler to cobra's RunE. `name` is the envelope's `command`
// field and the audit log's command name; it is the space-joined command path
// without the "pay" prefix ("auth login", "find").
//
// Handle is the ONLY place a command's output reaches a stream. A Handler that
// returns (nil, nil) is a programming error and surfaces as exit 1.
func Handle(rt *Runtime, name string, h Handler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt.Command = name
		rt.cobraCmd = cmd
		env, err := h(cmd.Context(), rt, args)
		if err != nil {
			env = output.NewError(name, err)
		} else if env == nil {
			env = output.NewError(name, apierr.New(apierr.CodeInternal,
				"command %q produced no result", name))
		}
		rt.emit(env)
		return nil
	}
}

// emit finalises and renders one envelope, recording the exit status.
func (rt *Runtime) emit(env *output.Envelope) {
	if env.Command == "" {
		env.Command = rt.Command
	}
	// The runtime owns most of meta, but §9.4 makes date_field/since/until and
	// the resolved locale pair mandatory and only the command knows them, so
	// the command's block is merged OVER the runtime's rather than replaced by
	// it.
	base := rt.Meta()
	mergeCommandMeta(&base, env.Meta)
	env.WithMeta(base)
	for _, w := range rt.Warnings() {
		env.AddWarning(w)
	}
	env.PropagateRawRedaction()

	// §5.3's redaction runs BEFORE --path is evaluated: `pay get users 66
	// --path .apiKey` must print the mask, not the key. output.Writer redacts
	// again as a backstop, which is a no-op once this pass has run.
	if rt.Cfg == nil || rt.Cfg.Redact {
		if paths := env.RedactData(); len(paths) > 0 {
			env.AddWarning(output.DataRedactedWarning(paths))
		}
	}

	if rt.Cfg != nil && rt.Cfg.Path != "" && env.OK {
		if err := env.ApplyPath(rt.Cfg.Path); err != nil {
			env = output.NewError(env.Command, err)
			env.WithMeta(base)
		}
	}
	code, err := rt.Out.Render(env)
	if err != nil {
		// Rendering itself failed (a closed pipe, a broken writer). There is
		// nowhere left to put an envelope, so report it on stderr and exit 1.
		fmt.Fprintf(rt.App.Stderr, "pay: internal (exit 1): %s\n", redact.Text(err.Error()))
		code = apierr.ExitInternal
	}
	rt.exit = code
	rt.rendered = true
}

// fail renders an error that escaped the command tree — a cobra parse error, a
// failed persistent pre-run, or a panic.
func (rt *Runtime) fail(err error) int {
	if err == nil {
		return rt.exit
	}
	if rt.rendered {
		// A command already produced an envelope; do not print a second one.
		return rt.exit
	}
	// cobra rejects an unknown command or flag before it parses persistent
	// flags at all, so the renderer is still unconfigured here. Re-parse what
	// we can, tolerating the very flag that failed, and then configure the
	// renderer: --errors-to and --quiet must apply to parse failures too.
	if rt.Cfg == nil && rt.rootCmd != nil {
		set := rt.rootCmd.PersistentFlags()
		set.ParseErrorsWhitelist.UnknownFlags = true
		set.Init(set.Name(), pflag.ContinueOnError)
		_ = set.Parse(rt.App.Args)
	}
	rt.ensureSetup(rt.rootCmd)
	var already interface{ Rendered() bool }
	if errors.As(err, &already) && already.Rendered() {
		// A data command rendered its own envelope through Deps.Renderer; take
		// its exit status and stay silent.
		rt.exit = apierr.ExitCode(err)
		return rt.exit
	}
	rt.emit(output.NewError(rt.Command, normaliseCobraError(err)))
	return rt.exit
}

// normaliseCobraError maps cobra's plain-text parse failures onto PayCLI's
// codes so an agent never sees a bare Go error string with exit 1.
func normaliseCobraError(err error) error {
	if _, ok := apierr.As(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apierr.Wrap(err, apierr.CodeTimeout, "the command was interrupted before it finished")
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "unknown command"),
		strings.HasPrefix(msg, "unknown flag"),
		strings.HasPrefix(msg, "unknown shorthand flag"),
		strings.HasPrefix(msg, "flag needs an argument"),
		strings.HasPrefix(msg, "invalid argument"),
		strings.Contains(msg, "accepts "),
		strings.Contains(msg, "requires at least"),
		strings.Contains(msg, "requires exactly"):
		return apierr.Wrap(err, apierr.CodeInvalidArgs, "%s", msg)
	default:
		return apierr.Wrap(err, apierr.CodeInvalidArgs, "%s", msg)
	}
}

// Meta builds the §10.1 meta block from whatever the runtime currently knows.
func (rt *Runtime) Meta() output.Meta {
	m := output.Meta{
		RequestID:  rt.requestID,
		CLIVersion: buildinfo.Version(),
		DurationMS: rt.Elapsed().Milliseconds(),
	}
	if rt.Cfg != nil {
		m.Profile = rt.Cfg.Profile
		m.BaseURL = rt.Cfg.BaseURL
		m.APIPath = rt.Cfg.APIPath
		m.DryRun = rt.Cfg.DryRun
		m.AuthMode = string(rt.Cfg.AuthMode)
		if rt.Cfg.Locale != "" {
			loc := rt.Cfg.Locale
			m.Locale.Requested = &loc
		}
		if rt.Cfg.FallbackLocale != "" {
			fb := rt.Cfg.FallbackLocale
			m.Locale.Fallback = &fb
		}
	}
	if rt.cred != nil {
		m.AuthMode = string(rt.cred.Mode)
	}
	if rt.client != nil && m.RequestID == "" {
		// §6: meta.request_id echoes the X-Request-Id PayCLI actually sent, so
		// the user can find that exact request in the Payload server log.
		m.RequestID = rt.client.LastRequestID()
	}
	if rt.client != nil {
		st := rt.client.Stats()
		m.HTTPRequests = int(st.Requests)
		m.Retries = int(st.Retries)
		m.Bytes = st.Bytes
	}
	if rt.dataBytes > 0 {
		m.Bytes = rt.dataBytes
	}
	if rt.disc != nil {
		if rt.disc.manifest != nil {
			m.DiscoveryRevision = rt.disc.manifest.Revision()
			if v := rt.disc.manifest.Source.PayloadVersion; v != nil {
				m.PayloadVersion = v
				m.PayloadVersionSource = rt.disc.manifest.Source.PayloadVersionSource
			}
		}
		if rt.disc.cacheMeta != nil {
			m.Cache = rt.disc.cacheMeta
		}
	}
	return m
}

// SetDataBytes makes meta.bytes report the size of the rendered payload rather
// than the bytes read from the network. `pay explain` is the case §18 names.
func (rt *Runtime) SetDataBytes(n int64) { rt.dataBytes = n }

// Confirmer builds §12.2's prompt over the injected streams.
func (rt *Runtime) Confirmer() *safety.Confirmer {
	c := &safety.Confirmer{In: rt.App.Stdin, Out: rt.App.Stderr, TTY: rt.App.StdinIsTTY}
	if rt.Cfg != nil {
		c.AssumeYes = rt.Cfg.Yes
		c.ConfirmWrites = rt.Cfg.ConfirmWrites
		c.Quiet = rt.Cfg.Quiet
		c.DryRun = rt.Cfg.DryRun
	}
	return c
}

// Audit returns the audit logger, building it on first use.
func (rt *Runtime) Audit() *audit.Logger {
	if rt.auditor != nil {
		return rt.auditor
	}
	opts := audit.Options{
		Path:    rt.Paths.AuditFile(),
		Now:     rt.App.Now,
		Secrets: rt.Secrets(),
	}
	if rt.Cfg != nil {
		opts.Disabled = rt.Cfg.NoAudit
	}
	rt.auditor = audit.New(opts)
	return rt.auditor
}

// SecretStore is the credentials.json + keychain store for this profile set.
func (rt *Runtime) SecretStore() *secret.Store {
	if rt.store != nil {
		return rt.store
	}
	mode := secret.KeyringAuto
	if rt.Cfg != nil {
		mode = secret.KeyringMode(string(rt.Cfg.KeyringMode))
	}
	rt.store = secret.NewStore(rt.Paths.CredentialsFile(), mode)
	return rt.store
}

// Cache is the discovery cache store. It is permanently disabled (every read a
// miss, every write a no-op) under --no-cache.
func (rt *Runtime) Cache() *cache.Store {
	if rt.cache != nil {
		return rt.cache
	}
	root := rt.Paths.CacheDir
	if rt.Cfg != nil {
		if rt.Cfg.CacheDir != "" {
			root = rt.Cfg.CacheDir
		}
		if rt.Cfg.NoCache {
			root = ""
		}
	}
	rt.cache = cache.New(root)
	return rt.cache
}

// Scope is the §8.1 cache scope for the resolved connection and credential.
func (rt *Runtime) Scope() (cache.Scope, error) {
	rt.scopeOnce.Do(func() {
		if rt.Cfg == nil {
			rt.scopeErr = apierr.New(apierr.CodeInternal, "configuration was not resolved")
			return
		}
		fp := cache.AnonKeyFingerprint
		if cred, err := rt.Credential(context.Background()); err == nil && cred != nil && !cred.Anonymous() {
			fp = cred.Fingerprint
		}
		rt.scope, rt.scopeErr = cache.NewScope(cache.ScopeInput{
			BaseURL:        rt.Cfg.BaseURL,
			APIPath:        rt.Cfg.APIPath,
			GraphQLPath:    rt.Cfg.GraphQLPath,
			KeyFingerprint: fp,
			Headers:        rt.Cfg.Headers,
		})
	})
	return rt.scope, rt.scopeErr
}

// Session is the per-invocation cache session (§8.6).
func (rt *Runtime) Session() *cache.Session {
	if rt.session != nil {
		return rt.session
	}
	sc, err := rt.Scope()
	if err != nil {
		// A scope that cannot be computed disables the cache rather than
		// failing the command: the network path still works.
		sc = cache.Scope{}
	}
	opts := cache.SessionOptions{}
	if rt.Cfg != nil {
		opts.NoCache = rt.Cfg.NoCache
		opts.Refresh = rt.Cfg.Refresh
	}
	rt.session = cache.NewSession(rt.Cache(), sc, opts)
	return rt.session
}

// Credential resolves §5.1's chain once per invocation.
func (rt *Runtime) Credential(ctx context.Context) (*secret.Credential, error) {
	rt.credOnce.Do(func() {
		if rt.Cfg == nil {
			rt.credErr = apierr.New(apierr.CodeInternal, "configuration was not resolved")
			return
		}
		in := secret.Input{
			Profile:   rt.Cfg.Profile,
			BaseURL:   rt.Cfg.BaseURL,
			Env:       rt.Env,
			Mode:      secret.Mode(string(rt.Cfg.AuthMode)),
			APIKeyEnv: rt.Cfg.APIKeyEnv,
			Store:     rt.SecretStore(),
			Now:       rt.Now(),
			Flags: secret.Flags{
				APIKey:      rt.Flags.apiKey,
				APIKeyStdin: rt.Flags.apiKeyStdin,
				APIKeyFile:  rt.Flags.apiKeyFile,
				Stdin:       rt.App.Stdin,
				IsTTY:       rt.App.StdinIsTTY,
			},
		}
		if rt.Cfg.CredentialHelper != "" {
			in.Helper = secret.NewHelper(rt.Cfg.CredentialHelper)
		}
		cred, err := secret.Resolve(ctx, in)
		if err != nil {
			rt.credErr = err
			return
		}
		rt.cred = cred
		for _, w := range cred.Warnings {
			rt.Warnf("auth_source", "%s", w)
		}
		// Re-arm the logger now that the literal secret is known, so the
		// scrubber can strip it from every subsequent line (§5.3).
		rt.rebuildLogger()
	})
	return rt.cred, rt.credErr
}

// Client builds the Payload client for the resolved profile and credential.
//
// It never hands out a client whose auth-collection slug is still §7.0's
// "auto" placeholder: in api-key mode the slug is part of the Authorization
// header, and a wrong slug answers HTTP 200 with the anonymous view rather
// than an error. resolveAuthCollection runs the §7.0 ladder (configured →
// cached → Stage -1) before the client escapes this function.
func (rt *Runtime) Client(ctx context.Context) (*payload.Client, error) {
	client, err := rt.baseClient(ctx)
	if err != nil {
		return nil, err
	}
	if err := rt.resolveAuthCollection(ctx); err != nil {
		return nil, err
	}
	if rt.client != nil {
		return rt.client, nil
	}
	return client, nil
}

// baseClient builds the client once. The slug it is built with may still be
// the placeholder; only discovery, which overrides the slug per request, is
// allowed to see it in that state.
func (rt *Runtime) baseClient(ctx context.Context) (*payload.Client, error) {
	rt.clientOnce.Do(func() {
		if rt.Cfg == nil {
			rt.clientErr = apierr.New(apierr.CodeInternal, "configuration was not resolved")
			return
		}
		if err := rt.Cfg.RequireBaseURL(); err != nil {
			rt.clientErr = err
			return
		}
		cred, err := rt.Credential(ctx)
		if err != nil {
			rt.clientErr = err
			return
		}
		cfg := payload.Config{
			BaseURL:            rt.Cfg.BaseURL,
			APIPath:            rt.Cfg.APIPath,
			GraphQLPath:        rt.Cfg.GraphQLPath,
			AuthMode:           string(cred.Mode),
			AuthCollection:     rt.configuredAuthCollection(),
			Credential:         cred.Value,
			AuthScheme:         rt.Cfg.AuthHeaderScheme,
			Headers:            rt.Cfg.Headers,
			AcceptLanguage:     rt.Cfg.AcceptLanguage,
			Timeout:            rt.Cfg.Timeout,
			MaxRetries:         rt.Cfg.MaxRetries,
			Concurrency:        rt.Cfg.Concurrency,
			InsecureSkipVerify: rt.Cfg.InsecureSkipVerify,
			UserAgent:          userAgent(),
			Logger:             rt.Log,
			Now:                rt.App.Now,
			RoundTripper:       rt.App.HTTP,
		}
		if cred.Mode == secret.ModeAuto || cred.Mode == "" {
			cfg.AuthMode = payload.AuthModeAnonymous
		}
		client, err := payload.New(cfg)
		if err != nil {
			rt.clientErr = err
			return
		}
		rt.client = client
	})
	return rt.client, rt.clientErr
}

// configuredAuthCollection is the slug the client is built with: whatever the
// §7.0 ladder has already resolved, else the configured value (which may still
// be the "auto" placeholder).
func (rt *Runtime) configuredAuthCollection() string {
	if rt.authSlug != "" {
		return rt.authSlug
	}
	return rt.Cfg.AuthCollection
}

// adoptAuthCollection installs a resolved slug on the runtime and on the
// already-built client, so every later request carries the right header
// without mutating a client another goroutine may be reading.
func (rt *Runtime) adoptAuthCollection(slug, source string) {
	if slug == "" || slug == config.DefaultAuthCollection {
		return
	}
	rt.authSlug, rt.authSlugSource = slug, source
	if rt.client != nil && rt.client.AuthCollection() != slug {
		rt.client = rt.client.WithAuthCollection(slug)
	}
}

// enterDiscovery marks the current goroutine as running inside discovery and
// returns the function that unmarks it. While it is set, Client hands out the
// client as built — discovery overrides the auth-collection slug per request
// (§7.0) — which is what keeps the slug ladder from re-entering discovery and
// deadlocking on its sync.Once.
func (rt *Runtime) enterDiscovery() func() {
	prev := rt.authResolving
	rt.authResolving = true
	return func() { rt.authResolving = prev }
}

// resolveAuthCollection runs §7.0's ladder: an explicitly configured slug, the
// cached Stage -1 answer for this (base URL, key) pair, then discovery's Stage
// -1 itself. Only api-key mode needs the network step — the jwt header does not
// carry the slug — so a jwt or anonymous profile never pays for it here.
func (rt *Runtime) resolveAuthCollection(ctx context.Context) error {
	if rt.authSlug != "" || rt.authResolving || rt.Cfg == nil {
		return nil
	}
	if rt.Cfg.AuthCollection != "" && rt.Cfg.AuthCollection != config.DefaultAuthCollection {
		rt.adoptAuthCollection(rt.Cfg.AuthCollection, rt.Cfg.Sources["auth_collection"])
		return nil
	}
	cred, err := rt.Credential(ctx)
	if err != nil {
		return err
	}
	if cred == nil || cred.Anonymous() {
		return nil
	}
	if sc, scopeErr := rt.Scope(); scopeErr == nil {
		res, ok, warn := rt.Cache().LookupAuthResolution(sc)
		if warn != nil {
			rt.Warn(*warn)
		}
		if ok && res.AuthCollection != "" {
			rt.adoptAuthCollection(res.AuthCollection, discovery.SourceCached)
			return nil
		}
	}
	if cred.Mode != secret.ModeAPIKey {
		return nil
	}
	restore := rt.enterDiscovery()
	m, err := rt.Discovery(ctx)
	restore()
	if err != nil {
		return err
	}
	if m != nil && m.Identity.AuthCollection != nil {
		rt.adoptAuthCollection(*m.Identity.AuthCollection, m.Identity.AuthCollectionSource)
	}
	return nil
}

func userAgent() string {
	info := buildinfo.Current()
	return fmt.Sprintf("pay/%s (%s/%s)", info.Version, info.OS, info.Arch)
}

// rebuildLogger re-creates the logger with the currently known secret
// literals. It is cheap and idempotent.
func (rt *Runtime) rebuildLogger() {
	opts := logging.Options{
		Output:  rt.App.Stderr,
		Secrets: rt.Secrets(),
	}
	if rt.Cfg != nil {
		opts.Level = rt.Cfg.LogLevel
		opts.Format = rt.Cfg.LogFormat
		opts.Verbose = rt.Cfg.Verbose
		opts.Quiet = rt.Cfg.Quiet
	}
	rt.Log = logging.New(opts)
}
