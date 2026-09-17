package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func init() { Register(newExplainCmd) }

// §18's tiers.
const (
	// DetailCap is how many per-collection detail entries the default tier
	// emits. The SLUG LIST IS NEVER TRUNCATED — data.collection_slugs always
	// holds every slug, because it is the single most load-bearing thing this
	// command returns.
	DetailCap = 40
	// DefaultMaxBytes is --max-bytes' default: above it, --full refuses.
	DefaultMaxBytes = 400 << 10
)

// explainSections is the closed set §9.2's --section flag accepts.
var explainSections = []string{
	"connection", "collections", "globals", "commands", "query_syntax", "exit_codes", "gotchas",
}

type explainFlags struct {
	slim       bool
	full       bool
	section    string
	collection string
	maxBytes   int
	offset     int
	limit      int
	grep       string
}

func newExplainCmd(rt *Runtime) *cobra.Command {
	f := &explainFlags{}

	cmd := &cobra.Command{
		Use:     "explain",
		Aliases: []string{"capabilities"},
		Short:   "Answer \"what can I do here?\" in one call — cache-backed and offline after the first run.",
		GroupID: GroupDiscovery,
		Args:    maxArgs(0, "pay explain [--slim|--full] [--section S] [--collection SLUG]"),
		RunE: Handle(rt, "explain", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if f.slim && f.full {
				return nil, apierr.New(apierr.CodeInvalidArgs, "--slim and --full are mutually exclusive")
			}
			if err := oneOf("section", f.section, explainSections); err != nil {
				return nil, err
			}
			if f.offset < 0 || f.limit < 0 {
				return nil, apierr.New(apierr.CodeInvalidArgs, "--offset and --limit must not be negative")
			}
			m, err := rt.Discovery(ctx)
			if err != nil {
				return nil, err
			}
			data, err := rt.buildExplain(ctx, m, f)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return nil, apierr.Wrap(err, apierr.CodeInternal, "could not encode the capability report")
			}
			// meta.bytes is set even on the refusal path (§18): the size is
			// exactly the fact the agent needs in order to decide what to ask
			// for instead.
			rt.SetDataBytes(int64(len(encoded)))
			if f.full && len(encoded) > f.maxBytes {
				return nil, apierr.New(apierr.CodeRequestTooLarge,
					"the full capability report is %d bytes, above --max-bytes %d", len(encoded), f.maxBytes).
					WithHint("use `pay explain` (the default tier) or `pay explain --collection SLUG`, or raise --max-bytes")
			}
			return output.New("explain", output.KindCapabilities, data), nil
		}),
	}
	cmd.Flags().BoolVar(&f.slim, "slim", false, "connection + every slug + exit codes only (< 1.5 KB)")
	cmd.Flags().BoolVar(&f.full, "full", false, "every field of every entity; refuses above --max-bytes")
	cmd.Flags().StringVar(&f.section, "section", "", strings.Join(explainSections, "|"))
	cmd.Flags().StringVar(&f.collection, "collection", "", "one entity, in full detail")
	cmd.Flags().IntVar(&f.maxBytes, "max-bytes", DefaultMaxBytes, "size ceiling for --full")
	cmd.Flags().IntVar(&f.offset, "offset", 0, "skip N entries of the selected section")
	cmd.Flags().IntVar(&f.limit, "limit", 0, "return at most M entries of the selected section (0 = tier default)")
	cmd.Flags().StringVar(&f.grep, "grep", "", "case-insensitive substring over the slug and both labels (not a regex)")
	_ = cmd.RegisterFlagCompletionFunc("section", CompleteEnum(explainSections...))
	_ = cmd.RegisterFlagCompletionFunc("collection", CompleteEntities(rt))

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay explain [--slim|--full] [--collection SLUG]",
			"pay explain --section connection|collections|globals|commands|query_syntax|exit_codes|gotchas [--offset N] [--limit M] [--grep PATTERN]",
		},
		Collections: true,
		Globals:     true,
		Long: "This is the first command to run on an unfamiliar project. It is size-tiered,\n" +
			"because a 400 KB capability dump is functionally the same as no capability dump.\n" +
			"data.collection_slugs is NEVER truncated: only the per-collection detail array is\n" +
			"capped, and the truncation warning carries the exact command that pages past it.",
		FlagInfo: map[string]FlagInfo{
			"section":   {Values: explainSections},
			"grep":      {Grammar: "plain case-insensitive substring; not a regex"},
			"max-bytes": {Min: intPtr(1024)},
		},
		Output:    OutputSpec{Kind: output.KindCapabilities, Skeleton: `{"connection":{…},"collection_slugs":[…all of them…],"collections":[…detail…],"globals":[…],"query_syntax":{…},"exit_codes":[…],"gotchas":[…]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "the whole picture, budgeted for a context window", Cmd: "pay explain"},
			{Why: "smallest useful answer: connection + every slug", Cmd: "pay explain --slim"},
			{Why: "page past the 40-entry detail cap", Cmd: "pay explain --section collections --offset 40"},
			{Why: "one entity in full", Cmd: "pay explain --collection pages"},
			{Why: "teach yourself the filter syntax", Cmd: "pay explain --section query_syntax"},
			{Why: "the whole exit-code table", Cmd: "pay explain --section exit_codes"},
			{Why: "the verified footguns for this Payload version", Cmd: "pay explain --section gotchas"},
			{Why: "find the CRM collections on a 49-collection project", Cmd: "pay explain --section collections --grep crm"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading `.collections` and concluding those are all the collections.",
				Right: "That array is capped at 40. `.collection_slugs` always holds every slug; page the detail with --section collections --offset N."},
			{Wrong: "Running `--full` on a large project and hitting exit 5.",
				Right: "That is the point — it refuses instead of flooding you. Use the default tier, or --collection SLUG for the one you need."},
			{Wrong: "Calling it before every command.",
				Right: "It is cached for 10 minutes and served offline; call it once per session, and again after a schema change with --refresh."},
			{Wrong: "Using --grep as a regex.",
				Right: "It is a plain substring over the slug and both labels. Use --path or jq for anything richer."},
		},
		SeeAlso: []string{"pay collections", "pay describe <collection>", "pay doctor", "pay discover --refresh"},
	})
	return cmd
}

func intPtr(n int) *int { return &n }

// buildExplain assembles the tiered report.
func (rt *Runtime) buildExplain(ctx context.Context, m *discovery.Manifest, f *explainFlags) (map[string]any, error) {
	if f.collection != "" {
		return rt.explainEntity(ctx, m, f.collection)
	}

	slugs := m.CollectionSlugs()
	globalSlugs := m.GlobalSlugs()

	// The slug lists and the connection block are present in every tier and
	// every section: they are what makes any other answer interpretable.
	data := map[string]any{
		"connection":       rt.connectionBlock(m),
		"collection_slugs": slugs,
		"global_slugs":     globalSlugs,
	}

	switch f.section {
	case "connection":
		return data, nil
	case "exit_codes":
		data["exit_codes"] = exitCodeTable(true)
		return data, nil
	case "query_syntax":
		data["query_syntax"] = querySyntaxBlock()
		return data, nil
	case "gotchas":
		data["gotchas"] = gotchas(m)
		return data, nil
	case "commands":
		data["commands"] = rt.commandTable(true)
		return data, nil
	case "globals":
		rows := make([]globalRow, 0, len(m.Globals))
		for _, g := range m.Globals {
			if matchesGrep(f.grep, g.Slug, g.Labels.Singular, g.Labels.Plural) {
				rows = append(rows, newGlobalRow(g))
			}
		}
		page, warn := pageRows(rows, f.offset, f.limit, len(rows), "globals")
		if warn != nil {
			rt.Warn(*warn)
		}
		data["globals"] = page
		return data, nil
	case "collections":
		rows := filterCollections(m, collectionsFilter{grep: f.grep, includeInterna: true})
		limit := f.limit
		if limit == 0 {
			limit = len(rows)
		}
		page, warn := pageRows(rows, f.offset, limit, len(m.Collections), "collections")
		if warn != nil {
			rt.Warn(*warn)
		}
		data["collections"] = page
		return data, nil
	}

	if f.slim {
		data["exit_codes"] = exitCodeShort()
		return data, nil
	}

	rows := filterCollections(m, collectionsFilter{grep: f.grep, includeInterna: true})
	detailCap := DetailCap
	if f.limit > 0 {
		detailCap = f.limit
	}
	if f.full {
		detailCap = len(rows)
	}
	shown := rows
	if len(shown) > detailCap {
		shown = shown[:detailCap]
		rt.Warn(output.Warning{
			Code: "collections_truncated",
			Message: fmt.Sprintf("detail shown for %d of %d collections; all %d slugs are in data.collection_slugs",
				detailCap, len(rows), len(m.CollectionSlugs())),
			Hint: fmt.Sprintf("pay explain --section collections --offset %d", detailCap),
		})
	}

	if f.full {
		detailed := make([]map[string]any, 0, len(shown))
		for _, row := range shown {
			entry := map[string]any{"collection": row}
			if shard, ok := rt.Shard(ctx, row.Slug, cache.KindCollection); ok {
				entry["fields"] = shard.Fields
				entry["required_paths"] = orEmptyStrings(shard.RequiredPaths)
				entry["blocks"] = shard.Blocks
			}
			detailed = append(detailed, entry)
		}
		data["collections"] = detailed
	} else {
		data["collections"] = compactRows(shown)
	}

	globals := make([]globalRow, 0, len(m.Globals))
	for _, g := range m.Globals {
		globals = append(globals, newGlobalRow(g))
	}
	data["globals"] = globals
	data["commands"] = rt.commandTable(false)
	data["query_syntax"] = querySyntaxBlock()
	data["exit_codes"] = exitCodeTable(false)
	data["gotchas"] = gotchas(m)
	// Declared holes are useful but open-ended; the default tier carries the
	// first few plus the total so the size stays bounded on a project with a
	// degraded discovery run.
	lim := m.Limitations
	if !f.full && len(lim) > limitationsCap {
		lim = lim[:limitationsCap]
	}
	data["limitations"] = lim
	data["limitations_total"] = len(m.Limitations)
	return data, nil
}

// explainEntity is the --collection tier: one entity, every field.
func (rt *Runtime) explainEntity(ctx context.Context, m *discovery.Manifest, slug string) (map[string]any, error) {
	if c, ok := m.Collection(slug); ok {
		data := map[string]any{
			"connection": rt.connectionBlock(m),
			"collection": newCollectionRow(c),
			"entity":     collectionHeader(c),
			"gotchas":    gotchas(m),
		}
		if shard, ok := rt.Shard(ctx, slug, cache.KindCollection); ok {
			data["fields"] = shard.Fields
			data["required_paths"] = orEmptyStrings(shard.RequiredPaths)
			data["queryable_paths"] = orEmptyStrings(shard.QueryablePaths())
			data["sortable_paths"] = orEmptyStrings(shard.SortablePaths())
			data["join_fields"] = orEmptyStrings(shard.JoinFields)
			data["blocks"] = shard.Blocks
			// The slugs alone say which blocks may go in a field, never what is
			// inside one. The interiors are named here and printed by
			// `pay describe <c> --block <slug>`; inlining them would multiply
			// the size of a command that is already the biggest one PayCLI has.
			addBlockSchemas(rt, data, shard, shard.BlockSlugsFor(), false, slug)
			data["examples"] = describeExamples(slug, "collection", shard, m)
		}
		return data, nil
	}
	if g, ok := m.Global(slug); ok {
		data := map[string]any{
			"connection": rt.connectionBlock(m),
			"global":     newGlobalRow(g),
			"entity":     globalHeader(g),
			"gotchas":    gotchas(m),
		}
		if shard, ok := rt.Shard(ctx, slug, cache.KindGlobal); ok {
			data["fields"] = shard.Fields
			data["required_paths"] = orEmptyStrings(shard.RequiredPaths)
			data["examples"] = describeExamples(slug, "global", shard, m)
		}
		return data, nil
	}
	if _, err := m.ResolveCollection(slug); err != nil {
		return nil, err
	}
	return nil, apierr.New(apierr.CodeCollectionUnknown, "unknown entity %q", slug)
}

// pageRows applies --offset/--limit and returns the truncation warning §18
// requires: one that names a command that actually pages.
func pageRows[T any](rows []T, offset, limit, total int, section string) ([]T, *output.Warning) {
	if offset > len(rows) {
		offset = len(rows)
	}
	page := rows[offset:]
	truncated := false
	if limit > 0 && len(page) > limit {
		page = page[:limit]
		truncated = true
	}
	if !truncated && offset+len(page) >= len(rows) {
		return page, nil
	}
	return page, &output.Warning{
		Code: section + "_truncated",
		Message: fmt.Sprintf("detail shown for %d of %d %s; all %d slugs are in data.%s_slugs",
			len(page), len(rows), section, total, strings.TrimSuffix(section, "s")),
		Hint: fmt.Sprintf("pay explain --section %s --offset %d", section, offset+len(page)),
	}
}

// connectionBlock is the "where am I and who am I" header every tier carries.
func (rt *Runtime) connectionBlock(m *discovery.Manifest) map[string]any {
	block := map[string]any{
		"profile":                rt.Cfg.Profile,
		"base_url":               rt.Cfg.BaseURL,
		"api_path":               m.Source.APIPath,
		"api_path_source":        m.Source.APIPathSource,
		"graphql_path":           m.Source.GraphQLPath,
		"graphql_mode":           m.Capabilities.GraphQL.Mode,
		"payload_version":        m.Source.PayloadVersion,
		"payload_version_source": m.Source.PayloadVersionSource,
		"db_adapter":             m.Source.DBAdapter,
		"db_adapter_source":      m.Source.DBAdapterSource,
		"cli_version":            buildinfo.Version(),
		"discovery_revision":     m.Revision(),
		"generated_at":           m.GeneratedAt,
		"auth_mode":              m.Identity.AuthMode,
		"auth_collection":        m.Identity.AuthCollection,
		"identity_verified":      m.Identity.Verified,
		"can_access_admin":       m.Identity.CanAccessAdmin,
		"collections_total":      len(m.Collections),
		"globals_total":          len(m.Globals),
		"localization":           m.Capabilities.Localization,
		"custom_endpoints":       orEmptyStrings(m.Capabilities.CustomEndpoints),
	}
	return block
}

// compactRow is the per-collection detail §18's default tier asks for: ops,
// features and key fields, and nothing else. The full row (permissions,
// provenance, reachability) belongs to `pay collections` and
// `pay explain --collection SLUG`, which are not size-tiered.
type compactRow struct {
	Slug      string   `json:"slug"`
	Label     string   `json:"label"`
	Kind      string   `json:"kind"`
	Internal  bool     `json:"internal,omitempty"`
	IDType    string   `json:"id_type"`
	Ops       []string `json:"ops"`
	Features  []string `json:"features"`
	KeyFields []string `json:"key_fields"`
	TotalDocs *int     `json:"total_docs,omitempty"`
}

func compactRows(rows []collectionRow) []compactRow {
	out := make([]compactRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, compactRow{
			Slug: r.Slug, Label: r.Plural, Kind: r.Kind, Internal: r.Internal,
			IDType: r.IDType, Ops: r.Ops, Features: r.Features,
			KeyFields: r.KeyFields, TotalDocs: r.TotalDocs,
		})
	}
	return out
}

// limitationsCap bounds the declared-holes array in the size-tiered output.
const limitationsCap = 8

// exitCodeShort is the smallest useful exit-code answer: the number and what it
// means. §18 targets under 1.5 KB for --slim, and 70 error codes with hints do
// not fit in that budget at any indentation — the names live one
// `pay explain --section exit_codes` away.
func exitCodeShort() []map[string]any {
	byExit := apierr.CodesByExit()
	exits := make([]int, 0, len(byExit))
	for exit := range byExit {
		exits = append(exits, exit)
	}
	sort.Ints(exits)
	out := []map[string]any{{"exit": apierr.ExitOK, "meaning": exitCodeMeaning(apierr.ExitOK)}}
	for _, exit := range exits {
		out = append(out, map[string]any{
			"exit": exit, "meaning": exitCodeMeaning(exit), "codes_total": len(byExit[exit]),
		})
	}
	return out
}

// exitCodeTable is the §11 map, rendered from apierr so it can never drift from
// the codes the binary actually emits.
//
// `full` controls the per-code detail. The size-tiered callers (§18) get the
// exit number, its meaning and the bare code names — about 2 KB — while
// `pay explain --section exit_codes` gets retriability and the default hint for
// every code, which is roughly five times larger and is what the agent asks for
// once it has an actual failure in hand.
func exitCodeTable(full bool) []map[string]any {
	byExit := apierr.CodesByExit()
	exits := make([]int, 0, len(byExit))
	for exit := range byExit {
		exits = append(exits, exit)
	}
	sort.Ints(exits)
	out := make([]map[string]any, 0, len(exits)+1)
	out = append(out, map[string]any{
		"exit": apierr.ExitOK, "meaning": exitCodeMeaning(apierr.ExitOK), "codes": []string{},
	})
	for _, exit := range exits {
		row := map[string]any{"exit": exit, "meaning": exitCodeMeaning(exit)}
		if full {
			codes := make([]map[string]any, 0, len(byExit[exit]))
			for _, c := range byExit[exit] {
				codes = append(codes, map[string]any{
					"code": c.String(), "retriable": c.Retriable(), "hint": apierr.DefaultHint(c),
				})
			}
			row["codes"] = codes
		} else {
			names := make([]string, 0, len(byExit[exit]))
			for _, c := range byExit[exit] {
				names = append(names, c.String())
			}
			row["codes"] = names
		}
		out = append(out, row)
	}
	return out
}

// querySyntaxBlock teaches the filter DSL inside the payload, so an agent that
// never read the help text still gets it.
func querySyntaxBlock() map[string]any {
	ops := make([]map[string]any, 0, len(whereOperators))
	for _, row := range whereOperators {
		aliases := strings.Split(row[0], ", ")
		ops = append(ops, map[string]any{
			"aliases": aliases, "operator": row[1], "value": row[2],
		})
	}
	return map[string]any{
		"grammar": "--where 'PATH OP VALUE' — split into at most three whitespace-separated tokens; the value is the rest of the line and needs no quoting.",
		"combining": map[string]string{
			"--where":      "repeatable; terms are ANDed",
			"--or":         "repeatable; forms ONE or-group which is ANDed with the --where terms",
			"--where-json": "replaces both",
			"--where-raw":  "appends a literal query string",
		},
		"operators":         ops,
		"payload_operators": query.Operators,
		"value_typing": []string{
			"null -> JSON null",
			"true/false -> bool",
			"an integer or float literal -> number",
			"anything else -> string",
			`force a string with quotes: --where 'code eq "123"'`,
			"force raw JSON with the json: prefix: --where 'tags in json:[1,2]'",
		},
		"deviations": []string{
			"contains escapes % _ and \\ — without it, contains=% matches every row (verified)",
			"nlike auto-wraps the value in %…% — without it, not_like=hopper matches every row (verified)",
		},
		"sugar": map[string]string{
			"--id / --ids":      `{"id":{"in":[…]}}`,
			"--draft-only":      `{"_status":{"equals":"draft"}}`,
			"--published-only":  `{"_status":{"equals":"published"}}`,
			"--q TEXT":          "an OR of contains across up to 12 text-ish fields; meta.searched_fields reports which",
			"--since / --until": "resolved client-side to an absolute instant against meta.date_field",
		},
		"since_forms": []string{
			"N[h|d|w|mo] — 12h, 30d, 4w, 1mo (1mo is calendar-aware)",
			"a Go duration — 720h, 90m, 36h30m",
			"YYYY-MM-DD — that date at 00:00:00Z",
			"full RFC 3339 — 2026-08-01T09:30:00Z",
		},
		"encoding": "where and data are sent as URL-encoded JSON strings; everything else uses Payload's bracket notation (sort=-createdAt, select[title]=true).",
	}
}

// gotchas travels in the payload, not only in the skill file, so the warnings
// reach agents that never installed the skill (§18). Every entry is a fact
// verified against a live Payload 3.x instance.
func gotchas(m *discovery.Manifest) []map[string]string {
	out := []map[string]string{
		{
			"id":   "auth_200_with_null_user",
			"text": "A wrong API key returns HTTP 200 with {\"user\":null}. Never read a status code as proof of auth.",
			"do":   "pay auth test",
		},
		{
			"id":   "anonymous_reads_succeed",
			"text": "Many projects allow unauthenticated reads, so a successful `pay find` proves nothing about your credential.",
			"do":   "pay whoami",
		},
		{
			"id":   "unknown_sort_is_silent",
			"text": "Payload silently ignores an unknown --sort field and returns 200, unsorted.",
			"do":   "pay describe <collection> --path .sortable_paths[]",
		},
		{
			"id":   "unknown_select_returns_only_id",
			"text": "An unknown --select key returns 200 with only id, not an error.",
			"do":   "pay describe <collection> --path .fields[].path",
		},
		{
			"id":   "limit_zero_is_unlimited",
			"text": "limit=0 means UNLIMITED in Payload, not \"no documents\". PayCLI rejects --limit 0 with exit 5.",
			"do":   "pay count <collection>",
		},
		{
			"id":   "drafts_need_published_only",
			"text": "A bare read does NOT exclude never-published documents; --draft is not the inverse of published.",
			"do":   "pay find <collection> --published-only",
		},
		{
			"id":   "bulk_writes_ignore_limit",
			"text": "A bulk PATCH/DELETE with a where clause ignores limit entirely; the blast radius is everything that matches.",
			"do":   "pay delete <collection> --where '…' --dry-run",
		},
		{
			"id":   "auth_header_carries_the_slug",
			"text": "The API-key header is `Authorization: {authCollectionSlug} API-Key {key}` — the slug is required and project-specific.",
			"do":   "pay auth status --path .auth_collection --output id",
		},
	}
	if m.Capabilities.Localization.Enabled != nil && *m.Capabilities.Localization.Enabled {
		out = append(out, map[string]string{
			"id":   "unknown_locale_is_silent",
			"text": "An unknown --locale returns 200 with the DEFAULT locale rather than an error.",
			"do":   "pay explain --path .connection.localization.locales[]",
		})
	}
	if m.Source.DBAdapter == discovery.DBPostgres {
		out = append(out, map[string]string{
			"id":   "postgres_operator_500s",
			"text": "On Postgres the all/near/within/intersects operators return HTTP 500 rather than a validation error.",
			"do":   "pay find <collection> --where 'tags in a,b'",
		})
	}
	return out
}

// commandTable is the one-line map of the tool, generated from the live tree so
// it cannot list a command this binary does not have.
func (rt *Runtime) commandTable(full bool) []map[string]any {
	root := rt.cobraCmd
	if root == nil {
		return []map[string]any{}
	}
	root = root.Root()
	var walk func(cmd *cobra.Command, prefix string) []map[string]any
	walk = func(cmd *cobra.Command, prefix string) []map[string]any {
		var out []map[string]any
		for _, sub := range cmd.Commands() {
			if !sub.IsAvailableCommand() {
				continue
			}
			name := strings.TrimSpace(prefix + " " + sub.Name())
			entry := map[string]any{
				"command": name,
				"short":   sub.Short,
				"group":   sub.GroupID,
			}
			if len(sub.Aliases) > 0 {
				entry["aliases"] = sub.Aliases
			}
			if h := HelpOf(sub); full && len(h.Synopsis) > 0 {
				entry["synopsis"] = h.Synopsis
			}
			out = append(out, entry)
			out = append(out, walk(sub, name)...)
		}
		return out
	}
	var rows []map[string]any
	if full {
		rows = walk(root, "")
	} else {
		// The size-tiered answer lists the top level only; a subcommand tree is
		// one `pay explain --section commands` (or one --help) away.
		rows = walkTop(root)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := rows[i]["command"].(string)
		b, _ := rows[j]["command"].(string)
		return a < b
	})
	return rows
}

func walkTop(root *cobra.Command) []map[string]any {
	var out []map[string]any
	for _, sub := range root.Commands() {
		if !sub.IsAvailableCommand() {
			continue
		}
		entry := map[string]any{"command": sub.Name(), "short": sub.Short}
		if names := subcommandNames(sub); len(names) > 0 {
			entry["subcommands"] = names
		}
		out = append(out, entry)
	}
	return out
}

func subcommandNames(cmd *cobra.Command) []string {
	var out []string
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			out = append(out, sub.Name())
		}
	}
	return out
}
