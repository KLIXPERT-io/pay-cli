package cli

import (
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// CompletionBudget is §9.9's hard limit. A shell that hangs on TAB is worse
// than one that suggests nothing, so every completion function checks the
// budget before and after the one cache read it is allowed to make.
const CompletionBudget = 100 * time.Millisecond

// noSuggestions is the answer to every cold cache, every error and every
// timeout: an empty list plus NoFileComp, so the shell does not fall back to
// offering filenames for a collection slug.
func noSuggestions() ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completionSetup arms the runtime for a completion call. Cobra reaches
// __complete without running PersistentPreRunE, and completion must never
// write a log line into the shell's completion buffer, so the logger is
// silenced for the duration.
func (rt *Runtime) completionSetup(cmd *cobra.Command) bool {
	rt.ensureSetup(cmd)
	rt.Log = logging.Discard()
	return rt.Cfg != nil
}

func (rt *Runtime) overBudget(started time.Time) bool {
	return rt.Now().Sub(started) > CompletionBudget
}

// CompleteCollections completes a collection slug from the cached index.
func CompleteCollections(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return noSuggestions()
		}
		return rt.completeSlugs(cmd, toComplete, true, false)
	}
}

// CompleteGlobals completes a global slug from the cached index.
func CompleteGlobals(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return noSuggestions()
		}
		return rt.completeSlugs(cmd, toComplete, false, true)
	}
}

// CompleteEntities completes a collection OR global slug, for `pay describe`.
func CompleteEntities(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return noSuggestions()
		}
		return rt.completeSlugs(cmd, toComplete, true, true)
	}
}

