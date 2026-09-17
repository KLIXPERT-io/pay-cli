package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func init() { Register(newDescribeCmd) }

type describeFlags struct {
	field        string
	requiredOnly bool
	queryable    bool
	examples     bool
	sample       bool
}

func newDescribeCmd(rt *Runtime) *cobra.Command {
	f := &describeFlags{}

	cmd := &cobra.Command{
		Use:               "describe <collection|global>",
		Short:             "Print one entity's full field schema: types, operators, required, relations.",
		GroupID:           GroupDiscovery,
		Args:              exactArgs(1, "pay describe <collection|global> [--field PATH]"),
		ValidArgsFunction: CompleteEntities(rt),
		RunE: Handle(rt, "describe", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			slug := args[0]
			m, err := rt.Discovery(ctx)
			if err != nil {
				return nil, err
			}

			kind := cache.KindCollection
			var header map[string]any
			var targetKind string
			if coll, ok := m.Collection(slug); ok {
				targetKind = "collection"
				header = collectionHeader(coll)
			} else if glob, ok := m.Global(slug); ok {
				targetKind, kind = "global", cache.KindGlobal
				header = globalHeader(glob)
			} else {
				// Resolve produces the collection_unknown/global_unknown error
				// with did_you_mean from the manifest.
				if _, err := m.ResolveCollection(slug); err != nil {
					return nil, err
				}
				return nil, apierr.New(apierr.CodeCollectionUnknown, "unknown entity %q", slug)
			}

			shard, haveShard := rt.Shard(ctx, slug, kind)
			data := map[string]any{
				"kind":   targetKind,
				"entity": header,
			}
			if !haveShard {
				rt.Warnf(discovery.LimFieldsUnavailable,
					"no field schema is cached for %q; run `pay discover --refresh`", slug)
				data["fields"] = []discovery.Field{}
				data["fields_source"] = discovery.SourceUnknown
				env := output.New("describe", output.KindSchema, data)
				env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
				return env, nil
			}

			if f.field != "" {
				return describeOneField(rt, m, slug, targetKind, shard, f.field, header)
			}

			fields := filterFields(shard.Fields, f)
			data["fields"] = fields
			data["fields_count"] = len(fields)
			data["fields_total"] = len(shard.Fields)
			data["required_paths"] = orEmptyStrings(shard.RequiredPaths)
			data["join_fields"] = orEmptyStrings(shard.JoinFields)
			data["blocks"] = shard.Blocks
			data["blocks_source"] = shard.BlocksSource
			data["queryable_paths"] = orEmptyStrings(shard.QueryablePaths())
			data["sortable_paths"] = orEmptyStrings(shard.SortablePaths())
			data["date_fields"] = orEmptyStrings(shard.DateFields())

			if f.examples {
				data["examples"] = describeExamples(slug, targetKind, shard, m)
			}
			if f.sample {
				sample, err := rt.sampleDocument(ctx, slug, targetKind)
				if err != nil {
					rt.Warnf("sample_unavailable", "could not fetch a sample document: %s", err)
				} else {
					data["sample"] = sample
				}
			}
			env := output.New("describe", output.KindSchema, data)
			env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
			return env, nil
		}),
	}
	cmd.Flags().StringVar(&f.field, "field", "", "describe one field path in full (dotted, e.g. hero.links.link.url)")
	cmd.Flags().BoolVar(&f.requiredOnly, "required-only", false, "only fields whose required is known true")
	cmd.Flags().BoolVar(&f.queryable, "queryable", false, "only fields usable in --where")
	cmd.Flags().BoolVar(&f.examples, "examples", false, "add generated, copy-pasteable commands for this entity")
	cmd.Flags().BoolVar(&f.sample, "sample", false, "also fetch one real document (one extra request)")
	_ = cmd.RegisterFlagCompletionFunc("field", CompleteFieldPaths(rt, FieldSelectable))

	SetHelp(cmd, &Help{
		Synopsis:    []string{"pay describe <collection|global> [--field PATH] [--required-only] [--queryable] [--examples] [--sample]"},
		Collections: true,
		Globals:     true,
		Long: "Every field entry carries the same 26 keys, using null or the sentinels \"n/a\" /\n" +
			"\"unknown\" where a fact could not be learned, so `jq '.data.fields[] |\n" +
			"select(.required)'` is total and never needs a presence check.",
		Args: []ArgSpec{
			{Name: "entity", Required: true, Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"},
		},
		Output:    OutputSpec{Kind: output.KindSchema, Skeleton: `{"kind":"collection","entity":{…},"fields":[{"path":"title","payload_type":"text","required":true,"queryable":true,"operators":["equals","contains",…]}],"required_paths":["title"]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "everything about a collection", Cmd: "pay describe pages"},
			{Why: "what must I supply to create one?", Cmd: "pay describe pages --required-only --path .fields[].path"},
			{Why: "what can I filter on?", Cmd: "pay describe pages --queryable --path .queryable_paths[]"},
			{Why: "one field in full, including its operators", Cmd: "pay describe pages --field title"},
			{Why: "the blockTypes a blocks field accepts", Cmd: "pay describe pages --field layout"},
			{Why: "schema plus a real document", Cmd: "pay describe pages --sample"},
			{Why: "generated commands for this collection", Cmd: "pay describe pages --examples --path .examples[]"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading `required: null` as \"not required\".",
				Right: "null means PayCLI never learned it. Attempt the write; a 400 names the missing fields precisely."},
			{Wrong: "Using a join field in --select.",
				Right: "Join fields are read-only and are requested with --joins 'field:limit=5,sort=-createdAt'; .join_fields lists them."},
			{Wrong: "Assuming `sortable: true` was measured.",
				Right: "Sortability is heuristic unless sortable_confidence says otherwise; Payload silently ignores an unsortable field."},
			{Wrong: "Expecting `blocks` to be filled on every project.",
				Right: "Block slugs are not in the API; a null means no source resolved them. .blocks_source says which source did."},
		},
		SeeAlso: []string{"pay collections", "pay explain --collection <slug>", "pay find <collection> --select …"},
	})
	return cmd
}

func collectionHeader(c *discovery.Collection) map[string]any {
	return map[string]any{
		"slug": c.Slug, "singular": c.Labels.Singular, "plural": c.Labels.Plural,
		"labels_source": c.Labels.Source,
		"kind":          collectionKind(c), "internal": c.Internal,
		"id_type": c.IDType, "id_type_source": c.IDTypeSource,
		"reachability": c.Reachability,
		"ops":          collectionOpsFor(c), "features": collectionFeaturesFor(c),
		"publishable": c.Publishable, "publishable_reason": c.PublishableReason,
		"title_field": c.TitleField, "date_fields": orEmptyStrings(c.DateFields),
		"fields_count": c.FieldsCount, "fields_source": c.FieldsSource,
		"permissions": c.Permissions, "flags": c.Flags, "flags_source": c.FlagsSource,
		"graphql": c.GraphQL,
	}
}

func globalHeader(g *discovery.Global) map[string]any {
	return map[string]any{
		"slug": g.Slug, "singular": g.Labels.Singular, "plural": g.Labels.Plural,
		"internal": g.Internal, "reachability": g.Reachability,
		"ops": newGlobalRow(g).Ops, "features": newGlobalRow(g).Features,
		"fields_count": g.FieldsCount, "fields_source": g.FieldsSource,
		"permissions": g.Permissions, "flags": g.Flags, "flags_source": g.FlagsSource,
		"graphql": g.GraphQL,
	}
}

func filterFields(fields []discovery.Field, f *describeFlags) []discovery.Field {
	out := make([]discovery.Field, 0, len(fields))
	for _, fl := range fields {
		if f.requiredOnly && (fl.Required == nil || !*fl.Required) {
			continue
		}
		if f.queryable && !fl.Queryable {
			continue
		}
		out = append(out, fl)
	}
	return out
}

func describeOneField(rt *Runtime, m *discovery.Manifest, slug, targetKind string,
	shard *discovery.Shard, path string, header map[string]any) (*output.Envelope, error) {
	field, ok := shard.Field(path)
	if !ok {
		return nil, apierr.New(apierr.CodeUnknownField,
			"%q has no field %q", slug, path).
			WithHint("pay describe %s --path .fields[].path lists every field", slug).
			WithDidYouMean(apierr.DidYouMean(path, shard.Paths())...)
	}
	data := map[string]any{
		"kind":   targetKind,
		"entity": header,
		"field":  field,
	}
	if field.PayloadType == discovery.TypeBlocks {
		data["block_types"] = shard.Blocks[path]
		data["blocks_source"] = shard.BlocksSource
		if len(shard.Blocks[path]) == 0 {
			data["blocks_help"] = discovery.DescribeBlocksHelp(nil, rt.Cfg.Profile, path)
			rt.Warnf(discovery.LimBlockSlugsUnknown, "%s", discovery.UnresolvedBlocksReason(path))
		}
	}
	if len(field.Operators) > 0 {
		data["where_examples"] = fieldWhereExamples(slug, field)
	}
	env := output.New("describe", output.KindSchema, data)
	env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
	return env, nil
}

// fieldWhereExamples turns a field's operator list into runnable --where terms.
func fieldWhereExamples(slug string, f discovery.Field) []string {
	value := "VALUE"
	switch f.PayloadType {
	case discovery.TypeNumber:
		value = "10"
	case discovery.TypeCheckbox:
		value = "true"
	case discovery.TypeDate:
		value = "2026-01-01"
	case discovery.TypeSelect, discovery.TypeRadio:
		if len(f.Options) > 0 {
			value = f.Options[0]
		}
	}
	var out []string
	for _, op := range f.Operators {
		alias := op
		if canonical, ok := query.Canonical(op); ok {
			alias = canonical
		}
		out = append(out, fmt.Sprintf("pay find %s --where '%s %s %s'", slug, f.Path, alias, value))
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// describeExamples generates commands that use this entity's real fields.
func describeExamples(slug, kind string, shard *discovery.Shard, m *discovery.Manifest) []string {
	if kind == "global" {
		return []string{
			"pay globals get " + slug,
			fmt.Sprintf("pay globals get %s --select %s", slug, strings.Join(firstPaths(shard, 3), ",")),
			fmt.Sprintf("pay globals update %s --set %s=VALUE --dry-run", slug, firstWritable(shard)),
		}
	}
	sel := strings.Join(firstPaths(shard, 4), ",")
	sortField := "updatedAt"
	if dates := shard.DateFields(); len(dates) > 0 {
		sortField = dates[0]
	}
	queryable := "id"
	if qs := shard.QueryablePaths(); len(qs) > 0 {
		queryable = qs[0]
	}
	out := []string{
		fmt.Sprintf("pay find %s --limit 5 --select %s", slug, sel),
		fmt.Sprintf("pay find %s --where '%s contains TEXT' --sort -%s", slug, queryable, sortField),
		fmt.Sprintf("pay count %s --where '%s exists true'", slug, queryable),
	}
	if req := shard.RequiredPaths; len(req) > 0 {
		sets := make([]string, 0, len(req))
		for _, p := range req {
			sets = append(sets, "--set "+p+"=VALUE")
		}
		out = append(out, fmt.Sprintf("pay create %s %s --dry-run", slug, strings.Join(sets, " ")))
	} else {
		out = append(out, fmt.Sprintf("pay create %s --set %s=VALUE --dry-run", slug, firstWritable(shard)))
	}
	if c, ok := m.Collection(slug); ok && c.Flags.Drafts != nil && *c.Flags.Drafts {
		out = append(out, fmt.Sprintf("pay find %s --published-only --limit 5", slug))
	}
	return out
}

func firstPaths(shard *discovery.Shard, n int) []string {
	paths := shard.Paths()
	out := []string{"id"}
	for _, p := range paths {
		if p == "id" {
			continue
		}
		out = append(out, p)
		if len(out) >= n {
			break
		}
	}
	return out
}

func firstWritable(shard *discovery.Shard) string {
	for _, f := range shard.Fields {
		if f.ReadOnly || f.Path == "id" {
			continue
		}
		if f.PayloadType == discovery.TypeText || f.PayloadType == discovery.TypeTextarea {
			return f.Path
		}
	}
	for _, f := range shard.Fields {
		if !f.ReadOnly && f.Path != "id" {
			return f.Path
		}
	}
	return "FIELD"
}

// sampleDocument fetches one real document so an agent can see the shape the
// server actually returns. It is redacted like everything else.
func (rt *Runtime) sampleDocument(ctx context.Context, slug, kind string) (any, error) {
	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}
	if kind == "global" {
		doc, _, err := client.GlobalGet(ctx, slug, query.Params{})
		if err != nil {
			return nil, err
		}
		masked, _ := redact.Value(map[string]any(doc))
		return masked, nil
	}
	limit := 1
	res, err := client.Find(ctx, slug, query.Params{Limit: &limit})
	if err != nil {
		return nil, err
	}
	if len(res.Docs) == 0 {
		return nil, apierr.New(apierr.CodeDocNotFound, "%s is empty", slug)
	}
	masked, _ := redact.Value(map[string]any(res.Docs[0]))
	return masked, nil
}
