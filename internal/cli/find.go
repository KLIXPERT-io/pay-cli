package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// Deps — the data commands' view of the shared *Runtime.
//
// app.go owns the process surface; this adapter resolves the two things every
// data command needs (the REST client and the discovery index) exactly once and
// exposes the small accessor set the command bodies use. Nothing in these files
// touches os.Stdout/os.Stderr/os.Stdin, os.Args, os.Getenv, os.Exit or
// time.Now (§3.1): all of them arrive through the Runtime.
// ---------------------------------------------------------------------------

// Deps is one data command's resolved dependencies.
type Deps struct {
	// RT is the shared runtime.
	RT *Runtime
	// Client is the configured Payload REST client.
	Client *payload.Client
	// Manifest is the discovery index, or nil when discovery could not answer.
	// A nil manifest disables every client-side rejection rather than inventing
	// one, which is §9.3's tri-state rule applied at the top.
	Manifest *discovery.Manifest
	// FetchURL downloads a remote upload source (§13). It is a field so unit
	// tests never reach the network.
	FetchURL func(ctx context.Context, rawurl string) (body io.ReadCloser, contentType string, size int64, err error)

	// memo is §7.11's reactive operator memo for THIS invocation: what
	// readFlags.build sent, so runData can attribute a server failure to the
	// operator that carried it without every command body repeating the test.
	// See opmemo.go.
	memo operatorMemo
}

// dataDeps resolves the client and the discovery index for one invocation.
func dataDeps(ctx context.Context, rt *Runtime) (*Deps, error) {
	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}
	d := &Deps{RT: rt, Client: client, FetchURL: remoteFetcher(rt)}
	m, err := rt.Discovery(ctx)
	if err != nil {
		if fatalDiscoveryError(err) {
			return nil, err
		}
		// Discovery is an optimisation for a read or a write, not a gate: the
		// command runs unvalidated rather than failing on a fact it never
		// needed.
		rt.Warn(output.Warning{
			Code:    "discovery_unavailable",
			Message: "the capability manifest could not be built, so no client-side validation ran: " + err.Error(),
			Hint:    "run `pay discover --refresh` to see why",
		})
	} else {
		d.Manifest = m
	}
	return d, nil
}

// fatalDiscoveryError separates "this is not a Payload endpoint / these
// credentials are wrong" from "discovery happened to fail". The first kind
// makes every subsequent request meaningless; the second does not.
func fatalDiscoveryError(err error) bool {
	for _, code := range []apierr.Code{
		apierr.CodeEndpointNotPayload, apierr.CodeAuthInvalid, apierr.CodeAuthRequired,
		apierr.CodeAuthMissing, apierr.CodeAuthLocked, apierr.CodeAuthCollectionUnknown,
		apierr.CodeConfigMissing, apierr.CodeBaseURLInvalid, apierr.CodeProfileUnknown,
	} {
		if apierr.HasCode(err, code) {
			return true
		}
	}
	return false
}

// remoteFetcher builds the §13 URL downloader over the injected transport. It
// deliberately uses http.Client.Get rather than constructing an http.Request:
// §3.1 reserves request construction for internal/payload/transport.go, and
// this fetch does not go to the Payload server at all.
func remoteFetcher(rt *Runtime) func(context.Context, string) (io.ReadCloser, string, int64, error) {
	return func(ctx context.Context, rawurl string) (io.ReadCloser, string, int64, error) {
		client := &http.Client{Transport: rt.App.HTTP, Timeout: 2 * time.Minute}
		if rt.Cfg != nil && rt.Cfg.Timeout > 0 {
			client.Timeout = rt.Cfg.Timeout * 4
		}
		resp, err := client.Get(rawurl)
		if err != nil {
			return nil, "", 0, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, "", 0, apierr.New(apierr.CodeNetworkUnreachable,
				"%s answered HTTP %d", redact.URL(rawurl), resp.StatusCode)
		}
		_ = ctx
		return resp.Body, resp.Header.Get("Content-Type"), resp.ContentLength, nil
	}
}

func (d *Deps) now() time.Time {
	if d == nil || d.RT == nil {
		return time.Time{}
	}
	return d.RT.Now()
}

func (d *Deps) log() *slog.Logger {
	if d == nil || d.RT == nil || d.RT.Log == nil {
		return logging.Discard()
	}
	return d.RT.Log
}

func (d *Deps) errStream() io.Writer {
	if d == nil || d.RT == nil {
		return io.Discard
	}
	return d.RT.Stderr()
}

func (d *Deps) inStream() io.Reader {
	if d == nil || d.RT == nil {
		return strings.NewReader("")
	}
	return d.RT.Stdin()
}

func (d *Deps) cfg() *config.Resolved {
	if d == nil || d.RT == nil || d.RT.Cfg == nil {
		return &config.Resolved{}
	}
	return d.RT.Cfg
}

// requireClient returns the HTTP client or the §4 config_missing error.
func (d *Deps) requireClient() (*payload.Client, error) {
	if d != nil && d.Client != nil {
		return d.Client, nil
	}
	if d != nil && d.RT != nil && d.RT.Cfg != nil {
		if err := d.RT.Cfg.RequireBaseURL(); err != nil {
			return nil, err
		}
	}
	return nil, apierr.New(apierr.CodeConfigMissing,
		"no Payload endpoint is configured for this profile").
		WithHint("run `pay auth login --profile <name> --base-url https://example.com --api-key <key>`")
}

// confirmer returns §12.1's prompt, never nil.
func (d *Deps) confirmer() *safety.Confirmer {
	if d != nil && d.RT != nil {
		return d.RT.Confirmer()
	}
	return &safety.Confirmer{In: d.inStream(), Out: d.errStream()}
}

// auditor returns the §12.7 logger, never nil.
func (d *Deps) auditor() *audit.Logger {
	if d == nil || d.RT == nil {
		return audit.New(audit.Options{})
	}
	return d.RT.Audit()
}

// ---------------------------------------------------------------------------
// the data-command RunE adapter
// ---------------------------------------------------------------------------

// dataHandler is a data command's body. It receives the cobra command because
// §12.3's --limit guard and §4.5's precedence both need to tell "flag not
// given" from "flag given its default value".
type dataHandler func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error)

