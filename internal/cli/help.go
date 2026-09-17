package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// Help is the §10.5 model for one command's help text. Every command supplies
// the parts that cannot be derived from its cobra definition; the flag table,
// the command path and the subcommand list are read off cobra itself so the two
// can never drift.
//
// A command with no Help still renders all eleven sections — it simply has no
// examples and no mistakes to show. That is a visible hole rather than a silent
// one, which is the point.
type Help struct {
	// Synopsis lines are printed verbatim under SYNOPSIS, metavariables and
	// all.
	Synopsis []string

	// Long is optional prose printed under the one-line Short.
	Long string

	// Discovered renders section 3 for this command. Returning nil falls back
	// to the shared collection inventory when Collections is true.
	Discovered func(rt *Runtime, m *discovery.Manifest) []string

	// Collections asks for the shared "collections in this project" block in
	// section 3.
	Collections bool
	// Globals asks for the globals inventory in section 3.
	Globals bool

	// Where and Sort switch on sections 4 and 5. They belong on any command
	// that accepts --where or --sort.
	Where bool
	Sort  bool

	// Args documents the positional arguments for §10.4.
	Args []ArgSpec
	// FlagInfo adds §10.4 detail (grammar, operators, bounds) that pflag
	// cannot express, keyed by flag name.
	FlagInfo map[string]FlagInfo

	Output    OutputSpec
	ExitCodes []int
	Examples  []Example
	Mistakes  []Mistake
	SeeAlso   []string

	// Notes are extra bullet lines printed after FLAGS. Use them for the
	// "this flag is a no-op on this project" callouts §10.5 item 6 requires.
	Notes []string
}

// ArgSpec documents one positional argument (§10.4).
type ArgSpec struct {
	Name       string   `json:"name"`
	Required   bool     `json:"required"`
	Type       string   `json:"type"`
	ValuesFrom string   `json:"values_from,omitempty"`
	Values     []string `json:"values,omitempty"`
	Example    string   `json:"example,omitempty"`
}

// FlagInfo is the §10.4 detail for one flag.
type FlagInfo struct {
	Grammar    string   `json:"grammar,omitempty"`
	Operators  []string `json:"operators,omitempty"`
	Values     []string `json:"values,omitempty"`
	Repeatable bool     `json:"repeatable,omitempty"`
	Min        *int     `json:"min,omitempty"`
	Max        *int     `json:"max,omitempty"`
	// Note is printed in the FLAGS section, e.g. "no-op on this project:
	// localization is disabled".
	Note string `json:"note,omitempty"`
}

// OutputSpec is §10.4's output block.
type OutputSpec struct {
	Kind      output.DataKind `json:"data_kind"`
	Paginated bool            `json:"paginated,omitempty"`
	Skeleton  string          `json:"skeleton,omitempty"`
}

// Example is one copy-pasteable invocation.
type Example struct {
	Why string `json:"why"`
	Cmd string `json:"cmd"`
}

// Mistake is one numbered anti-pattern and its correction. §10.5's hard rule:
// every "you cannot X" is followed by "do Y instead", which is why Right is not
// optional.
type Mistake struct {
	Wrong string `json:"wrong"`
	Right string `json:"right"`
}

var (
	helpMu    sync.RWMutex
	helpIndex = map[*cobra.Command]*Help{}
)

// SetHelp attaches a Help model to a command.
func SetHelp(cmd *cobra.Command, h *Help) {
	if cmd == nil || h == nil {
		return
	}
	helpMu.Lock()
	helpIndex[cmd] = h
	helpMu.Unlock()
}

// HelpOf returns the Help model attached to a command, or an empty one.
func HelpOf(cmd *cobra.Command) *Help {
	helpMu.RLock()
	h := helpIndex[cmd]
	helpMu.RUnlock()
	if h == nil {
		return &Help{}
	}
	return h
}

