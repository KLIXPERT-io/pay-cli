package cli

import (
	"context"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

func init() { Register(newCollectionsCmd) }

// collectionKinds and capabilityNames are the closed sets §9.2's --kind and
// --capability flags accept.
var (
	collectionKinds = []string{"content", "upload", "auth", "internal"}
	capabilityNames = []string{"upload", "versions", "drafts", "auth", "trash", "folders", "duplicate", "orderable"}
)

// collectionRow is one entry of `pay collections` and of `pay explain`'s
// collections section. It is the smallest description that still answers "can I
// do X here" without a second call.
type collectionRow struct {
	Slug         string   `json:"slug"`
	Singular     string   `json:"singular"`
	Plural       string   `json:"plural"`
	Kind         string   `json:"kind"`
	Internal     bool     `json:"internal"`
	IDType       string   `json:"id_type"`
	Reachability string   `json:"reachability"`
	Ops          []string `json:"ops"`
	Features     []string `json:"features"`
	KeyFields    []string `json:"key_fields"`
	TitleField   *string  `json:"title_field"`
	FieldsCount  int      `json:"fields_count"`
	TotalDocs    *int     `json:"total_docs"`
	Publishable  bool     `json:"publishable"`
	DateFields   []string `json:"date_fields"`
}

// globalRow is one entry of the globals inventory.
type globalRow struct {
	Slug        string   `json:"slug"`
	Label       string   `json:"label"`
	Internal    bool     `json:"internal"`
	Ops         []string `json:"ops"`
	Features    []string `json:"features"`
	FieldsCount int      `json:"fields_count"`
}

func newCollectionRow(c *discovery.Collection) collectionRow {
	row := collectionRow{
		Slug:         c.Slug,
		Singular:     c.Labels.Singular,
		Plural:       c.Labels.Plural,
		Kind:         collectionKind(c),
		Internal:     c.Internal,
		IDType:       c.IDType,
		Reachability: c.Reachability,
		Ops:          collectionOpsFor(c),
		Features:     collectionFeaturesFor(c),
		TitleField:   c.TitleField,
		FieldsCount:  c.FieldsCount,
		TotalDocs:    c.Stats.TotalDocs,
		Publishable:  c.Publishable,
		DateFields:   orEmptyStrings(c.DateFields),
	}
	row.KeyFields = keyFieldsFor(c)
	return row
}

func newGlobalRow(g *discovery.Global) globalRow {
	ops := []string{"read"}
	if g.Permissions.Update {
		ops = append(ops, "update")
	}
	if g.Permissions.ReadVersions {
		ops = append(ops, "versions")
	}
	var features []string
	if g.Flags.Versions != nil && *g.Flags.Versions {
		features = append(features, "versions")
	}
	if g.Flags.Drafts != nil && *g.Flags.Drafts {
		features = append(features, "drafts")
	}
	return globalRow{
		Slug: g.Slug, Label: g.Labels.Singular, Internal: g.Internal,
		Ops: ops, Features: orEmptyStrings(features), FieldsCount: g.FieldsCount,
	}
}

// collectionOps lists the verbs this identity may actually use, so an agent
// never plans a write it is not permitted to make.
func collectionOpsFor(c *discovery.Collection) []string {
	var ops []string
	if c.Permissions.Read {
		ops = append(ops, "read")
	}
	if c.Permissions.Create {
		ops = append(ops, "create")
	}
	if c.Permissions.Update {
		ops = append(ops, "update")
	}
	if c.Permissions.Delete {
		ops = append(ops, "delete")
	}
	if c.Permissions.ReadVersions {
		ops = append(ops, "versions")
	}
	return orEmptyStrings(ops)
}

// collectionFeatures lists only the capabilities that are known-true. A nil
// flag is "never learned" (§7.6) and must not appear as a claim either way.
func collectionFeaturesFor(c *discovery.Collection) []string {
	var out []string
	add := func(name string, v *bool) {
		if v != nil && *v {
			out = append(out, name)
		}
	}
	add("upload", c.Flags.Upload)
	add("auth", c.Flags.Auth)
	add("api_key", c.Flags.UseAPIKey)
	add("versions", c.Flags.Versions)
	add("drafts", c.Flags.Drafts)
	add("trash", c.Flags.Trash)
	add("folders", c.Flags.Folders)
	add("duplicate", c.Flags.Duplicate)
	add("orderable", c.Flags.Orderable)
	return orEmptyStrings(out)
}

func hasCapability(c *discovery.Collection, name string) bool {
	flag := map[string]*bool{
		"upload": c.Flags.Upload, "auth": c.Flags.Auth, "versions": c.Flags.Versions,
		"drafts": c.Flags.Drafts, "trash": c.Flags.Trash, "folders": c.Flags.Folders,
		"duplicate": c.Flags.Duplicate, "orderable": c.Flags.Orderable,
	}[name]
	return flag != nil && *flag
}

// keyFields is the short list an agent should reach for first: the title field,
// then the conventional identity fields this collection actually has.
func keyFieldsFor(c *discovery.Collection) []string {
	seen := map[string]bool{}
	var out []string
	push := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	push("id")
	if c.TitleField != nil {
		push(*c.TitleField)
	}
	for _, f := range c.DateFields {
		push(f)
	}
	if c.Publishable {
		push("_status")
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return orEmptyStrings(out)
}

func orEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// matchesGrep is §18's plain-substring filter over the slug and both labels. It
// is deliberately not a regex: an agent-supplied regex is an error surface with
// no upside here.
func matchesGrep(pattern string, values ...string) bool {
	if pattern == "" {
		return true
	}
	needle := strings.ToLower(pattern)
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}

type collectionsFilter struct {
	kind           string
	capability     string
	grep           string
	writable       bool
	includeInterna bool
}

// filterCollections applies §9.2's flags and §18's ordering: non-internal
// before internal, then total_docs descending, ties alphabetical.
func filterCollections(m *discovery.Manifest, f collectionsFilter) []collectionRow {
	var rows []collectionRow
	for _, c := range m.Collections {
		kind := collectionKind(c)
		if f.kind != "" && kind != f.kind {
			continue
		}
		if c.Internal && !f.includeInterna && f.kind != "internal" {
			continue
		}
		if f.writable && (!c.Permissions.Create && !c.Permissions.Update) {
			continue
		}
		if f.capability != "" && !hasCapability(c, f.capability) {
			continue
		}
		if !matchesGrep(f.grep, c.Slug, c.Labels.Singular, c.Labels.Plural) {
			continue
		}
		rows = append(rows, newCollectionRow(c))
	}
	sortCollectionRows(rows)
	return rows
}

func sortCollectionRows(rows []collectionRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Internal != b.Internal {
			return !a.Internal
		}
		ad, bd := -1, -1
		if a.TotalDocs != nil {
			ad = *a.TotalDocs
		}
		if b.TotalDocs != nil {
			bd = *b.TotalDocs
		}
		if ad != bd {
			return ad > bd
		}
		return a.Slug < b.Slug
	})
}