// runData adapts a dataHandler to cobra's RunE.
func runData(rt *Runtime, name string, h dataHandler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt.Command = name
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		var d *Deps
		env, err := func() (*output.Envelope, error) {
			var err error
			d, err = dataDeps(ctx, rt)
			if err != nil {
				return nil, err
			}
			return h(ctx, cmd, d, args)
		}()
		// §7.11, write half. Every --where command returns through here, so the
		// memo is learned once instead of at the ~10 error returns of runFind,
		// runCount, runUpdate, runDelete and runPublish — none of which could
		// be relied on to remember. d.memo carries what readFlags.build
		// actually sent; the attribution test itself lives in discovery.
		d.learnFromFailure(err)
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

// mergeCommandMeta copies the command-owned meta fields over the runtime's.
func mergeCommandMeta(base *output.Meta, extra output.Meta) {
	if extra.DateField != nil {
		base.DateField = extra.DateField
	}
	if extra.Since != nil {
		base.Since = extra.Since
	}
	if extra.Until != nil {
		base.Until = extra.Until
	}
	if extra.Locale.Requested != nil {
		base.Locale.Requested = extra.Locale.Requested
	}
	if extra.Locale.Fallback != nil {
		base.Locale.Fallback = extra.Locale.Fallback
	}
	if len(extra.SearchedFields) > 0 {
		base.SearchedFields = extra.SearchedFields
	}
	if extra.DryRun {
		base.DryRun = true
	}
}

// ---------------------------------------------------------------------------
// meta
// ---------------------------------------------------------------------------

// runStats records when the command started, for the audit entry's duration.
type runStats struct {
	start time.Time
	d     *Deps
}

func (d *Deps) beginRun() *runStats {
	return &runStats{start: d.now(), d: d}
}

// meta builds the envelope metadata for a finished command.
func (r *runStats) meta() output.Meta {
	// Only the fields Runtime.Meta() cannot know are filled in here; Runtime.emit
	// merges them over the runtime's own block, which already carries the
	// request id, the profile, the cache state and the HTTP counters.
	var m output.Meta
	if r == nil || r.d == nil {
		return m
	}
	m.DryRun = r.d.cfg().DryRun
	return m
}

// ---------------------------------------------------------------------------
// target resolution
// ---------------------------------------------------------------------------

// collTarget is everything the manifest knows about one collection. Every
// field may be nil: that is §9.3's tri-state, and it means "send the request".
type collTarget struct {
	Slug  string
	Coll  *discovery.Collection
	Shard *discovery.Shard
}

// idType returns the collection's id type, or "" when it was never learned.
func (t *collTarget) idType() string {
	if t == nil || t.Coll == nil {
		return ""
	}
	if t.Coll.IDType == discovery.IDTypeUnknown {
		return ""
	}
	return t.Coll.IDType
}

// profileFlag renders the ` --profile X` a hand-off command needs so that,
// copied verbatim, it reaches the SAME server the write just went to.
//
// Every verify_write next.cmd omitted it, so an agent working against a
// non-default profile got `pay get pages 30 --depth 0` and silently read the
// default server instead. It follows reinvocation()'s rule exactly: only a
// profile a config file actually defines is re-emitted, because `--profile
// default` on an env-configured, config-less run is a profile_unknown error —
// a worse failure than omitting the flag.
func (d *Deps) profileFlag() string {
	cfg := d.cfg()
	if cfg == nil || !cfg.ProfileDefined || cfg.Profile == "" {
		return ""
	}
	return " --profile " + shellQuote(cfg.Profile)
}

// checkID is the ONLY way a command should validate a document id.
//
// §7.6(a) requires a transparency warning whenever the local id check is
// SKIPPED — when PayCLI never learned the collection's id type, a malformed id
// comes back as an opaque server 500 instead of a PayCLI refusal, and the agent
// has to be able to tell "PayCLI checked this" from "PayCLI had no idea". Nine
// commands validate ids; wrapping apierr.CheckID once here is what stops the
// tenth from silently shipping without the warning.
func (d *Deps) checkID(t *collTarget, slug, id string) *apierr.Error {
	idType := t.idType()
	if idType == "" {
		if slug == "" && t != nil {
			slug = t.Slug
		}
		if d != nil && d.RT != nil {
			d.RT.Warn(output.IDTypeUnknownWarning(slug, d.cfg().Profile))
		}
	}
	return apierr.CheckID(slug, id, idType)
}

func (t *collTarget) flags() discovery.Flags {
	if t == nil || t.Coll == nil {
		return discovery.Flags{}
	}
	return t.Coll.Flags
}

func (t *collTarget) singular() string {
	if t == nil || t.Coll == nil {
		return ""
	}
	return t.Coll.Labels.Singular
}

// dateFields prefers the shard (authoritative) and falls back to the index.
func (t *collTarget) dateFields() []string {
	if t == nil {
		return nil
	}
	if t.Shard != nil && len(t.Shard.Fields) > 0 {
		return t.Shard.DateFields()
	}
	if t.Coll != nil {
		return t.Coll.DateFields
	}
	return nil
}

// schema projects the manifest into the slice internal/payload/query needs.
// Every slice keeps the nil-means-unknown contract.
func (t *collTarget) schema(cfg *config.Resolved) query.Schema {
	s := query.Schema{}
	if t != nil {
		s.Collection = t.Slug
	}
	if cfg != nil {
		s.DBAdapter = cfg.DBAdapter
		if cfg.DBAdapter != "" {
			s.DBAdapterSource = config.SourceConfigured
		}
	}
	if t == nil || t.Shard == nil || len(t.Shard.Fields) == 0 {
		if t != nil && t.Coll != nil && len(t.Coll.DateFields) > 0 {
			s.DateFields = t.Coll.DateFields
		}
		return s
	}
	s.Queryable = t.Shard.QueryablePaths()
	s.Sortable = t.Shard.SortablePaths()
	s.Selectable = t.Shard.Paths()
	s.DateFields = t.Shard.DateFields()
	return s
}

// envTarget builds the envelope's target block.
func (t *collTarget) envTarget(id any) *output.Target {
	out := &output.Target{Kind: "collection", Slug: t.Slug, ID: id}
	if t.Coll != nil {
		out.Singular = t.Coll.Labels.Singular
		if t.Coll.IDType != "" {
			out.IDType = t.Coll.IDType
		}
	}
	return out
}

// collection resolves a slug against the manifest. An unknown slug is
// collection_unknown (exit 10) with did_you_mean; a nil manifest resolves to a
// bare target, because a fact PayCLI never learned is never a local rejection.
func (d *Deps) collection(ctx context.Context, slug string) (*collTarget, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "a collection slug is required").
			WithHint("run `pay collections` for this project's slugs")
	}
	t := &collTarget{Slug: slug}
	if d == nil || d.Manifest == nil {
		return t, nil
	}
	c, err := d.Manifest.ResolveCollection(slug)
	if err != nil {
		return nil, err
	}
	t.Coll = c
	t.Shard = d.shardFor(ctx, slug, cache.KindCollection)
	return t, nil
}