// renderHelp is the single help entry point (cobra's HelpFunc and UsageFunc).
//
// It never blocks on the network: section 3 is rendered from the cached index
// only, and a cold cache prints the command that populates it.
func renderHelp(rt *Runtime, cmd *cobra.Command) {
	rt.ensureSetup(cmd)
	h := HelpOf(cmd)

	if rt.Out != nil && rt.Out.Format == output.FormatJSON && rt.Flags != nil && rt.Flags.changed("output") {
		// §10.4: `pay <cmd> --help --output json` is the machine-readable form.
		rt.emit(output.New(commandName(cmd), output.KindCommandSpec, buildCommandSpec(rt, cmd, h)))
		return
	}
	if rt.Out != nil && rt.Flags != nil && rt.Flags.changed("output") && rt.Out.Format != output.FormatJSON {
		if err := output.CheckFormat(rt.Out.Format, output.KindCommandSpec); err != nil {
			rt.emit(output.NewError(commandName(cmd), err))
			return
		}
	}
	w := cmd.OutOrStdout()
	renderHelpText(w, rt, cmd, h)
}

// ensureSetup runs the persistent pre-run's work for code paths cobra reaches
// before hooks — `--help`, `__complete` and every argument-parse failure all
// short-circuit above PersistentPreRunE. Failures are swallowed: help on a
// machine with a broken config file must still print.
//
// It also guarantees §10.1's `command` key. Runtime.fail() hands it the ROOT
// command because cobra never got far enough to bind one, and commandName(root)
// is "" — which the envelope then drops, so `pay get pages --published-only`
// used to answer with no `command` at all. Resolving argv against the command
// tree recovers the name for every invocation that names a real command.
func (rt *Runtime) ensureSetup(cmd *cobra.Command) {
	cmd = rt.resolveCommand(cmd)
	if !rt.setupDone && rt.Cfg == nil {
		_ = rt.setup(cmd)
	}
	if rt.Command == "" && cmd != nil {
		// setup() early-returns once it has run, and never runs at all when
		// the config was already resolved, so the assignment is repeated here
		// rather than left to it.
		rt.Command = commandName(cmd)
	}
}

// resolveCommand maps the root command onto the subcommand argv actually names.
// It is a no-op for any command cobra already bound (those have a parent), and
// for an unknown command, where Find fails and the root is returned unchanged.
func (rt *Runtime) resolveCommand(cmd *cobra.Command) *cobra.Command {
	if cmd == nil || cmd.HasParent() || len(rt.App.Args) == 0 {
		return cmd
	}
	if found, _, err := cmd.Find(rt.App.Args); err == nil && found != nil {
		return found
	}
	return cmd
}