func newCollectionsCmd(rt *Runtime) *cobra.Command {
	var f collectionsFilter

	cmd := &cobra.Command{
		Use:   "collections",
		Short: "List this project's collections with their ops, features and key fields.",
		// §9.2 gives `ls` to `pay collections` and `list, ls` to `pay find`.
		// One alias cannot belong to two commands; `collections` keeps `ls`
		// because it takes no argument, so a bare `pay ls` is meaningful.
		Aliases: []string{"ls"},
		GroupID: GroupDiscovery,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			// `pay ls pages` is the one plausible mistake this alias creates:
			// §9.2 also lists `ls` under `pay find`, and only one command can
			// own the name. Say where the documents are instead of just
			// refusing the argument.
			return apierr.New(apierr.CodeInvalidArgs,
				"%s takes no arguments, got %d", cmd.CommandPath(), len(args)).
				WithHint("usage: pay collections [--kind K] [--capability C] [--grep P]; to query documents use `pay find %s` (alias `pay list`)", args[0])
		},
		RunE: Handle(rt, "collections", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if err := oneOf("kind", f.kind, collectionKinds); err != nil {
				return nil, err
			}
			if err := oneOf("capability", f.capability, capabilityNames); err != nil {
				return nil, err
			}
			m, err := rt.Discovery(ctx)
			if err != nil {
				return nil, err
			}
			rows := filterCollections(m, f)
			data := map[string]any{
				"collections": rows,
				"count":       len(rows),
				"total":       len(m.Collections),
				"filters": map[string]any{
					"kind": f.kind, "capability": f.capability, "grep": f.grep,
					"writable": f.writable, "include_internal": f.includeInterna,
				},
			}
			env := output.New("collections", output.KindCapabilities, data)
			if len(rows) == 0 && len(m.Collections) > 0 {
				rt.Warnf("no_matches",
					"no collection matched; %d exist — drop --kind/--capability/--grep or add --include-internal", len(m.Collections))
			}
			return env, nil
		}),
	}
	cmd.Flags().StringVar(&f.kind, "kind", "", "content|upload|auth|internal")
	cmd.Flags().StringVar(&f.capability, "capability", "", strings.Join(capabilityNames, "|"))
	cmd.Flags().StringVar(&f.grep, "grep", "", "case-insensitive substring over the slug and both labels (not a regex)")
	cmd.Flags().BoolVar(&f.writable, "writable", false, "only collections this credential may create or update")
	cmd.Flags().BoolVar(&f.includeInterna, "include-internal", false, "include Payload's own payload-* collections")
	_ = cmd.RegisterFlagCompletionFunc("kind", CompleteEnum(collectionKinds...))
	_ = cmd.RegisterFlagCompletionFunc("capability", CompleteEnum(capabilityNames...))

	SetHelp(cmd, &Help{
		Synopsis:    []string{"pay collections [--kind content|upload|auth|internal] [--writable] [--include-internal] [--capability C] [--grep PATTERN]"},
		Collections: true,
		Long: "Ordering is specified, not incidental: non-internal collections first, then by\n" +
			"document count descending, ties alphabetical. Payload's own payload-* collections\n" +
			"are hidden unless you ask for them.",
		FlagInfo: map[string]FlagInfo{
			"kind":       {Values: collectionKinds},
			"capability": {Values: capabilityNames},
			"grep":       {Grammar: "plain case-insensitive substring; not a regex"},
		},
		Output:    OutputSpec{Kind: output.KindCapabilities, Skeleton: `{"collections":[{"slug":"pages","kind":"content","ops":["read","create","update","delete"],"features":["versions","drafts"],"key_fields":["id","title","updatedAt"]}],"count":18,"total":49}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "what content can I edit here?", Cmd: "pay collections"},
			{Why: "slugs only, for a loop", Cmd: "pay collections --path .collections[].slug"},
			{Why: "which collections take file uploads?", Cmd: "pay collections --capability upload"},
			{Why: "which ones can I write to?", Cmd: "pay collections --writable --path .collections[].slug"},
			{Why: "find the CRM ones on a large project", Cmd: "pay collections --grep crm"},
			{Why: "human-readable table", Cmd: "pay collections --path .collections[] --output table"},
		},
		Mistakes: []Mistake{
			{Wrong: "Assuming a missing feature means the collection lacks it.",
				Right: "`features` lists only known-true capabilities. A capability PayCLI never learned is simply absent; attempt the operation and read the server's answer."},
			{Wrong: "Looking for payload-jobs and concluding it does not exist.",
				Right: "Internal collections are hidden by default; add --include-internal or --kind internal."},
			{Wrong: "Using this to discover field names.",
				Right: "It lists only key fields. `pay describe <collection>` prints every field with its type and operators."},
			{Wrong: "Passing a regex to --grep.",
				Right: "It is a plain substring match. Filter with --path or jq for anything richer."},
		},
		SeeAlso: []string{"pay describe <collection>", "pay explain --section collections", "pay access"},
	})
	return cmd
}