// shardFor loads one entity's field shard. A miss is not an error: §9.3's
// tri-state rule turns "fields unknown" into "send the request".
func (d *Deps) shardFor(ctx context.Context, slug string, kind cache.EntityKind) *discovery.Shard {
	if d == nil || d.RT == nil {
		return nil
	}
	s, ok := d.RT.Shard(ctx, slug, kind)
	if !ok {
		d.log().Debug("field shard unavailable", "slug", slug, "kind", string(kind))
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// shared help text
// ---------------------------------------------------------------------------

// draftTrapHelp is §9.6.2's mandatory verbatim wording. It appears in the help
// of every read command that can return an unpublished document, and a unit
// test asserts the exact sentences.
const draftTrapHelp = `A read without --draft does NOT filter out unpublished documents — documents that were
never published are returned with _status:"draft". To get only published content, always pass
--published-only (_status = published). --draft additionally swaps in the newest draft for
documents that do have a published version.`

// ---------------------------------------------------------------------------
// read flags
// ---------------------------------------------------------------------------

// readFlags is the union of §9.2's read flags. One struct serves find, get and
// count; each command registers only the subset it accepts.
type readFlags struct {
	where      []string
	or         []string
	whereJSON  string
	whereRaw   string
	whereStyle string
	q          string
	ids        []string
	idsCSV     string

	sort  []string
	limit int
	page  int
	all   bool
	max   int

	selectFields  []string
	selectExclude []string
	depth         int
	populate      []string
	joins         []string

	draft         bool
	draftOnly     bool
	publishedOnly bool
	trash         bool

	since     string
	until     string
	dateField string

	locale         string
	fallbackLocale string

	countOnly       bool
	noValidateSort  bool
	noValidateWhere bool
}

// registerFilterFlags adds the §9.4 filter DSL.
func (f *readFlags) registerFilterFlags(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.where, "where", nil, "filter term 'PATH OP VALUE' (repeatable, ANDed)")
	fl.StringArrayVar(&f.or, "or", nil, "filter term 'PATH OP VALUE' (repeatable, one OR group ANDed with --where)")
	fl.StringVar(&f.whereJSON, "where-json", "", "raw Payload where clause as JSON (replaces --where/--or)")
	fl.StringVar(&f.whereRaw, "where-raw", "", "literal query string appended verbatim")
	fl.StringVar(&f.whereStyle, "where-style", "", "where encoding: json (default) or qs")
	fl.StringVar(&f.q, "q", "", "substring search across this collection's text fields")
	fl.StringArrayVar(&f.ids, "id", nil, "match this id (repeatable)")
	fl.StringVar(&f.idsCSV, "ids", "", "match these ids (comma separated)")
	fl.BoolVar(&f.draftOnly, "draft-only", false, "only documents whose _status is draft")
	fl.BoolVar(&f.publishedOnly, "published-only", false, "only published documents (_status = published) — REQUIRED to exclude drafts")
	fl.BoolVar(&f.trash, "trash", false, "include soft-deleted documents")
	fl.StringVar(&f.since, "since", "", "lower date bound: "+query.SinceForms)
	fl.StringVar(&f.until, "until", "", "upper date bound: same grammar as --since")
	fl.StringVar(&f.dateField, "date-field", "", "date field --since/--until resolve against (default publishedAt when published-scoped, else updatedAt)")
	fl.BoolVar(&f.noValidateWhere, "no-validate-where", false, "skip client-side where-path validation")
}

// registerSelectionFlags adds the shaping flags shared by find and get.
func (f *readFlags) registerSelectionFlags(cmd *cobra.Command, depthDefault int) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.selectFields, "select", nil, "return only these fields")
	fl.StringSliceVar(&f.selectExclude, "select-exclude", nil, "return every field except these")
	fl.IntVar(&f.depth, "depth", depthDefault, "relationship expansion depth (0 = bare ids)")
	fl.StringArrayVar(&f.populate, "populate", nil, "'collection:field1,field2' — expand only these fields of a relation (repeatable)")
	fl.StringArrayVar(&f.joins, "joins", nil, "'field:limit=5,sort=-createdAt' — tune a join field (repeatable)")
	fl.BoolVar(&f.draft, "draft", false, "swap in the newest draft for documents that have a published version")
	fl.StringVar(&f.locale, "locale", "", "locale code, or 'all'")
	fl.StringVar(&f.fallbackLocale, "fallback-locale", "", "fallback locale ('none' is the default on a localised project)")
}

// registerPagingFlags adds page-size and pagination.
func (f *readFlags) registerPagingFlags(cmd *cobra.Command, limitDefault int) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.sort, "sort", nil, "sort field(s); prefix with - for descending")
	fl.IntVar(&f.limit, "limit", limitDefault, "page size")
	fl.IntVar(&f.page, "page", 0, "page number (1-based)")
	fl.BoolVar(&f.all, "all", false, "page through every match, capped by --max")
	fl.IntVar(&f.max, "max", 1000, "maximum documents to collect with --all")
	fl.BoolVar(&f.noValidateSort, "no-validate-sort", false, "skip client-side sort-field validation")
}

// ---------------------------------------------------------------------------
// query construction
// ---------------------------------------------------------------------------

// builtQuery is the result of turning the read flags into a request.
type builtQuery struct {
	Params    query.Params
	Warnings  []output.Warning
	DateField *string
	Since     *string
	Until     *string
	Locale    output.Locale
	QFields   []string
	Published bool
}