func renderHelpText(w io.Writer, rt *Runtime, cmd *cobra.Command, h *Help) {
	p := &printer{w: w}

	// 1. Short, then the prose — with the Short line dropped from the prose
	// when the prose repeats it (the root command's Long opens with its own
	// Short), so help never starts with the same sentence twice.
	p.line(cmd.Short)
	long := h.Long
	if long == "" {
		long = cmd.Long
	}
	long = strings.TrimPrefix(strings.TrimSpace(long), strings.TrimSpace(cmd.Short))
	long = strings.TrimLeft(long, "\n")
	if long != "" {
		p.blank()
		p.wrap(long)
	}

	// 2. SYNOPSIS.
	p.section("SYNOPSIS")
	if len(h.Synopsis) > 0 {
		for _, s := range h.Synopsis {
			p.indent(s)
		}
	} else {
		p.indent(cmd.UseLine())
	}
	if cmd.HasAvailableSubCommands() {
		p.blank()
		p.indent("Subcommands:")
		for _, sub := range cmd.Commands() {
			if !sub.IsAvailableCommand() {
				continue
			}
			p.indent(fmt.Sprintf("  %-14s %s", sub.Name(), sub.Short))
		}
	}

	// 3. DISCOVERED IN THIS PROJECT.
	if h.Collections || h.Globals || h.Discovered != nil {
		p.section("DISCOVERED IN THIS PROJECT")
		p.indent(rt.stamp())
		m, ok := rt.CachedManifest()
		switch {
		case !ok:
			p.indent("cache is cold — run 'pay discover' to populate")
		default:
			var lines []string
			if h.Discovered != nil {
				lines = h.Discovered(rt, m)
			}
			if len(lines) == 0 {
				lines = discoveredInventory(m, h.Collections, h.Globals)
			}
			for _, l := range lines {
				p.indent(l)
			}
		}
	}

	// 4. WHERE SYNTAX.
	if h.Where {
		p.section("WHERE SYNTAX")
		for _, l := range whereSyntaxLines() {
			p.indent(l)
		}
	}

	// 5. SORT.
	if h.Sort {
		p.section("SORT")
		for _, l := range sortLines() {
			p.indent(l)
		}
	}

	// 6. FLAGS.
	p.section("FLAGS")
	local := cmd.LocalFlags()
	if countFlags(local) == 0 {
		p.indent("(none beyond the root flags)")
	} else {
		for _, l := range flagLines(local, h) {
			p.indent(l)
		}
	}
	if inherited := cmd.InheritedFlags(); countFlags(inherited) > 0 {
		p.blank()
		p.indent("Root flags (every command): run 'pay --help' for the full list.")
		p.indent("  --profile --base-url --output --path --quiet --verbose --timeout --deadline")
		p.indent("  --no-cache --refresh --yes --dry-run --no-audit --no-redact --errors-to")
	}
	for _, n := range h.Notes {
		p.blank()
		p.indent("note: " + n)
	}

	// 7. OUTPUT.
	p.section("OUTPUT")
	kind := h.Output.Kind
	if kind == "" {
		kind = output.KindOpResult
	}
	p.indent(fmt.Sprintf("data_kind: %s%s", kind, paginatedSuffix(h.Output.Paginated)))
	if h.Output.Skeleton != "" {
		p.indent(`.data = ` + h.Output.Skeleton)
	}
	p.indent(`Envelope: {"ok":true,"v":1,"command":"…","data_kind":"…","data":…,"meta":{…},"warnings":[]}`)

	// 8. EXIT CODES.
	p.section("EXIT CODES")
	for _, l := range exitCodeLines(h.ExitCodes) {
		p.indent(l)
	}

	// 9. EXAMPLES.
	p.section("EXAMPLES")
	if len(h.Examples) == 0 {
		p.indent("(none recorded for this command)")
	}
	for _, ex := range h.Examples {
		p.indent("# " + ex.Why)
		p.indent(ex.Cmd)
		p.blank()
	}

	// 10. COMMON MISTAKES.
	p.section("COMMON MISTAKES")
	if len(h.Mistakes) == 0 {
		p.indent("(none recorded for this command)")
	}
	for i, m := range h.Mistakes {
		p.indent(fmt.Sprintf("%d. %s", i+1, m.Wrong))
		p.indent("   Instead: " + m.Right)
	}

	// 11. SEE ALSO.
	p.section("SEE ALSO")
	if len(h.SeeAlso) == 0 {
		p.indent("pay explain    # what can I do here?")
		p.indent("pay doctor     # why is this failing?")
	}
	for _, s := range h.SeeAlso {
		p.indent(s)
	}
	p.blank()
}

// stamp is §10.5's mandatory header line: an agent that cached help from
// project A cannot mistake it for project B.
func (rt *Runtime) stamp() string {
	profile, base, rev := "-", "-", "-"
	if rt.Cfg != nil {
		if rt.Cfg.Profile != "" {
			profile = rt.Cfg.Profile
		}
		if rt.Cfg.BaseURL != "" {
			base = rt.Cfg.BaseURL
		}
	}
	if m, ok := rt.CachedManifest(); ok {
		rev = m.Revision()
	}
	return fmt.Sprintf("profile=%s base_url=%s discovery_revision=%s", profile, base, rev)
}

