package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

// Command groups, so `pay --help` reads as a map of the tool rather than an
// alphabetical wall. A command that sets no GroupID lands in "other".
const (
	GroupDiscovery = "discovery"
	GroupRead      = "read"
	GroupWrite     = "write"
	GroupEdit      = "edit"
	GroupFiles     = "files"
	GroupAdmin     = "admin"
	GroupOther     = "other"
)

var commandGroups = []*cobra.Group{
	{ID: GroupDiscovery, Title: "Discovery — what can I do here?"},
	{ID: GroupRead, Title: "Read"},
	{ID: GroupWrite, Title: "Write"},
	{ID: GroupEdit, Title: "Edit — local JSON transforms, built to be piped"},
	{ID: GroupFiles, Title: "Files"},
	{ID: GroupAdmin, Title: "Configuration and maintenance"},
	{ID: GroupOther, Title: "Other"},
}

// factory builds one subcommand against the shared runtime.
type factory func(rt *Runtime) *cobra.Command

var registry []factory

// Register adds a subcommand factory to the root tree.
//
// Every command file calls this from an init(), so root.go never has to name
// the individual commands and several agents can add commands to the same
// package without editing a shared switch. Registration order does not matter:
// the tree is sorted by command name before assembly, which makes `pay --help`
// and the alias-resolution order deterministic.
func Register(f factory) { registry = append(registry, f) }

// rootFlags is the raw parsed state of §9.1's persistent flags. Raw values are
// kept alongside the FlagSet so a command can distinguish "not given" from
// "given the zero value" — the distinction §4.5's precedence chain and §12.3's
// --limit guard both depend on.
type rootFlags struct {
	profile          string
	baseURL          string
	apiPath          string
	graphqlPath      string
	graphqlRoute     string
	authCollection   string
	authMode         string
	authHeaderScheme string

	apiKey      string
	apiKeyFile  string
	apiKeyStdin bool

	output   string
	path     string
	errorsTo string

	quiet   bool
	verbose bool
	noColor bool

	timeout     string
	deadline    string
	cacheTTL    string
	maxRetries  int
	retries     int
	concurrency int

	acceptLanguage string
	locale         string
	fallbackLocale string

	noCache bool
	refresh bool

	yes     bool
	dryRun  bool
	noAudit bool

	noRedact           bool
	noRaw              bool
	configPath         string
	insecureSkipVerify bool

	logLevel  string
	logFormat string

	set *pflag.FlagSet
}

// changed reports whether the named persistent flag was given on the command
// line.
func (f *rootFlags) changed(name string) bool {
	if f == nil || f.set == nil {
		return false
	}
	return f.set.Changed(name)
}

// NoRaw reports whether --no-raw suppressed raw-body passthrough.
func (rt *Runtime) NoRaw() bool { return rt.Flags != nil && rt.Flags.noRaw }