// build compiles the read flags into query.Params, running every §9.3
// client-side check first. Nothing here performs I/O.
func (f *readFlags) build(cmd *cobra.Command, d *Deps, t *collTarget) (*builtQuery, error) {
	cfg := d.cfg()
	out := &builtQuery{}
	schema := t.schema(cfg)

	// --- where -------------------------------------------------------------
	in := query.Input{
		Where:         f.where,
		Or:            f.or,
		WhereJSON:     f.whereJSON,
		IDs:           mergeIDs(f.ids, f.idsCSV),
		IDType:        t.idType(),
		DraftOnly:     f.draftOnly,
		PublishedOnly: f.publishedOnly,
		Q:             f.q,
		Options:       query.ParseOptions{ReadFile: os.ReadFile},
	}
	if f.q != "" {
		out.QFields = qFields(t)
		if len(out.QFields) == 0 {
			return nil, apierr.New(apierr.CodeUnknownField,
				"--q needs at least one text field on %q and none is known", t.Slug).
				WithHint("run `pay discover --refresh`, or filter explicitly with --where 'field contains text'")
		}
		in.QFields = out.QFields
	}

	// --- --since / --until -------------------------------------------------
	published := f.publishedOnly || mentionsStatus(f.where) || mentionsStatus(f.or) ||
		strings.Contains(f.whereJSON, "_status")
	out.Published = published
	if f.since != "" || f.until != "" {
		field, defaulted := resolveDateField(f.dateField, published, t)
		if err := query.ValidateDateField(field, schema); err != nil {
			return nil, err
		}
		var sincePtr, untilPtr *time.Time
		if f.since != "" {
			ts, err := query.ParseSince(f.since, d.now())
			if err != nil {
				return nil, err
			}
			sincePtr = &ts
			s := ts.UTC().Format(time.RFC3339)
			out.Since = &s
		}
		if f.until != "" {
			ts, err := query.ParseSince(f.until, d.now())
			if err != nil {
				return nil, err
			}
			untilPtr = &ts
			s := ts.UTC().Format(time.RFC3339)
			out.Until = &s
		}
		in.Extra = append(in.Extra, query.DateTerm(field, sincePtr, untilPtr))
		out.DateField = &field
		if defaulted {
			out.Warnings = append(out.Warnings, dateFieldDefaultedWarning(t, field, published, out.Since, out.Until))
		}
	} else if f.dateField != "" {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--date-field only means something together with --since or --until").
			WithHint("add --since 7d, or drop --date-field")
	}

	where, err := query.Build(in)
	if err != nil {
		return nil, err
	}
	// §7.11, read half. Every --where command funnels through here, so this is
	// the one place that has both the collection and the operators before the
	// request leaves. It goes to rt.Warn(), not to out.Warnings: builtQuery's
	// warnings are attached to the SUCCESS envelope only, and the run that most
	// needs to be told "this operator failed here before" is the one that is
	// about to fail again.
	d.rememberQueryOperators(t, query.ExtractOperators(where))
	if !f.noValidateWhere {
		if err := query.ValidateWhere(where, schema); err != nil {
			return nil, err
		}
	}
	if w, err := f.validateEnums(where, t); err != nil {
		return nil, err
	} else if w != nil {
		out.Warnings = append(out.Warnings, *w)
	}

	// --- sort / select -----------------------------------------------------
	if !f.noValidateSort {
		if err := query.ValidateSort(f.sort, schema); err != nil {
			return nil, err
		}
	}
	if err := query.ValidateSelect(f.selectFields, schema); err != nil {
		return nil, err
	}
	if err := query.ValidateSelect(f.selectExclude, schema); err != nil {
		return nil, err
	}

	// --- feature gates (§9.3, tri-state) -----------------------------------
	flags := t.flags()
	if f.draft || f.draftOnly || f.publishedOnly {
		if err := checkDrafts(t.Slug, flags.Drafts); err != nil {
			return nil, err
		}
	}
	if f.trash {
		if err := checkTrash(t.Slug, flags.Trash); err != nil {
			return nil, err
		}
	}

	// --- paging ------------------------------------------------------------
	p := query.Params{
		Where:         where,
		WhereRaw:      f.whereRaw,
		Sort:          f.sort,
		Select:        f.selectFields,
		SelectExclude: f.selectExclude,
	}
	if f.whereStyle != "" {
		style, err := query.ParseWhereStyle(f.whereStyle)
		if err != nil {
			return nil, err
		}
		p.WhereStyle = style
	}
	if cmd != nil && cmd.Flags().Lookup("limit") != nil {
		limit := f.limit
		if err := query.ValidateLimit(&limit); err != nil {
			return nil, err
		}
		p.Limit = query.IntPtr(limit)
	}
	if f.page > 0 {
		p.Page = f.page
	}
	// depth is always sent: Payload's own default is 2 and PayCLI's is 0
	// (§9.6.1), so omitting it would silently change the answer.
	if cmd != nil && cmd.Flags().Lookup("depth") != nil {
		depth := f.depth
		if depth < 0 {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--depth must be >= 0; got %d", depth)
		}
		p.Depth = query.IntPtr(depth)
	}
	if f.draft {
		p.Draft = query.BoolPtr(true)
	}
	if f.trash {
		p.Trash = query.BoolPtr(true)
	}

	pop, err := parsePopulate(f.populate)
	if err != nil {
		return nil, err
	}
	p.Populate = pop
	joins, err := parseJoins(f.joins)
	if err != nil {
		return nil, err
	}
	p.Joins = joins

	// --- locale ------------------------------------------------------------
	loc, locWarn, err := resolveLocale(d, t, f.locale, f.fallbackLocale)
	if err != nil {
		return nil, err
	}
	if locWarn != nil {
		out.Warnings = append(out.Warnings, *locWarn)
	}
	if loc.Requested != nil {
		p.Locale = *loc.Requested
	}
	if loc.Fallback != nil {
		p.FallbackLocale = *loc.Fallback
	}
	out.Locale = loc
	out.Params = p

	return out, nil
}

// applyMeta folds the query-derived facts into the envelope metadata.
func (b *builtQuery) applyMeta(m output.Meta) output.Meta {
	if b == nil {
		return m
	}
	m.DateField = b.DateField
	// §9.4: --q always reports the fields it searched, so an unexpectedly wide
	// (or narrow) match set is explainable without re-reading the schema.
	if len(b.QFields) > 0 {
		m.SearchedFields = append([]string(nil), b.QFields...)
	}
	m.Since = b.Since
	m.Until = b.Until
	if b.Locale.Requested != nil || b.Locale.Fallback != nil {
		m.Locale = b.Locale
	}
	return m
}

// ---------------------------------------------------------------------------
// re-invocation (§10.1)
//
// next.cmd and every alternative promise a LITERALLY RUNNABLE command with the
// current flags and profile already baked in — the agent skill tells the agent
// to copy it and never to reconstruct it. Rebuilding one from the collection
// slug alone is therefore the worst failure this CLI can have: the follow-up
// runs without the filter, returns the wrong rows (or none at all) and exits 0,
// so nothing in the envelope contradicts the answer the agent then gives.
//
// Every re-emitted read invocation goes through reinvocation(), which renders
// the RESOLVED query rather than a fragment of it.
// ---------------------------------------------------------------------------

// reinvokeOpts overrides the paging facts of a re-emitted invocation. The zero
// value means "exactly the query this run resolved".
type reinvokeOpts struct {
	// Page emits --page N. Ignored when All is set, which starts from the top.
	Page int
	// Limit emits --limit N instead of the limit this run resolved. It exists
	// for `pay count`, whose own flag set has no --limit to resolve.
	Limit int
	// All emits --all --max Max.
	All bool
	Max int
	// Output replaces the resolved --output (the jsonl streaming alternative).
	Output string
}