// discoveredInventory renders the shared section 3 body. The collection list is
// grouped by kind and, unlike `pay explain`'s detail array, truncation always
// names the command that shows the rest.
func discoveredInventory(m *discovery.Manifest, wantCollections, wantGlobals bool) []string {
	var lines []string
	if wantCollections {
		groups := groupCollections(m)
		for _, name := range collectionGroupOrder {
			list := groups[name]
			if len(list) == 0 {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s (%d):", name, len(list)))
			shown := list
			truncated := false
			if len(shown) > helpSlugLimit {
				shown = shown[:helpSlugLimit]
				truncated = true
			}
			lines = append(lines, wrapList("  ", shown, 84)...)
			if truncated {
				lines = append(lines, fmt.Sprintf("  … %d more — pay explain --section collections --offset %d",
					len(list)-helpSlugLimit, helpSlugLimit))
			}
		}
	}
	if wantGlobals {
		slugs := m.GlobalSlugs()
		lines = append(lines, fmt.Sprintf("globals (%d):", len(slugs)))
		lines = append(lines, wrapList("  ", slugs, 84)...)
	}
	if len(lines) == 0 {
		lines = append(lines, "(nothing discovered)")
	}
	return lines
}

const helpSlugLimit = 40

var collectionGroupOrder = []string{"content", "upload", "auth", "internal"}

// groupCollections splits the inventory the way §9.2's --kind flag does.
func groupCollections(m *discovery.Manifest) map[string][]string {
	out := map[string][]string{}
	for _, c := range m.Collections {
		out[collectionKind(c)] = append(out[collectionKind(c)], collectionLabel(c))
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// collectionKind is the single definition of §9.2's --kind enum, shared by help
// and `pay collections`.
func collectionKind(c *discovery.Collection) string {
	switch {
	case c.Internal:
		return "internal"
	case c.Flags.Upload != nil && *c.Flags.Upload:
		return "upload"
	case c.Flags.Auth != nil && *c.Flags.Auth:
		return "auth"
	default:
		return "content"
	}
}

func collectionLabel(c *discovery.Collection) string {
	if c.Stats.TotalDocs != nil {
		return fmt.Sprintf("%s(%d)", c.Slug, *c.Stats.TotalDocs)
	}
	return c.Slug
}

func wrapList(prefix string, items []string, width int) []string {
	if len(items) == 0 {
		return []string{prefix + "(none)"}
	}
	var lines []string
	cur := prefix
	for i, it := range items {
		add := it
		if i < len(items)-1 {
			add += ","
		}
		if len(cur) > len(prefix) && len(cur)+1+len(add) > width {
			lines = append(lines, cur)
			cur = prefix
		}
		if len(cur) > len(prefix) {
			cur += " "
		}
		cur += add
	}
	if strings.TrimSpace(cur) != "" {
		lines = append(lines, cur)
	}
	return lines
}

// whereOperators is §9.4's table, taught inline in every help block that
// mentions --where. It is also the `operators` array of §10.4's command spec.
var whereOperators = [][3]string{
	{"eq, =, is", "equals", "typed scalar"},
	{"ne, !=, not", "not_equals", "typed scalar"},
	{"gt, >", "greater_than", "number/date"},
	{"gte, >=", "greater_than_equal", "number/date"},
	{"lt, <", "less_than", "number/date"},
	{"lte, <=", "less_than_equal", "number/date"},
	{"contains, ~", "contains", "substring; % _ \\ are escaped for you"},
	{"like", "like", "space-separated word AND (adapter-specific)"},
	{"nlike, !~", "not_like", "value auto-wrapped in %…%"},
	{"in", "in", "comma-split (\\, escapes) or json:[…]"},
	{"nin", "not_in", "same as in"},
	{"all", "all", "same as in — blocked client-side on Postgres"},
	{"exists", "exists", "bool; value optional, defaults true"},
	{"near", "near", "lng,lat,maxMeters[,minMeters]"},
	{"within, intersects", "within / intersects", "GeoJSON literal or @file.geojson"},
}

// WhereOperatorAliases is the flat alias list §10.4 puts on the --where flag.
func WhereOperatorAliases() []string {
	out := make([]string, 0, len(whereOperators)*2)
	for _, row := range whereOperators {
		for _, a := range strings.Split(row[0], ",") {
			out = append(out, strings.TrimSpace(a))
		}
	}
	return out
}

func whereSyntaxLines() []string {
	lines := []string{
		"--where 'PATH OP VALUE'   repeatable; terms are ANDed.",
		"--or    'PATH OP VALUE'   repeatable; forms ONE or-group, ANDed with the --where terms.",
		"--where-json JSON         replaces both. --where-raw QS appends a literal query string.",
		"",
		"A term is split into at most three whitespace-separated tokens: path, operator and",
		"the rest of the string as the value — a value with spaces needs no quoting.",
		"Paths may be dotted, including across relationships: company.name, activities.type.",
		"",
		fmt.Sprintf("  %-20s %-20s %s", "ALIAS(ES)", "PAYLOAD OPERATOR", "VALUE"),
	}
	for _, row := range whereOperators {
		lines = append(lines, fmt.Sprintf("  %-20s %-20s %s", row[0], row[1], row[2]))
	}
	lines = append(lines,
		"",
		"Value typing: null -> JSON null; true/false -> bool; a number literal -> number;",
		"anything else -> string. Force a string with quotes (code eq \"123\"); force raw",
		"JSON with the json: prefix (tags in json:[1,2]).",
		"",
		"Two deliberate deviations from pass-through, both verified footguns:",
		"  contains escapes % _ and \\  (contains=% otherwise matches every row)",
		"  nlike auto-wraps in %…%      (not_like=hopper otherwise matches every row)",
	)
	return lines
}

func sortLines() []string {
	return []string{
		"--sort FIELD      ascending; prefix with - for descending (--sort -createdAt).",
		"Repeat or comma-separate for a multi-key sort.",
		"",
		"WARNING: Payload SILENTLY IGNORES an unknown sort field and returns 200 with",
		"unsorted results. PayCLI validates the field against the cached schema and fails",
		"with invalid_sort_field (exit 5) plus the sortable-field list instead.",
		"Pass --no-validate-sort to send it anyway when the schema is stale.",
	}
}

// ---------------------------------------------------------------------------
// Exit-code sets (§10.5 section 8)
//
// These are MEASURED, not guessed: every code below was produced by running the
// command against a live Payload instance. The default fallback in
// exitCodeLines() prints {0, 1}, which is actively harmful for a data command —
// an agent that branches on `pay find --help` would believe find cannot answer
// 5 (bad --where), 9 (no base_url) or 10 (unknown collection).
// ---------------------------------------------------------------------------

// ReadExitCodes is what any command that reads from the server can return.
var ReadExitCodes = []int{
	apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
	apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork,
	apierr.ExitAccessDenied, apierr.ExitConfig, apierr.ExitCapability,
}

// WriteExitCodes is ReadExitCodes for a single-document write: same failures,
// and nothing bulk-specific.
var WriteExitCodes = ReadExitCodes

// DestructiveExitCodes adds §12's confirmation gate to a single-document write.
var DestructiveExitCodes = []int{
	apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
	apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork,
	apierr.ExitAccessDenied, apierr.ExitConfig, apierr.ExitCapability,
	apierr.ExitConfirmationRequired,
}

// BulkExitCodes adds §12.5's partial commit and §12's confirmation gate. 7 is
// the one an agent must never auto-retry.
var BulkExitCodes = []int{
	apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
	apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork,
	apierr.ExitPartial, apierr.ExitAccessDenied, apierr.ExitConfig,
	apierr.ExitCapability, apierr.ExitConfirmationRequired,
}

func paginatedSuffix(p bool) string {
	if p {
		return "  (paginated — .page carries limit/page/total_pages/has_next_page)"
	}
	return ""
}

// fallbackExitCodes is what a command that declares none publishes. It is a
// single var rather than two literals because the text renderer used to fall
// back to {0, 1} while buildCommandSpec fell back to {0}: the same command
// answered `--help` and `--help --output json` with two different, and two
// differently wrong, tables. TestEveryCommandDeclaresItsExitCodes keeps it
// unreachable in the shipped tree; it exists only so a half-built command in a
// working tree still renders.
var fallbackExitCodes = []int{apierr.ExitOK, apierr.ExitInternal}

func exitCodeLines(codes []int) []string {
	if len(codes) == 0 {
		codes = fallbackExitCodes
	}
	seen := map[int]bool{}
	var lines []string
	for _, c := range codes {
		if seen[c] {
			continue
		}
		seen[c] = true
		lines = append(lines, fmt.Sprintf("  %-3d %s", c, exitCodeMeaning(c)))
	}
	lines = append(lines, "", "Full table: pay explain --section exit_codes")
	return lines
}

func exitCodeMeaning(code int) string {
	switch code {
	case apierr.ExitOK:
		return "success"
	case apierr.ExitInternal:
		return "internal — a PayCLI bug, a corrupt cache or an unwritable audit log"
	case apierr.ExitAuth:
		return "auth — missing, invalid, locked or unverifiable credential"
	case apierr.ExitThrottled:
		return "throttled — rate limited, document locked or server busy (retriable)"
	case apierr.ExitNotFound:
		return "not found — document, route or version does not exist"
	case apierr.ExitValidation:
		return "validation / bad input — the request was never worth sending"
	case apierr.ExitNetwork:
		return "network — DNS, TLS, timeout or a 5xx from the server"
	case apierr.ExitPartial:
		return "partial — some items in a bulk operation failed; see .error.failures"
	case apierr.ExitAccessDenied:
		return "access denied — authenticated but not permitted"
	case apierr.ExitConfig:
		return "configuration — no base_url, unknown profile, secret in plaintext"
	case apierr.ExitCapability:
		return "capability — this project does not support the operation"
	case apierr.ExitConfirmationRequired:
		return "confirmation required — re-run with --yes (or --dry-run first)"
	default:
		return "unknown"
	}
}

func countFlags(set *pflag.FlagSet) int {
	n := 0
	set.VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			n++
		}
	})
	return n
}