func (f *rootFlags) bind(set *pflag.FlagSet) {
	f.set = set

	set.StringVar(&f.profile, "profile", "", "named profile to use")
	set.StringVar(&f.baseURL, "base-url", "", "Payload server origin, e.g. http://localhost:3000")
	set.StringVar(&f.apiPath, "api-path", "", "Payload routes.api prefix (default /api)")
	set.StringVar(&f.graphqlPath, "graphql-path", "", "full GraphQL path; overrides --api-path + --graphql-route")
	set.StringVar(&f.graphqlRoute, "graphql-route", "", "GraphQL route appended to --api-path (default /graphql)")
	set.StringVar(&f.authCollection, "auth-collection", "", "auth collection slug; \"auto\" discovers it")
	set.StringVar(&f.authMode, "auth-mode", "", "auto|api-key|jwt|anonymous")
	set.StringVar(&f.authHeaderScheme, "auth-header-scheme", "", "JWT|Bearer — the scheme used for a token")

	set.StringVar(&f.apiKey, "api-key", "", "API key (prefer --api-key-stdin: argv is world-readable)")
	set.StringVar(&f.apiKeyFile, "api-key-file", "", "read the API key from a 0600 file")
	set.BoolVar(&f.apiKeyStdin, "api-key-stdin", false, "read the API key from the first line of stdin")

	set.StringVar(&f.output, "output", "", "json|jsonl|table|csv|id|raw (default json, never TTY-dependent)")
	set.StringVar(&f.path, "path", "", "apply a three-form path expression to .data (§10.3; NOT jq)")
	set.StringVar(&f.errorsTo, "errors-to", "", "stdout|stderr — where the error envelope goes")

	set.BoolVarP(&f.quiet, "quiet", "q", false, "suppress the human stderr summary")
	set.BoolVarP(&f.verbose, "verbose", "v", false, "log every request at debug level")
	set.BoolVar(&f.noColor, "no-color", false, "accepted for compatibility; PayCLI never colours output")

	set.StringVar(&f.timeout, "timeout", "", "per-request timeout (default 30s)")
	set.StringVar(&f.deadline, "deadline", "", "whole-command deadline (default 120s)")
	set.StringVar(&f.cacheTTL, "cache-ttl", "", "override the discovery cache TTL for this run")
	set.IntVar(&f.maxRetries, "max-retries", 0, "retry budget for idempotent requests (default 3)")
	// §9.1 gives --max-retries the alias --retries. pflag has no alias
	// mechanism, so the alias is a real hidden flag that writes the same value;
	// configFlags reads whichever one was actually given.
	set.IntVar(&f.retries, "retries", 0, "alias for --max-retries")
	set.Lookup("retries").Hidden = true
	set.IntVar(&f.concurrency, "concurrency", 0, "parallel requests (default 8, hard cap 32)")

	set.StringVar(&f.acceptLanguage, "accept-language", "", "Accept-Language sent to Payload (default en)")
	set.StringVar(&f.locale, "locale", "", "locale for localized fields")
	set.StringVar(&f.fallbackLocale, "fallback-locale", "", "CODE|none|default")

	set.BoolVar(&f.noCache, "no-cache", false, "ignore and do not write the discovery cache")
	set.BoolVar(&f.refresh, "refresh", false, "force re-discovery before running")

	set.BoolVar(&f.yes, "yes", false, "assume yes for every confirmation prompt")
	set.BoolVar(&f.dryRun, "dry-run", false, "print what would be sent and exit 0 without sending it")
	set.BoolVar(&f.noAudit, "no-audit", false, "do not write the audit log for this command")

	set.BoolVar(&f.noRedact, "no-redact", false, "do not redact secrets in output (dangerous)")
	set.BoolVar(&f.noRaw, "no-raw", false, "omit Payload's raw response body from the envelope")
	set.StringVar(&f.configPath, "config", "", "path to config.toml")
	set.BoolVar(&f.insecureSkipVerify, "insecure-skip-verify", false, "skip TLS verification (dev only)")

	set.StringVar(&f.logLevel, "log-level", "", strings.Join(logging.Levels, "|"))
	set.StringVar(&f.logFormat, "log-format", "", strings.Join(logging.Formats, "|"))
}

// configFlags projects the parsed state onto config.Flags, leaving anything
// untouched by the user as the zero value so §4.5's chain sees "not set".
func (f *rootFlags) configFlags() config.Flags {
	cf := config.Flags{
		Profile:          f.profile,
		BaseURL:          f.baseURL,
		APIPath:          f.apiPath,
		GraphQLPath:      f.graphqlPath,
		GraphQLRoute:     f.graphqlRoute,
		AuthCollection:   f.authCollection,
		AuthMode:         f.authMode,
		AuthHeaderScheme: f.authHeaderScheme,
		Output:           f.output,
		ErrorsTo:         f.errorsTo,
		Path:             f.path,
		Locale:           f.locale,
		FallbackLocale:   f.fallbackLocale,
		AcceptLanguage:   f.acceptLanguage,
		Timeout:          f.timeout,
		Deadline:         f.deadline,
		CacheTTL:         f.cacheTTL,
		LogLevel:         f.logLevel,
		LogFormat:        f.logFormat,
	}
	switch {
	case f.changed("max-retries"):
		v := f.maxRetries
		cf.MaxRetries = &v
	case f.changed("retries"):
		v := f.retries
		cf.MaxRetries = &v
	}
	if f.changed("concurrency") {
		v := f.concurrency
		cf.Concurrency = &v
	}
	if f.changed("no-redact") {
		v := !f.noRedact
		cf.Redact = &v
	}
	if f.changed("insecure-skip-verify") {
		v := f.insecureSkipVerify
		cf.InsecureSkipVerify = &v
	}
	if f.changed("no-cache") {
		v := f.noCache
		cf.NoCache = &v
	}
	if f.changed("refresh") {
		v := f.refresh
		cf.Refresh = &v
	}
	if f.changed("yes") {
		v := f.yes
		cf.Yes = &v
	}
	if f.changed("dry-run") {
		v := f.dryRun
		cf.DryRun = &v
	}
	if f.changed("no-audit") {
		v := f.noAudit
		cf.NoAudit = &v
	}
	if f.changed("quiet") {
		v := f.quiet
		cf.Quiet = &v
	}
	if f.changed("verbose") {
		v := f.verbose
		cf.Verbose = &v
	}
	return cf
}