// reinvocation renders the resolved read invocation as a shell-safe command
// string, plus the same parameter set as structured args for a caller that
// would rather not re-parse a string (§10.1's next.args).
//
// Everything that decides WHICH rows come back is re-emitted: the profile, the
// whole filter, the sort, the projection, the draft scope, the paging and the
// output format. The user's own --where/--or terms are re-emitted verbatim
// because they stay legible; the two inputs that are not stable over time — a
// relative --since/--until and a defaulted --date-field — are re-emitted in
// their resolved absolute form, so the second run asks the same question the
// first one did.
func (f *readFlags) reinvocation(d *Deps, t *collTarget, b *builtQuery, o reinvokeOpts) (string, map[string]any) {
	slug := ""
	if t != nil {
		slug = t.Slug
	}
	argv := []string{"pay", "find", shellQuote(slug)}
	args := map[string]any{"command": "find", "collection": slug}

	str := func(flag, key, value string) {
		argv = append(argv, "--"+flag, shellQuote(value))
		args[key] = value
	}
	num := func(flag, key string, value int) {
		argv = append(argv, "--"+flag, strconv.Itoa(value))
		args[key] = value
	}
	on := func(flag, key string) {
		argv = append(argv, "--"+flag)
		args[key] = true
	}
	list := func(flag, key string, values []string) {
		if len(values) == 0 {
			return
		}
		for _, v := range values {
			argv = append(argv, "--"+flag, shellQuote(v))
		}
		args[key] = append([]string(nil), values...)
	}

	// --- profile -----------------------------------------------------------
	// Only a profile that a config file actually defines is re-emitted:
	// `--profile default` on an env-configured, config-less run is a
	// profile_unknown error, which would be a worse failure than omitting it.
	cfg := d.cfg()
	if cfg != nil && cfg.ProfileDefined && cfg.Profile != "" {
		str("profile", "profile", cfg.Profile)
	}

	// --- filter ------------------------------------------------------------
	list("where", "where", f.where)
	list("or", "or", f.or)
	if f.whereJSON != "" {
		str("where-json", "where_json", f.whereJSON)
	}
	if f.whereRaw != "" {
		str("where-raw", "where_raw", f.whereRaw)
	}
	if f.whereStyle != "" {
		str("where-style", "where_style", f.whereStyle)
	}
	if f.q != "" {
		str("q", "q", f.q)
	}
	list("id", "ids", mergeIDs(f.ids, f.idsCSV))
	if f.publishedOnly {
		on("published-only", "published_only")
	}
	if f.draftOnly {
		on("draft-only", "draft_only")
	}
	if f.trash {
		on("trash", "trash")
	}
	if b != nil && b.Since != nil {
		str("since", "since", *b.Since)
	}
	if b != nil && b.Until != nil {
		str("until", "until", *b.Until)
	}
	if b != nil && b.DateField != nil && (b.Since != nil || b.Until != nil) {
		str("date-field", "date_field", *b.DateField)
	}
	if f.noValidateWhere {
		on("no-validate-where", "no_validate_where")
	}

	// --- projection --------------------------------------------------------
	list("select", "select", f.selectFields)
	list("select-exclude", "select_exclude", f.selectExclude)
	list("populate", "populate", f.populate)
	list("joins", "joins", f.joins)
	if b != nil && b.Params.Depth != nil && *b.Params.Depth != 0 {
		num("depth", "depth", *b.Params.Depth)
	}
	if f.draft {
		on("draft", "draft")
	}
	if b != nil && b.Params.Locale != "" {
		str("locale", "locale", b.Params.Locale)
	}
	if b != nil && b.Params.FallbackLocale != "" {
		str("fallback-locale", "fallback_locale", b.Params.FallbackLocale)
	}

	// --- paging ------------------------------------------------------------
	list("sort", "sort", f.sort)
	if f.noValidateSort {
		on("no-validate-sort", "no_validate_sort")
	}
	limit := o.Limit
	if limit == 0 && b != nil && b.Params.Limit != nil {
		limit = *b.Params.Limit
	}
	if limit > 0 {
		num("limit", "limit", limit)
	}
	switch {
	case o.All:
		on("all", "all")
		if o.Max > 0 {
			num("max", "max", o.Max)
		}
	case o.Page > 0:
		num("page", "page", o.Page)
	}

	// --- output ------------------------------------------------------------
	out := o.Output
	if out == "" && cfg != nil {
		out = cfg.Output
	}
	if out != "" && out != config.DefaultOutput {
		str("output", "output", out)
	}

	return strings.Join(argv, " "), args
}

// shellQuote renders one argv element so that a POSIX shell passes it through
// unchanged. The allowlist is deliberate: a --where term can legally contain a
// space, a quote, a glob character, a $ or a ;, and "literally runnable" is a
// promise the envelope has to keep for every one of them.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == ',', r == '+', r == '@', r == '=':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// warning codes owned by the data commands.
const (
	warnMixedStatus        = "mixed_status_results"
	warnDateFieldDefaulted = "date_field_defaulted"
	warnWriteShapeUnknown  = "write_shape_unknown"
	warnUnknownBlockType   = "unknown_block_type"
	warnFilenameChanged    = "filename_changed"
	warnAuditWriteFailed   = audit.WarnCode
)