func flagLines(set *pflag.FlagSet, h *Help) []string {
	var names []string
	byName := map[string]*pflag.Flag{}
	set.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		names = append(names, f.Name)
		byName[f.Name] = f
	})
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		f := byName[name]
		head := "--" + f.Name
		if f.Shorthand != "" {
			head = "-" + f.Shorthand + ", " + head
		}
		typ := f.Value.Type()
		if typ != "bool" {
			head += " " + strings.ToUpper(metavar(f))
		}
		line := fmt.Sprintf("  %-28s %s", head, f.Usage)
		var extra []string
		if def := f.DefValue; def != "" && def != "false" && def != "[]" && def != "0" {
			extra = append(extra, "default "+def)
		}
		if info, ok := h.FlagInfo[f.Name]; ok {
			if info.Grammar != "" {
				extra = append(extra, "grammar "+info.Grammar)
			}
			if len(info.Values) > 0 {
				extra = append(extra, "values "+strings.Join(info.Values, "|"))
			}
			if info.Repeatable {
				extra = append(extra, "repeatable")
			}
			if info.Min != nil || info.Max != nil {
				extra = append(extra, boundsText(info))
			}
			if info.Note != "" {
				extra = append(extra, info.Note)
			}
		} else if strings.HasSuffix(typ, "Slice") || strings.HasSuffix(typ, "Array") {
			extra = append(extra, "repeatable")
		}
		if len(extra) > 0 {
			line += "  [" + strings.Join(extra, "; ") + "]"
		}
		lines = append(lines, line)
	}
	return lines
}