const rootShort = "Drive any Payload CMS 3.x project through its REST API."

// newRootCommand assembles the tree. Everything registered through Register is
// attached in name order.
func newRootCommand(rt *Runtime) *cobra.Command {
	flags := &rootFlags{}
	rt.Flags = flags

	root := &cobra.Command{
		Use:   "pay",
		Short: rootShort,
		Long: rootShort + "\n\n" +
			"PayCLI is built for LLM coding agents: every command prints one JSON envelope\n" +
			"with a stable shape, a machine-readable error code and an exit code that maps to\n" +
			"a class of failure. Run `pay explain` first — it answers \"what can I do here?\"\n" +
			"in a single call, offline after the first discovery.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		Version:           buildinfo.Version(),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := rt.setup(cmd); err != nil {
				return err
			}
			// Opportunistic cache maintenance (§8.7) belongs to a real command
			// run, not to `--help` or `__complete`: those reach setup through
			// ensureSetup and must not create directories or spend a
			// millisecond on a sweep.
			rt.Warn(rt.Cache().MaybeGC(rt.Now())...)
			rt.maybeAutoUpdate()
			return nil
		},
		RunE: Handle(rt, "", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			// A bare `pay` is not an error worth a stack trace, but it is not a
			// success either: point the agent at the one command that answers
			// every other question.
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"pay needs a command").
				WithHint("run `pay explain` to see what this project supports, or `pay --help` for the command tree")
		}),
	}
	rt.rootCmd = root
	flags.bind(root.PersistentFlags())
	for _, g := range commandGroups {
		root.AddGroup(g)
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { renderHelp(rt, cmd) })
	root.SetUsageFunc(func(cmd *cobra.Command) error { renderHelp(rt, cmd); return nil })
	root.CompletionOptions.DisableDefaultCmd = true
	registerRootCompletions(rt, root)

	subs := make([]factory, len(registry))
	copy(subs, registry)
	built := make([]*cobra.Command, 0, len(subs))
	for _, f := range subs {
		if c := f(rt); c != nil {
			built = append(built, c)
		}
	}
	sort.SliceStable(built, func(i, j int) bool { return built[i].Name() < built[j].Name() })
	// Two registrations of the same command name would show the command twice
	// in `pay --help` and make alias resolution order-dependent. First wins,
	// and because the list is name-sorted first, "first" is deterministic.
	seen := map[string]bool{}
	for _, c := range built {
		if seen[c.Name()] {
			continue
		}
		seen[c.Name()] = true
		if c.GroupID == "" {
			c.GroupID = GroupOther
		}
		root.AddCommand(c)
	}
	return root
}

// setup is the persistent pre-run: it loads the configuration layers, resolves
// them (§4.5) and arms the renderer, the logger and the cache. It runs exactly
// once per invocation, before any command body.
func (rt *Runtime) setup(cmd *cobra.Command) error {
	rt.Command = commandName(cmd)
	if rt.setupDone {
		return nil
	}
	rt.setupDone = true

	if rt.Flags.configPath != "" {
		rt.Paths = rt.Paths.WithConfigFile(rt.Flags.configPath)
	}
	if rt.App.WorkDir != "" {
		rt.Project = config.FindProject(rt.App.WorkDir)
	} else {
		rt.Project = &config.Project{}
	}

	userFile, err := config.Load(rt.Paths.ConfigFile, config.KindUser)
	if err != nil {
		return err
	}
	rt.UserFile = userFile

	projectFile, err := rt.Project.LoadConfig()
	if err != nil {
		return err
	}
	if projectFile == nil {
		projectFile = &config.File{Kind: config.KindProject}
	}
	rt.ProjectFile = projectFile

	resolved, err := config.Resolve(config.Input{
		Flags:   rt.Flags.configFlags(),
		Env:     rt.Env,
		Project: projectFile,
		User:    userFile,
	})
	if err != nil {
		return err
	}
	rt.Cfg = resolved

	// Arm the renderer in the order that keeps a LATER failure reportable the
	// way the user asked for: --errors-to and --quiet decide where the error
	// envelope goes, so they are applied before anything that can fail.
	errorsTo, errorsToErr := output.ParseErrorsTo(resolved.ErrorsTo)
	if errorsToErr == nil {
		rt.Out.ErrorsTo = errorsTo
	}
	rt.Out.Quiet = resolved.Quiet
	rt.Out.NoRedact = !resolved.Redact
	rt.rebuildLogger()

	if errorsToErr != nil {
		return errorsToErr
	}
	format, err := output.ParseFormat(resolved.Output)
	if err != nil {
		return err
	}
	rt.Out.Format = format
	if _, err := logging.ParseLevel(resolved.LogLevel); err != nil {
		return err
	}
	if _, err := logging.ParseFormat(resolved.LogFormat); err != nil {
		return err
	}
	if resolved.Path != "" {
		if _, err := output.ParsePath(resolved.Path); err != nil {
			return err
		}
	}

	for _, w := range resolved.Warnings {
		rt.Warnf("config", "%s", w)
	}
	if resolved.ConcurrencyClamped {
		rt.Warnf("config", "concurrency was clamped to the hard cap of %d", config.MaxConcurrency)
	}
	if rt.Flags.changed("no-color") {
		rt.Warnf("config", "--no-color is accepted for compatibility; PayCLI never colours its output")
	}

	return nil
}