// mergeIDs folds --id and --ids into one list.
func mergeIDs(repeated []string, csv string) []string {
	out := append([]string(nil), repeated...)
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// mentionsStatus reports whether any raw filter term touches _status.
func mentionsStatus(terms []string) bool {
	for _, t := range terms {
		if strings.HasPrefix(strings.TrimSpace(t), "_status") {
			return true
		}
	}
	return false
}

// resolveDateField implements §9.4's field resolution. It never guesses:
// publishedAt is only chosen when the collection provably has that date field
// and the query is published-scoped.
func resolveDateField(flag string, published bool, t *collTarget) (field string, defaulted bool) {
	if strings.TrimSpace(flag) != "" {
		return strings.TrimSpace(flag), false
	}
	if published {
		for _, f := range t.dateFields() {
			if f == "publishedAt" {
				return "publishedAt", true
			}
		}
	}
	return "updatedAt", true
}

func dateFieldDefaultedWarning(t *collTarget, field string, published bool, since, until *string) output.Warning {
	var parts []string
	if since != nil {
		parts = append(parts, fmt.Sprintf("--since resolved against %s >= %s", field, *since))
	}
	if until != nil {
		parts = append(parts, fmt.Sprintf("--until resolved against %s <= %s", field, *until))
	}
	why := fmt.Sprintf("%s is the default date field", t.Slug)
	alt := "publishedAt"
	if field == "publishedAt" {
		why = fmt.Sprintf("%s has a publishedAt field and the query is published-scoped", t.Slug)
		alt = "updatedAt"
	} else if published {
		why = fmt.Sprintf("%s has no publishedAt date field", t.Slug)
		alt = "createdAt"
	} else {
		why = fmt.Sprintf("%s: the query is not published-scoped", t.Slug)
	}
	return output.Warning{
		Code:    warnDateFieldDefaulted,
		Message: strings.Join(parts, "; ") + " (" + why + ")",
		Paths:   []string{field},
		Hint:    "override with --date-field " + alt,
	}
}

// qFields picks the --q search fields in §9.4's documented order.
func qFields(t *collTarget) []string {
	if t == nil || t.Shard == nil {
		return nil
	}
	textish := map[string]bool{
		discovery.TypeText:     true,
		discovery.TypeTextarea: true,
		discovery.TypeEmail:    true,
	}
	byPath := map[string]discovery.Field{}
	var order []string
	for _, f := range t.Shard.Fields {
		if !f.Queryable || !textish[f.PayloadType] || f.HasMany {
			continue
		}
		byPath[f.Path] = f
		order = append(order, f.Path)
	}
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		if _, ok := byPath[path]; !ok {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	if t.Coll != nil && t.Coll.TitleField != nil {
		add(*t.Coll.TitleField)
	}
	for _, preferred := range []string{"name", "title", "slug", "email"} {
		add(preferred)
	}
	for _, path := range order {
		if len(out) >= query.MaxQFields {
			break
		}
		add(path)
	}
	if len(out) > query.MaxQFields {
		out = out[:query.MaxQFields]
	}
	return out
}

// validateEnums implements §9.3's enum row: an invalid select/radio value in a
// where clause is a 500 on Postgres and zero documents on Mongo, so it is
// rejected locally — but only when the option list was actually enumerated.
func (f *readFlags) validateEnums(w query.Where, t *collTarget) (*output.Warning, error) {
	if t == nil || t.Shard == nil || w.IsEmpty() {
		return nil, nil
	}
	for _, path := range query.ExtractPaths(w) {
		fld, ok := t.Shard.Field(path)
		if !ok || len(fld.Options) == 0 {
			continue
		}
		if fld.OptionsSource == discovery.SourceUnknown || fld.OptionsSource == discovery.SourceNA {
			continue
		}
		for _, v := range valuesFor(w, path) {
			s, isString := v.(string)
			if !isString || s == "" {
				continue
			}
			if containsString(fld.Options, s) {
				continue
			}
			return nil, apierr.New(apierr.CodeInvalidOption,
				"%q is not a valid value for %s.%s", s, t.Slug, path).
				WithDidYouMean(apierr.DidYouMean(s, fld.Options)...).
				WithHint("%s", "allowed values: "+strings.Join(fld.Options, ", "))
		}
	}
	return nil, nil
}

// valuesFor collects the scalar operands used against one path.
func valuesFor(w query.Where, path string) []any {
	var out []any
	var walk func(v any)
	walk = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			if list, isList := v.([]any); isList {
				for _, item := range list {
					walk(item)
				}
			}
			return
		}
		for k, child := range m {
			switch strings.ToLower(k) {
			case "and", "or":
				walk(child)
			default:
				if k != path {
					walk(child)
					continue
				}
				ops, isOps := child.(map[string]any)
				if !isOps {
					continue
				}
				for _, operand := range ops {
					switch val := operand.(type) {
					case []any:
						out = append(out, val...)
					default:
						out = append(out, val)
					}
				}
			}
		}
	}
	walk(map[string]any(w))
	return out
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// checkDrafts is §9.3's drafts row: reject only on a proven false.
func checkDrafts(slug string, drafts *bool) error {
	if drafts == nil || *drafts {
		return nil
	}
	return apierr.New(apierr.CodeFeatureUnavailable,
		"%q does not have drafts enabled, so it has no _status field and no draft parameter", slug).
		WithHint("drop --draft/--draft-only/--published-only, or enable versions.drafts on the collection in payload.config.ts")
}

// checkTrash is the same rule for the trash feature.
func checkTrash(slug string, trash *bool) error {
	if trash == nil || *trash {
		return nil
	}
	return apierr.New(apierr.CodeFeatureUnavailable,
		"%q does not have trash enabled, so it has no soft-deleted documents", slug).
		WithHint("drop --trash; a delete on this collection is permanent")
}

// parsePopulate turns 'collection:field1,field2' into populate[c][f]=true.
func parsePopulate(specs []string) (map[string][]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string][]string{}
	for _, spec := range specs {
		coll, fields, ok := strings.Cut(spec, ":")
		coll = strings.TrimSpace(coll)
		if !ok || coll == "" || strings.TrimSpace(fields) == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--populate %q is not 'collection:field1,field2'", spec).
				WithHint("example: --populate 'media:filename,url'")
		}
		for _, f := range strings.Split(fields, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				out[coll] = append(out[coll], f)
			}
		}
	}
	return out, nil
}

// parseJoins turns 'field:limit=5,sort=-createdAt' into joins[field][k]=v.
func parseJoins(specs []string) (map[string]map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string]map[string]string{}
	for _, spec := range specs {
		field, rest, ok := strings.Cut(spec, ":")
		field = strings.TrimSpace(field)
		if !ok || field == "" || strings.TrimSpace(rest) == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--joins %q is not 'field:key=value,key=value'", spec).
				WithHint("example: --joins 'relatedPosts:limit=5,sort=-createdAt'")
		}
		if out[field] == nil {
			out[field] = map[string]string{}
		}
		for _, pair := range strings.Split(rest, ",") {
			k, v, ok := strings.Cut(pair, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"--joins %q: %q is not key=value", spec, pair)
			}
			out[field][k] = strings.TrimSpace(v)
		}
	}
	return out, nil
}