func boundsText(info FlagInfo) string {
	lo, hi := "", ""
	if info.Min != nil {
		lo = strconv.Itoa(*info.Min)
	}
	if info.Max != nil {
		hi = strconv.Itoa(*info.Max)
	}
	return "range " + lo + ".." + hi
}

func metavar(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "int", "int64":
		return "n"
	case "duration":
		return "duration"
	case "stringSlice", "stringArray":
		return f.Name
	default:
		return f.Name
	}
}

// CommandSpec is §10.4's machine-readable help.
type CommandSpec struct {
	Path        []string   `json:"path"`
	Short       string     `json:"short"`
	Args        []ArgSpec  `json:"args"`
	Flags       []FlagSpec `json:"flags"`
	Subcommands []string   `json:"subcommands,omitempty"`
	Output      OutputSpec `json:"output"`
	ExitCodes   []int      `json:"exit_codes"`
	Examples    []Example  `json:"examples"`
	Mistakes    []Mistake  `json:"common_mistakes,omitempty"`
	SeeAlso     []string   `json:"see_also,omitempty"`
}

// FlagSpec is one flag of a CommandSpec.
type FlagSpec struct {
	Name       string   `json:"name"`
	Shorthand  string   `json:"shorthand,omitempty"`
	Type       string   `json:"type"`
	Repeatable bool     `json:"repeatable"`
	Default    any      `json:"default"`
	Usage      string   `json:"usage"`
	Grammar    string   `json:"grammar,omitempty"`
	Operators  []string `json:"operators,omitempty"`
	Values     []string `json:"values,omitempty"`
	Min        *int     `json:"min,omitempty"`
	Max        *int     `json:"max,omitempty"`
}