// maybeAutoUpdate implements §15.2's background apply. It never blocks, never
// re-execs and never reports a failure: the command the user asked for is what
// matters.
func (rt *Runtime) maybeAutoUpdate() {
	// §4.5: a variable that is set but empty counts as unset, which is how
	// selfupdate.go reads the same variable.
	if rt.Cfg == nil || rt.Env.Get("PAY_NO_UPDATE") != "" {
		return
	}
	state, err := update.LoadState(rt.Paths.UpdateStateFile())
	if err != nil {
		return
	}
	if !update.ShouldAutoApply(state, rt.Cfg.UpdateAuto, buildinfo.Version(), rt.Env.Get("PAY_NO_UPDATE") != "") {
		return
	}
	u := &update.Updater{
		StatePath:      rt.Paths.UpdateStateFile(),
		CurrentVersion: buildinfo.Version(),
		Channel:        rt.Cfg.UpdateChannel,
	}
	env := append(append([]string{}, rt.App.Env...), update.NoRecurseEnv)
	_ = u.SpawnApply(env)
}

// commandName is the envelope's `command` field: the command path with the
// "pay" prefix removed.
func commandName(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	path = strings.TrimPrefix(path, cmd.Root().Name())
	return strings.TrimSpace(path)
}

// requireServer is the guard every command that talks to Payload calls first.
func (rt *Runtime) requireServer() error {
	if rt.Cfg == nil {
		return apierr.New(apierr.CodeInternal, "configuration was not resolved")
	}
	return rt.Cfg.RequireBaseURL()
}

// deadlineContext applies --deadline to a command's context.
//
// The budget is expressed as the time REMAINING on the injected clock rather
// than as an absolute instant: an absolute deadline derived from a pinned test
// clock would already be in the past, and context deadlines are enforced
// against the real monotonic clock no matter what the caller injected.
func (rt *Runtime) deadlineContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if rt.Cfg == nil || rt.Cfg.Deadline <= 0 {
		return ctx, func() {}
	}
	remaining := rt.Cfg.Deadline - rt.Elapsed()
	if remaining <= 0 {
		remaining = time.Millisecond
	}
	return context.WithTimeout(ctx, remaining)
}

// exactArgs is cobra's positional check, restated so a violation carries
// PayCLI's invalid_args code and a usable hint instead of cobra's prose.
func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return apierr.New(apierr.CodeInvalidArgs,
				"%s takes exactly %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("usage: %s", usage)
		}
		return nil
	}
}

func maxArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return apierr.New(apierr.CodeInvalidArgs,
				"%s takes at most %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("usage: %s", usage)
		}
		return nil
	}
}

func minArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return apierr.New(apierr.CodeInvalidArgs,
				"%s takes at least %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("usage: %s", usage)
		}
		return nil
	}
}

func rangeArgs(minN, maxN int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < minN || len(args) > maxN {
			return apierr.New(apierr.CodeInvalidArgs,
				"%s takes between %d and %d arguments, got %d", cmd.CommandPath(), minN, maxN, len(args)).
				WithHint("usage: %s", usage)
		}
		return nil
	}
}

// oneOf validates an enum-valued flag and produces did_you_mean.
func oneOf(flag, value string, valid []string) error {
	if value == "" {
		return nil
	}
	for _, v := range valid {
		if v == value {
			return nil
		}
	}
	return apierr.New(apierr.CodeInvalidOption,
		"%q is not a valid --%s value. Valid values: %s.", value, flag, strings.Join(valid, ", ")).
		WithDidYouMean(apierr.DidYouMean(value, valid)...)
}