func (rt *Runtime) completeSlugs(cmd *cobra.Command, prefix string, collections, globals bool) ([]string, cobra.ShellCompDirective) {
	started := rt.Now()
	if !rt.completionSetup(cmd) {
		return noSuggestions()
	}
	m, ok := rt.CachedManifest()
	if !ok || rt.overBudget(started) {
		return noSuggestions()
	}
	var out []string
	if collections {
		for _, c := range m.Collections {
			if strings.HasPrefix(c.Slug, prefix) {
				out = append(out, c.Slug+"\t"+c.Labels.Plural)
			}
		}
	}
	if globals {
		for _, g := range m.Globals {
			if strings.HasPrefix(g.Slug, prefix) {
				out = append(out, g.Slug+"\tglobal: "+g.Labels.Singular)
			}
		}
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// FieldMode selects which field-path projection a completion offers.
type FieldMode string

const (
	// FieldQueryable completes --where / --or paths.
	FieldQueryable FieldMode = "queryable"
	// FieldSortable completes --sort.
	FieldSortable FieldMode = "sortable"
	// FieldSelectable completes --select / --select-exclude.
	FieldSelectable FieldMode = "selectable"
	// FieldDate completes --date-field.
	FieldDate FieldMode = "date"
)

// CompleteFieldPaths completes a field path for the collection already on the
// command line. It decodes at most the single shard for that collection (§9.9);
// a missing or generation-mismatched shard yields nothing.
func CompleteFieldPaths(rt *Runtime, mode FieldMode) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		started := rt.Now()
		if !rt.completionSetup(cmd) || len(args) == 0 {
			return noSuggestions()
		}
		slug := args[0]
		m, ok := rt.CachedManifest()
		if !ok {
			return noSuggestions()
		}
		kind := cache.KindCollection
		if _, isColl := m.Collection(slug); !isColl {
			if _, isGlobal := m.Global(slug); !isGlobal {
				return noSuggestions()
			}
			kind = cache.KindGlobal
		}
		shard, ok := rt.CachedShard(slug, kind)
		if !ok || rt.overBudget(started) {
			return noSuggestions()
		}

		var paths []string
		switch mode {
		case FieldSortable:
			paths = shard.SortablePaths()
		case FieldSelectable:
			paths = shard.Paths()
		case FieldDate:
			paths = shard.DateFields()
		default:
			paths = shard.QueryablePaths()
		}

		// --sort accepts a leading '-' for descending; complete both forms so
		// TAB after `--sort -` still works.
		desc := mode == FieldSortable && strings.HasPrefix(toComplete, "-")
		trimmed := strings.TrimPrefix(toComplete, "-")

		out := make([]string, 0, len(paths))
		for _, p := range paths {
			if !strings.HasPrefix(p, trimmed) {
				continue
			}
			if desc {
				out = append(out, "-"+p)
			} else {
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteProfiles completes --profile from the two config files. It reads no
// credentials and never touches the keychain.
// CompleteBlockSlugs completes `pay describe <entity> --block <slug>` from the
// cached shard's block schemas.
//
// It suggests the slugs, never the GraphQL interfaceNames: an interfaceName is
// exactly the value Payload silently drops (§7.10), so offering one on TAB
// would be a suggestion that produces a 201 and no content.
func CompleteBlockSlugs(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		started := rt.Now()
		if !rt.completionSetup(cmd) || len(args) == 0 {
			return noSuggestions()
		}
		slug := args[0]
		m, ok := rt.CachedManifest()
		if !ok {
			return noSuggestions()
		}
		kind := cache.KindCollection
		if _, isColl := m.Collection(slug); !isColl {
			if _, isGlobal := m.Global(slug); !isGlobal {
				return noSuggestions()
			}
			kind = cache.KindGlobal
		}
		shard, ok := rt.CachedShard(slug, kind)
		if !ok || rt.overBudget(started) {
			return noSuggestions()
		}
		out := []string{}
		for _, b := range shard.BlockSlugsFor() {
			if strings.HasPrefix(b, toComplete) {
				out = append(out, b)
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

func CompleteProfiles(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if !rt.completionSetup(cmd) {
			return noSuggestions()
		}
		var out []string
		for _, name := range config.ProfileNames(rt.ProjectFile, rt.UserFile) {
			if strings.HasPrefix(name, toComplete) {
				out = append(out, name)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteEnum completes a fixed value set, for --output and friends.
func CompleteEnum(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var out []string
		for _, v := range values {
			if strings.HasPrefix(v, toComplete) {
				out = append(out, v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// registerRootCompletions wires the root flags that have a closed or
// discoverable value set.
func registerRootCompletions(rt *Runtime, cmd *cobra.Command) {
	formats := make([]string, 0, len(output.Formats))
	for _, f := range output.Formats {
		formats = append(formats, string(f))
	}
	_ = cmd.RegisterFlagCompletionFunc("output", CompleteEnum(formats...))
	_ = cmd.RegisterFlagCompletionFunc("errors-to", CompleteEnum(string(output.ErrorsToStdout), string(output.ErrorsToStderr)))
	_ = cmd.RegisterFlagCompletionFunc("auth-mode", CompleteEnum(
		string(config.AuthModeAuto), string(config.AuthModeAPIKey),
		string(config.AuthModeJWT), string(config.AuthModeAnonymous)))
	_ = cmd.RegisterFlagCompletionFunc("auth-header-scheme", CompleteEnum("JWT", "Bearer"))
	_ = cmd.RegisterFlagCompletionFunc("log-level", CompleteEnum(logging.Levels...))
	_ = cmd.RegisterFlagCompletionFunc("log-format", CompleteEnum(logging.Formats...))
	_ = cmd.RegisterFlagCompletionFunc("profile", CompleteProfiles(rt))
	_ = cmd.RegisterFlagCompletionFunc("auth-collection", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if !rt.completionSetup(c) {
			return noSuggestions()
		}
		m, ok := rt.CachedManifest()
		if !ok {
			return noSuggestions()
		}
		var out []string
		for _, coll := range m.Collections {
			if coll.Flags.Auth != nil && *coll.Flags.Auth && strings.HasPrefix(coll.Slug, toComplete) {
				out = append(out, coll.Slug)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	})
}