func buildCommandSpec(rt *Runtime, cmd *cobra.Command, h *Help) CommandSpec {
	path := strings.Fields(commandName(cmd))
	if path == nil {
		path = []string{}
	}
	spec := CommandSpec{
		Path:      path,
		Short:     cmd.Short,
		Args:      h.Args,
		Flags:     []FlagSpec{},
		Output:    h.Output,
		ExitCodes: h.ExitCodes,
		Examples:  h.Examples,
		Mistakes:  h.Mistakes,
		SeeAlso:   h.SeeAlso,
	}
	if spec.Args == nil {
		spec.Args = []ArgSpec{}
	}
	if spec.Examples == nil {
		spec.Examples = []Example{}
	}
	if len(spec.ExitCodes) == 0 {
		spec.ExitCodes = fallbackExitCodes
	}
	if spec.Output.Kind == "" {
		spec.Output.Kind = output.KindOpResult
	}
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			spec.Subcommands = append(spec.Subcommands, sub.Name())
		}
	}
	// Values that can only come from discovery are filled from the cache, never
	// from the network (§10.5): a cold cache leaves `values` absent rather than
	// making --help hang.
	m, haveManifest := rt.CachedManifest()
	for i := range spec.Args {
		if spec.Args[i].ValuesFrom == "discovery.collections" && haveManifest {
			spec.Args[i].Values = m.CollectionSlugs()
		}
		if spec.Args[i].ValuesFrom == "discovery.globals" && haveManifest {
			spec.Args[i].Values = m.GlobalSlugs()
		}
	}

	var names []string
	byName := map[string]*pflag.Flag{}
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		names = append(names, f.Name)
		byName[f.Name] = f
	})
	sort.Strings(names)
	for _, name := range names {
		f := byName[name]
		fs := FlagSpec{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      specType(f.Value.Type()),
			Usage:     f.Usage,
			Default:   specDefault(f),
		}
		fs.Repeatable = strings.HasSuffix(f.Value.Type(), "Slice") || strings.HasSuffix(f.Value.Type(), "Array")
		if info, ok := h.FlagInfo[f.Name]; ok {
			fs.Grammar = info.Grammar
			fs.Operators = info.Operators
			fs.Values = info.Values
			fs.Min = info.Min
			fs.Max = info.Max
			if info.Repeatable {
				fs.Repeatable = true
			}
		}
		spec.Flags = append(spec.Flags, fs)
	}
	return spec
}

func specType(t string) string {
	switch t {
	case "stringSlice", "stringArray":
		return "string"
	case "int", "int64":
		return "int"
	case "bool":
		return "bool"
	case "duration":
		return "duration"
	default:
		return t
	}
}

func specDefault(f *pflag.Flag) any {
	switch f.Value.Type() {
	case "bool":
		return f.DefValue == "true"
	case "int", "int64":
		n, err := strconv.Atoi(f.DefValue)
		if err != nil {
			return nil
		}
		return n
	case "stringSlice", "stringArray":
		return []string{}
	default:
		if f.DefValue == "" {
			return nil
		}
		return f.DefValue
	}
}

// printer is a tiny text builder; it exists so every help block indents and
// spaces identically and the golden files stay diffable.
type printer struct {
	w io.Writer
}

func (p *printer) line(s string)   { fmt.Fprintln(p.w, s) }
func (p *printer) blank()          { fmt.Fprintln(p.w) }
func (p *printer) indent(s string) { fmt.Fprintln(p.w, "  "+s) }

func (p *printer) section(title string) {
	fmt.Fprintf(p.w, "\n%s\n", title)
}

func (p *printer) wrap(s string) {
	for _, l := range strings.Split(s, "\n") {
		fmt.Fprintln(p.w, l)
	}
}