// resolveLocale implements §9.6.5 / §7.9: validate only when the true codes are
// known, and default fallback-locale to none on a localised project so an
// untranslated field comes back null instead of the default locale's text.
func resolveLocale(d *Deps, t *collTarget, locale, fallback string) (output.Locale, *output.Warning, error) {
	cfg := d.cfg()
	if locale == "" {
		locale = cfg.Locale
	}
	if fallback == "" {
		fallback = cfg.FallbackLocale
	}

	var loc discovery.Localization
	if d != nil && d.Manifest != nil {
		loc = d.Manifest.Capabilities.Localization
	}
	localised := loc.Enabled != nil && *loc.Enabled

	var warn *output.Warning
	if locale != "" && locale != "all" {
		switch {
		case len(loc.Locales) > 0 && discovery.LocalesValidatable(loc.LocalesSource):
			if !containsString(loc.Locales, locale) {
				return output.Locale{}, nil, apierr.New(apierr.CodeInvalidOption,
					"%q is not a locale of this project", locale).
					WithDidYouMean(apierr.DidYouMean(locale, loc.Locales)...).
					WithHint("%s", "locales: "+strings.Join(loc.Locales, ", "))
			}
		case len(cfg.Locales) > 0:
			if !containsString(cfg.Locales, locale) {
				return output.Locale{}, nil, apierr.New(apierr.CodeInvalidOption,
					"%q is not one of the profile's configured locales", locale).
					WithDidYouMean(apierr.DidYouMean(locale, cfg.Locales)...).
					WithHint("%s", "locales: "+strings.Join(cfg.Locales, ", "))
			}
		default:
			warn = &output.Warning{
				Code:    discovery.LocaleWarnUnverified,
				Message: discovery.LocaleUnverifiedMessage,
				Paths:   []string{locale},
				Hint:    discovery.LocaleUnverifiedHint(cfg.Profile),
			}
		}
	}

	out := output.Locale{}
	if locale != "" {
		l := locale
		out.Requested = &l
	}
	switch {
	case fallback != "":
		f := fallback
		out.Fallback = &f
	case localised, locale != "" && loc.Enabled == nil:
		// §7.9a / conflict 35. Payload's default is fallback:true, so an
		// untranslated field comes back in the DEFAULT locale, byte-identical
		// to a real translation — which is how an agent read-modify-writes
		// English into `de` and believes it translated the page.
		//
		// The second case is the one that used to be missing. `Enabled` is fed
		// only by the GraphQL schema, so on a project with
		// graphQL:{disable:true} — an explicitly supported portability case —
		// it can stay nil however localised the project actually is, and
		// `--locale de` went out bare. UNKNOWN IS NOT OFF: fallback-locale is
		// accepted and ignored on a non-localised project, so covering the
		// unknown case costs one no-op query parameter, versus fabricated
		// content. Localisation known to be OFF still sends nothing, because
		// then there is genuinely nothing to fall back from.
		f := discovery.FallbackNone
		out.Fallback = &f
	}
	return out, warn, nil
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func envPage(p payload.Page, returned int, truncated bool) *output.Page {
	return &output.Page{
		Limit:       p.Limit,
		Page:        p.Page,
		TotalPages:  p.TotalPages,
		TotalDocs:   p.TotalDocs,
		Returned:    returned,
		HasNextPage: p.HasNextPage,
		HasPrevPage: p.HasPrevPage,
		NextPage:    p.NextPage,
		PrevPage:    p.PrevPage,
		Truncated:   truncated,
	}
}

// mixedStatusWarning is §9.6.2's enforcing warning: the bare read did not
// filter drafts out and the agent is about to treat them as published.
func mixedStatusWarning(slug string, docs []payload.Doc, drafts *bool, statusFiltered bool) *output.Warning {
	if statusFiltered || drafts == nil || !*drafts || len(docs) == 0 {
		return nil
	}
	n := 0
	for _, doc := range docs {
		status, ok := doc["_status"].(string)
		if !ok {
			continue
		}
		if status != "published" {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return &output.Warning{
		Code:    warnMixedStatus,
		Message: fmt.Sprintf("%d of %d returned documents are not published", n, len(docs)),
		Hint:    "pay find " + slug + " --published-only",
	}
}

// classifyFor builds the error-classification context for a request.
func classifyFor(t *collTarget, cfg *config.Resolved, sent map[string]bool) payload.ClassifyContext {
	ctx := payload.ClassifyContext{SentPaths: sent, IncludeRaw: true}
	if t != nil {
		ctx.Collection = t.Slug
		if up := t.flags().Upload; up != nil {
			ctx.UploadCollection = *up
		}
	}
	if cfg != nil {
		ctx.NoRedact = !cfg.Redact
	}
	return ctx
}

// ---------------------------------------------------------------------------
// pay find
// ---------------------------------------------------------------------------

func init() { Register(newFindCmd) }

func newFindCmd(rt *Runtime) *cobra.Command {
	f := &readFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "find <collection>",
		// §9.2 lists `ls` under both `pay find` and `pay collections`; cobra
		// cannot give one alias to two commands, and `pay collections` keeps
		// it because a bare `pay ls` needs no argument to mean something.
		// `pay list <collection>` is find's short form.
		Aliases: []string{"list"},
		Short:   "List documents in a collection (GET /api/{collection})",
		Long: `Query a collection with Payload's full filter surface.

` + draftTrapHelp + `

The filter, the sort field and the --select keys are validated against the
cached schema BEFORE the request, because Payload answers an unknown sort field
with HTTP 200 and unsorted rows, and an unknown --select key with a document
containing only "id". Both silently produce a confidently wrong answer.`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdFind, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runFind(ctx, cmd, d, f, args[0])
	})
	f.registerFilterFlags(cmd)
	f.registerSelectionFlags(cmd, 0)
	f.registerPagingFlags(cmd, 20)
	cmd.Flags().BoolVar(&f.countOnly, "count-only", false, "return only the match count")

	SetHelp(cmd, findHelp())
	return cmd
}

// findHelp is §10.5's model for `pay find`. Every command string below was run
// against a live Payload 3.x project before it was written down.
func findHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay find <collection> [--where 'PATH OP VALUE' ...] [--or 'PATH OP VALUE' ...]",
			"                      [--sort FIELD] [--select a,b] [--limit N] [--page N]",
			"                      [--all [--max N]] [--depth N] [--published-only|--draft-only|--draft]",
		},
		Collections: true,
		Where:       true,
		Sort:        true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
		},
		FlagInfo: map[string]FlagInfo{
			"where":      {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"or":         {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"sort":       {Grammar: "[-]FIELD[,[-]FIELD...]", Repeatable: true},
			"depth":      {Min: intPtr(0), Max: intPtr(10)},
			"limit":      {Min: intPtr(1), Note: "0 means UNLIMITED to Payload and is rejected here; use --all"},
			"select":     {Grammar: "field,field.sub", Repeatable: true},
			"populate":   {Grammar: "collection:field1,field2", Repeatable: true},
			"joins":      {Grammar: "field:limit=5,sort=-createdAt", Repeatable: true},
			"since":      {Grammar: "30d | 2026-01-01 | 2026-01-01T00:00:00Z"},
			"until":      {Grammar: "30d | 2026-01-01 | 2026-01-01T00:00:00Z"},
			"id":         {Repeatable: true, Note: "sugar for {\"id\":{\"in\":[…]}}"},
			"q":          {Note: "OR-contains across the collection's title-ish text fields"},
			"max":        {Min: intPtr(0), Note: "only with --all; 0 means no cap"},
			"count-only": {Note: "same request as `pay count`, returning data_kind count"},
		},
		Output: OutputSpec{
			Kind: output.KindDocList, Paginated: true,
			Skeleton: `[{"id":11,"title":"Home","_status":"published"}]  (.page carries limit/page/total_docs/truncated)`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "the cheapest possible look at a collection",
				Cmd: "pay find pages --limit 5 --select id,title,_status"},
			{Why: "live content only, newest first (a bare read returns drafts too)",
				Cmd: "pay find posts --published-only --sort -publishedAt --limit 10"},
			{Why: "substring filter; % _ and \\ are escaped for you",
				Cmd: "pay find crm-contacts --where 'name contains Ada' --select id,name,email"},
			{Why: "AND across --where, OR inside the --or group",
				Cmd: "pay find crm-contacts --where 'status eq lead' --or 'lifecycleStage eq lead' --or 'lifecycleStage eq customer' --limit 5"},
			{Why: "filter across a relationship with a dotted path",
				Cmd: "pay find crm-contacts --where 'company.name contains Atlas' --limit 3 --select id,name,company"},
			{Why: "every page into a file, envelope summary kept separate",
				Cmd: "pay find crm-contacts --all --max 5000 --output jsonl > contacts.jsonl 2> summary.json"},
			{Why: "just one field of every match, no envelope arithmetic",
				Cmd: "pay find pages --limit 3 --path '[].title'"},
			{Why: "how many match, without transferring them",
				Cmd: "pay find posts --count-only"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--limit 0` to mean \"no documents\".",
				Right: "0 means UNLIMITED to Payload and is rejected here (exit 5). Use `--limit 1`, or `--all --max N` to walk every page."},
			{Wrong: "Trusting a bare read to return only live content.",
				Right: "Never-published documents come back with _status \"draft\". Pass --published-only, or read _status on every document."},
			{Wrong: "Reconstructing the next page by hand from page.total_pages.",
				Right: "Run `next.cmd` verbatim; it already carries the filter, the page and the profile. Stop when page.truncated is false."},
			{Wrong: "Expecting --depth 0 to embed relationships.",
				Right: "0 returns bare ids. Use --depth 1, and --populate 'crm-companies:name' to embed only the fields you need."},
			{Wrong: "Passing a jq expression to --path.",
				Right: "--path has three forms only: .a.b, .a[0], .a[]. Pipe the whole envelope to jq for anything else."},
		},
		SeeAlso: []string{
			"pay count <collection>      # the same filter, just the number",
			"pay describe <collection> --queryable   # what can appear in --where",
			"pay get <collection> <id>   # one document by id",
			"pay explain --section gotchas",
		},
	}
}

func runFind(ctx context.Context, cmd *cobra.Command, d *Deps, f *readFlags, slug string) (*output.Envelope, error) {
	cfg := d.cfg()
	if !cmd.Flags().Changed("limit") && cfg.Limit > 0 {
		f.limit = cfg.Limit
	}
	if !cmd.Flags().Changed("depth") && cfg.Depth > 0 {
		f.depth = cfg.Depth
	}

	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	built, err := f.build(cmd, d, t)
	if err != nil {
		return nil, err
	}

	run := d.beginRun()
	opt := payload.WithClassify(classifyFor(t, cfg, nil))

	if f.countOnly {
		n, _, err := client.Count(ctx, t.Slug, built.Params, opt, payload.WithKnownRoute())
		if err != nil {
			return nil, err
		}
		env := output.New(safety.CmdFind, output.KindCount, n).
			WithTarget(t.envTarget(nil)).
			WithMeta(built.applyMeta(run.meta()))
		for _, w := range built.Warnings {
			env.AddWarning(w)
		}
		if n > 0 {
			// Same hand-off `pay count` emits, for the same reason: the agent
			// that counted with `find --count-only` needs a way to SEE those
			// documents, and the follow-up has to carry the same filter or
			// "69 leads" becomes a first page of all 104 contacts. Omitting it
			// here made the two counting paths behave differently for no
			// reason an agent could discover.
			nextCmd, args := f.reinvocation(d, t, built, reinvokeOpts{Limit: countHandoffLimit})
			env.WithNext(&output.Next{
				Reason: output.ReasonMorePages,
				Cmd:    nextCmd,
				Args:   args,
			})
		}
		return env, nil
	}

	var (
		docs      []payload.Doc
		page      payload.Page
		truncated bool
		raw       []byte
	)
	if f.all {
		max := f.max
		if max < 0 {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--max must be >= 0; got %d", max)
		}
		err = client.FindPages(ctx, t.Slug, built.Params, max, func(l *payload.ListResult) error {
			docs = append(docs, l.Docs...)
			page = l.Page
			if raw == nil {
				raw = l.Raw
			}
			return nil
		}, opt, payload.WithKnownRoute())
		if err != nil {
			return nil, err
		}
		truncated = max > 0 && len(docs) >= max && page.TotalDocs > len(docs)
		// --all reports the AGGREGATE, not the last response it happened to
		// stop on: page 1 with a prev_page of 5 is a contradiction an agent
		// cannot act on. has_next_page survives only when --max stopped the
		// walk early, which is exactly what truncated means.
		page.Page = 1
		page.HasPrevPage, page.PrevPage = false, nil
		if !truncated {
			page.HasNextPage, page.NextPage = false, nil
		} else {
			page.HasNextPage = true
		}
	} else {
		list, err := client.Find(ctx, t.Slug, built.Params, opt, payload.WithKnownRoute())
		if err != nil {
			return nil, err
		}
		docs, page, raw = list.Docs, list.Page, list.Raw
	}
	if docs == nil {
		docs = []payload.Doc{}
	}

	env := output.New(safety.CmdFind, output.KindDocList, docs).
		WithTarget(t.envTarget(nil)).
		WithPage(envPage(page, len(docs), truncated)).
		WithMeta(built.applyMeta(run.meta())).
		WithRawBody(raw, !cfg.Redact)
	for _, w := range built.Warnings {
		env.AddWarning(w)
	}
	statusFiltered := f.publishedOnly || f.draftOnly || built.Published
	if w := mixedStatusWarning(t.Slug, docs, t.flags().Drafts, statusFiltered); w != nil {
		env.AddWarning(*w)
	}
	if f.all && truncated {
		// --all stopped at --max with matches left over. Without a next[] the
		// agent has to notice page.truncated and reconstruct the command
		// itself, which is exactly the arithmetic §10.1 exists to remove — and
		// re-emitting only the slug would hand back a *wider* query that
		// returns exactly the promised document count out of the wrong rows.
		wider := reinvokeOpts{All: true, Max: page.TotalDocs}
		cmd, args := f.reinvocation(d, t, built, wider)
		args["returned"] = len(docs)
		stream := wider
		stream.Output = "jsonl"
		streamCmd, _ := f.reinvocation(d, t, built, stream)
		env.WithNext(&output.Next{
			Reason: output.ReasonMorePages,
			Cmd:    cmd,
			Args:   args,
			Alternatives: []output.Alternative{{
				Why: "stream every match without holding them in memory",
				Cmd: streamCmd,
			}},
		})
	}
	if page.HasNextPage && page.NextPage != nil && !f.all {
		cmd, args := f.reinvocation(d, t, built, reinvokeOpts{Page: *page.NextPage})
		allCmd, _ := f.reinvocation(d, t, built, reinvokeOpts{All: true, Max: page.TotalDocs})
		env.WithNext(&output.Next{
			Reason: output.ReasonMorePages,
			Cmd:    cmd,
			Args:   args,
			Alternatives: []output.Alternative{{
				Why: "collect every match in one call",
				Cmd: allCmd,
			}},
		})
	}
	return env, nil
}
