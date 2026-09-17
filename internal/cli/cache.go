package cli

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

func init() { Register(newCacheCmd) }

// newCacheCmd builds `pay cache` (§8) — the operator's window onto the
// discovery cache.
//
// §4.1 promises that `rm -rf $(pay cache path)` never breaks the CLI, only
// slows it down. Every subcommand here is built so that promise stays true: a
// missing, unreadable or foreign-epoch cache produces rows and warnings, never
// an error.
func newCacheCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cache",
		GroupID: GroupAdmin,
		Short:   "Inspect and manage the discovery cache",
		Long: `The discovery cache stores what PayCLI learned about a project: the
collection and global inventory, per-entity field schemas and the permission
map. It is keyed by base URL + api_path + header names + credential
fingerprint, so two profiles pointing at the same server with different keys
never see each other's view of what is readable.

Deleting the cache is always safe. The next command simply re-runs discovery.`,
	}
	cmd.AddCommand(
		newCacheInfoCmd(rt),
		newCacheLsCmd(rt),
		newCachePathCmd(rt),
		newCacheShowCmd(rt),
		newCacheClearCmd(rt),
		newCacheWarmCmd(rt),
	)
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache info|ls|show|path|clear|warm"},
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "is my view of this project fresh?", Cmd: "pay cache info"},
			{Why: "every scope this machine has cached", Cmd: "pay cache ls"},
			{Why: "re-discover now, before a batch of work", Cmd: "pay cache warm"},
			{Why: "the schema looks wrong after a deploy", Cmd: "pay cache clear"},
			{Why: "start completely fresh, every profile", Cmd: "pay cache clear --all"},
		},
		Mistakes: []Mistake{
			{Wrong: "Worrying that clearing the cache loses data.",
				Right: "The cache holds only what PayCLI learned ABOUT the project — no documents. Clearing it is always safe; the next command re-runs discovery."},
			{Wrong: "Expecting one profile's cache to serve another with a different key.",
				Right: "The scope key is base URL + api_path + header names + credential fingerprint, precisely so two credentials never share a view of what is readable. Two profiles on the same server are two scopes."},
			{Wrong: "Deleting the cache directory by hand to fix a stale schema.",
				Right: "`pay cache clear` (or `pay discover --refresh`) does it without racing a command that is mid-write."},
			{Wrong: "Reading a cache miss as an error.",
				Right: "A missing, unreadable or foreign-epoch cache produces rows and warnings, never a failure. PayCLI re-discovers and carries on."},
		},
		SeeAlso: []string{
			"pay discover --refresh   # re-run discovery for this profile",
			"pay explain   # what PayCLI currently believes about the project",
			"pay config paths   # where the cache directory is",
		},
	})
	return cmd
}

// jsonValue re-renders a typed value as the generic map/slice/scalar tree that
// --path (§10.3) can walk.
//
// internal/output normalises typed data on its own now (ApplyPath and every
// renderer round-trip it), so this is no longer load-bearing for --path. It is
// kept because it fixes the shape AT CONSTRUCTION: a []ScopeInfo becomes the
// same sorted-key tree that is printed, so the value these commands hand to
// warnings, hints and the envelope is byte-identical to the one an agent reads
// back. Numbers decode through json.Number, so a large id is not degraded to
// float64.
func jsonValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return out
}

// ---------------------------------------------------------------------------
// pay cache path

// ---------------------------------------------------------------------------

type cachePathData struct {
	Path      string `json:"path"`
	EpochDir  string `json:"epoch_dir"`
	Scope     string `json:"scope,omitempty"`
	ScopeDir  string `json:"scope_dir,omitempty"`
	Enabled   bool   `json:"enabled"`
	ScopeNote string `json:"scope_note,omitempty"`
}

func newCachePathCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cache directory",
		Long: `Print the cache directory. --output raw prints the bare path and nothing
else, so it composes:

  rm -rf "$(pay cache path --output raw)"`,
		Args: maxArgs(0, "pay cache path"),
		RunE: Handle(rt, "cache path", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			store := rt.Cache()
			data := cachePathData{
				Path:     rt.Paths.CacheDir,
				EpochDir: store.EpochDir(),
				Enabled:  store.Enabled(),
			}
			if !store.Enabled() {
				data.Path = rt.Paths.CacheDir
				data.EpochDir = ""
				data.ScopeNote = "the cache is disabled for this invocation (--no-cache)"
			}
			if sc, err := rt.Scope(); err == nil && sc.Valid() {
				data.Scope = sc.Key
				if store.Enabled() {
					data.ScopeDir = store.ScopeDir(sc)
				}
			} else if err != nil {
				data.ScopeNote = "no scope: " + err.Error()
			}
			return output.New("cache path", output.KindOpResult, jsonValue(data)).
				WithRawBody([]byte(data.Path+"\n"), true), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache path"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"path":"/home/u/.cache/pay","epoch_dir":"…/v1","scope":"…","scope_dir":"…","enabled":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal},
		Examples: []Example{
			{Why: "where the cache lives", Cmd: "pay cache path"},
			{Why: "the bare path, for a shell", Cmd: "pay cache path --output raw"},
			{Why: "how big has it grown?", Cmd: "du -sh \"$(pay cache path --output raw)\""},
			{Why: "this profile's scope directory", Cmd: "pay cache path --path .scope_dir --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Hardcoding ~/.cache/pay.",
				Right: "PAY_HOME, XDG_CACHE_HOME and the Windows layout all move it. Read it from this command."},
			{Wrong: "rm -rf on the directory while a command is running.",
				Right: "`pay cache clear` takes the lock; a manual delete can race a concurrent write and leave a half-written scope."},
			{Wrong: "Expecting the path to differ per profile.",
				Right: "One cache DIRECTORY holds every scope; the per-profile part is the scope key beneath it. `pay cache ls` lists them."},
		},
		SeeAlso: []string{"pay cache ls", "pay cache clear", "pay config paths"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay cache info
// ---------------------------------------------------------------------------

type cacheTTLRow struct {
	Class      string `json:"class"`
	FreshS     int64  `json:"fresh_s"`
	HardMaxS   int64  `json:"hard_max_s"`
	Persisted  bool   `json:"persisted"`
	ServeStale bool   `json:"serve_stale"`
}

type cacheInfoData struct {
	Enabled      bool             `json:"enabled"`
	Path         string           `json:"path"`
	EpochDir     string           `json:"epoch_dir"`
	Epoch        string           `json:"epoch"`
	Scopes       int              `json:"scopes"`
	Bytes        int64            `json:"bytes"`
	CurrentScope string           `json:"current_scope,omitempty"`
	Current      *cache.ScopeInfo `json:"current,omitempty"`
	Freshness    string           `json:"freshness,omitempty"`
	AgeS         *int64           `json:"age_s,omitempty"`
	TTL          []cacheTTLRow    `json:"ttl"`
}

func newCacheInfoCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Summarise the cache and the current scope's freshness",
		Args:  maxArgs(0, "pay cache info"),
		RunE: Handle(rt, "cache info", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			store := rt.Cache()
			scopes := store.Scopes()

			data := cacheInfoData{
				Enabled:  store.Enabled(),
				Path:     rt.Paths.CacheDir,
				EpochDir: store.EpochDir(),
				Epoch:    cache.Epoch,
				Scopes:   len(scopes),
				TTL:      cacheTTLTable(),
			}
			for i := range scopes {
				data.Bytes += scopes[i].Bytes
			}

			var next *output.Next
			if sc, err := rt.Scope(); err == nil {
				data.CurrentScope = sc.Key
				for i := range scopes {
					if scopes[i].Scope == sc.Key {
						data.Current = &scopes[i]
						break
					}
				}
				cm, ok, warn := store.ReadManifest(sc)
				if warn != nil {
					rt.Warn(*warn)
				}
				if ok {
					fresh, age := cm.Freshness(rt.Now())
					secs := int64(age / time.Second)
					data.Freshness = string(fresh)
					data.AgeS = &secs
				} else if store.Enabled() {
					data.Freshness = "cold"
					next = &output.Next{Reason: output.ReasonDiscoverFirst, Cmd: "pay discover"}
				}
			}

			env := output.New("cache info", output.KindOpResult, jsonValue(&data))
			if next != nil {
				env.WithNext(next)
			}
			return env, nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache info"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"current_scope":"…","freshness":"fresh|stale|missing","age_s":380,"bytes":2949312,"enabled":true,"epoch":"v1","current":{"collections":50,"globals":5,"shards":55,"base_url":"…"}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitConfig},
		Examples: []Example{
			{Why: "is this profile's view fresh, and how old?", Cmd: "pay cache info"},
			{Why: "just the age in seconds", Cmd: "pay cache info --path .age_s --output id"},
			{Why: "fresh or stale, as one word", Cmd: "pay cache info --path .freshness --output id"},
			{Why: "check a different profile's scope", Cmd: "pay cache info --profile staging"},
			{Why: "refresh it if it is stale", Cmd: "pay cache info || pay cache warm"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading a stale cache as a failure.",
				Right: "Staleness is reported, not fatal: PayCLI revalidates on the next command. `pay cache warm` forces it now."},
			{Wrong: "Comparing age_s against your own idea of a TTL.",
				Right: "Read .freshness instead — PayCLI has already applied the profile's TTL to produce it."},
			{Wrong: "Expecting document counts here.",
				Right: "This describes the SCHEMA cache — collections, fields, permissions. `pay count <collection>` counts documents; `pay cache warm --deep` adds counts to discovery."},
		},
		SeeAlso: []string{"pay cache warm", "pay cache ls", "pay discover --refresh"},
	})
	return cmd
}

// cacheTTLTable renders §8.5's freshness ladder so an operator can see, without
// reading the source, how long each class is trusted and for how long it may be
// served stale.
func cacheTTLTable() []cacheTTLRow {
	classes := cache.Classes()
	rows := make([]cacheTTLRow, 0, len(classes))
	for _, c := range classes {
		ttl, ok := cache.TTLFor(c)
		if !ok {
			continue
		}
		rows = append(rows, cacheTTLRow{
			Class:      string(c),
			FreshS:     int64(ttl.Fresh / time.Second),
			HardMaxS:   int64(ttl.HardMax / time.Second),
			Persisted:  ttl.Persist,
			ServeStale: ttl.HardMax > ttl.Fresh,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Class < rows[j].Class })
	return rows
}

// ---------------------------------------------------------------------------
// pay cache ls
// ---------------------------------------------------------------------------

func newCacheLsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List every cached scope",
		Long: `List every cached scope with the base URL, api_path, header names, key
fingerprint and profiles that produced it.

Header VALUES and the credential itself are never stored (§8.1); only the
sorted header names and a 16-hex fingerprint of the credential, which is what
lets this command explain why two profiles do or do not share a scope.`,
		Args: maxArgs(0, "pay cache ls"),
		RunE: Handle(rt, "cache ls", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			store := rt.Cache()
			scopes := store.Scopes()
			if scopes == nil {
				scopes = []cache.ScopeInfo{}
			}
			sort.Slice(scopes, func(i, j int) bool { return scopes[i].Scope < scopes[j].Scope })

			current := ""
			if sc, err := rt.Scope(); err == nil {
				current = sc.Key
			}
			data := map[string]any{
				"enabled":       store.Enabled(),
				"path":          rt.Paths.CacheDir,
				"current_scope": current,
				"count":         len(scopes),
				"scopes":        scopes,
			}
			env := output.New("cache ls", output.KindOpResult, jsonValue(data))
			for i := range scopes {
				if !scopes[i].Readable {
					rt.Warn(output.Warning{
						Code:    cache.WarnCacheUnreadable,
						Message: scopes[i].Path + ": the scope has no readable manifest",
						Hint:    "pay cache clear --scope " + scopes[i].Scope,
					})
				}
			}
			return env, nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache ls"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"count":1,"current_scope":"…","path":"…","enabled":true,"scopes":[{"scope":"…","base_url":"…","profiles":["dummy"],"collections":50,"readable":true}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal},
		Examples: []Example{
			{Why: "every scope this machine has cached", Cmd: "pay cache ls"},
			{Why: "as a table", Cmd: "pay cache ls --output table"},
			{Why: "just the scope keys", Cmd: "pay cache ls --path .scopes[].scope"},
			{Why: "then inspect one of them", Cmd: "pay cache show current"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting one row per profile.",
				Right: "Rows are SCOPES, not profiles: two profiles with the same base URL, api_path, headers and credential share one row, and one profile with a rotated key produces two."},
			{Wrong: "Reading an unreadable or foreign-epoch row as an error.",
				Right: "It is reported as a row with a warning. PayCLI re-discovers rather than failing."},
			{Wrong: "Using a scope key from another machine.",
				Right: "The key includes a credential fingerprint; it is meaningful only where it was generated."},
		},
		SeeAlso: []string{"pay cache show <scope>", "pay cache info", "pay cache clear --all"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay cache show <scope>
// ---------------------------------------------------------------------------

func newCacheShowCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <scope>",
		Short: "Print one scope's discovery index",
		Long: `Print one scope's manifest.json verbatim. Use "current" (or omit nothing —
the scope key comes from ` + "`pay cache ls`" + `) to inspect the scope this
profile resolves to.`,
		Args: exactArgs(1, "pay cache show <scope>  (use \"current\" for this profile's scope)"),
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return cacheScopeNames(rt), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: Handle(rt, "cache show", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			key := strings.TrimSpace(args[0])
			if key == "current" || key == "." {
				sc, err := rt.Scope()
				if err != nil {
					return nil, err
				}
				key = sc.Key
			}
			if !cache.ValidScopeKey(key) {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"%q is not a cache scope key.", args[0]).
					WithDidYouMean(apierr.DidYouMean(key, cacheScopeNames(rt))...).
					WithHint("`pay cache ls` lists the scopes that exist.")
			}

			store := rt.Cache()
			sc := cache.Scope{Key: key}
			cm, ok, warn := store.ReadManifest(sc)
			if warn != nil {
				rt.Warn(*warn)
			}
			if !ok {
				return nil, apierr.New(apierr.CodeDocNotFound,
					"no readable discovery index for scope %s.", key).
					WithDidYouMean(apierr.DidYouMean(key, cacheScopeNames(rt))...).
					WithHint("`pay cache ls` lists the scopes that exist; `pay discover` builds one.")
			}

			// The manifest is emitted verbatim: it is already canonical,
			// key-sorted, redacted JSON on disk (§8.2), so re-encoding it here
			// could only lose fidelity.
			var decoded any
			dec := json.NewDecoder(strings.NewReader(string(cm.Raw)))
			dec.UseNumber()
			if err := dec.Decode(&decoded); err != nil {
				return nil, apierr.Wrap(err, apierr.CodeCacheCorrupt,
					"%s could not be decoded: %v", cm.Path, err).
					WithHint("pay cache clear --scope %s", key)
			}
			fresh, age := cm.Freshness(rt.Now())
			rt.Log.Debug("cache show", "scope", key, "freshness", string(fresh))

			env := output.New("cache show", output.KindSchema, jsonValue(map[string]any{
				"scope":     key,
				"path":      cm.Path,
				"freshness": string(fresh),
				"age_s":     int64(age / time.Second),
				"manifest":  decoded,
			}))
			return env.WithRawBody(cm.Raw, true), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay cache show <scope>"},
		Args: []ArgSpec{{Name: "scope", Required: true, Type: "string",
			ValuesFrom: "pay cache ls --path .scopes[].scope", Example: "current"}},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"scope":"…","path":"…","age_s":897,"freshness":"stale","manifest":{…}}  · --output raw prints manifest.json verbatim`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitNotFound, apierr.ExitValidation},
		Examples: []Example{
			{Why: "this profile's own scope", Cmd: "pay cache show current"},
			{Why: "the raw manifest.json, unmodified", Cmd: "pay cache show current --output raw"},
			{Why: "which collections it knows about", Cmd: "pay cache show current --path .manifest.collections[].slug"},
			{Why: "a specific scope from `pay cache ls`", Cmd: "pay cache show 8c95d0750f63e960"},
		},
		Mistakes: []Mistake{
			{Wrong: "Omitting the argument.",
				Right: "The scope is required; pass \"current\" for the one this profile resolves to."},
			{Wrong: "Editing the manifest to change PayCLI's behaviour.",
				Right: "It is checksummed and re-verified on read; a hand-edit is discarded as a torn write. Pin behaviour in config instead."},
			{Wrong: "Treating it as documentation of the project.",
				Right: "It is a CACHE, as of generated_at. `pay explain` renders the same knowledge with freshness and provenance attached."},
		},
		SeeAlso: []string{"pay cache ls", "pay explain", "pay describe <collection>"},
	})
	return cmd
}

func cacheScopeNames(rt *Runtime) []string {
	infos := rt.Cache().Scopes()
	out := make([]string, 0, len(infos)+1)
	out = append(out, "current")
	for _, i := range infos {
		out = append(out, i.Scope)
	}
	return out
}

// ---------------------------------------------------------------------------
// pay cache clear
// ---------------------------------------------------------------------------

func newCacheClearCmd(rt *Runtime) *cobra.Command {
	var (
		scope string
		all   bool
	)
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete cached discovery data",
		Long: `Delete cached discovery data. With no flags this clears the scope the
current profile resolves to; --all clears every scope plus the cached auth
collection resolution.

This is never destructive to the server and never needs --yes: the only cost of
clearing is one extra discovery on the next command.`,
		Args: maxArgs(0, "pay cache clear [--scope S|--all]"),
		RunE: Handle(rt, "cache clear", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			if all && scope != "" {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"--all and --scope are mutually exclusive.")
			}
			store := rt.Cache()
			now := rt.Now()
			cleared := []string{}

			switch {
			case all:
				before := store.Scopes()
				if err := store.ClearAll(); err != nil {
					return nil, apierr.Wrap(err, apierr.CodeInternal,
						"could not clear the cache: %v", err).
						WithHint("check the permissions on %s", rt.Paths.CacheDir)
				}
				for _, s := range before {
					cleared = append(cleared, s.Scope)
				}
			default:
				key := scope
				if key == "" {
					sc, err := rt.Scope()
					if err != nil {
						return nil, err
					}
					key = sc.Key
				}
				if !cache.ValidScopeKey(key) {
					return nil, apierr.New(apierr.CodeInvalidArgs,
						"%q is not a cache scope key.", scope).
						WithDidYouMean(apierr.DidYouMean(key, cacheScopeNames(rt))...).
						WithHint("`pay cache ls` lists the scopes that exist.")
				}
				if err := store.Clear(key, now); err != nil {
					return nil, apierr.Wrap(err, apierr.CodeInternal,
						"could not clear scope %s: %v", key, err).
						WithHint("check the permissions on %s", store.Root())
				}
				cleared = append(cleared, key)
			}

			return output.New("cache clear", output.KindOpResult, jsonValue(map[string]any{
				"cleared": cleared,
				"all":     all,
				"path":    rt.Paths.CacheDir,
			})).WithNext(&output.Next{
				Reason: output.ReasonRefreshDiscovery,
				Cmd:    "pay discover",
			}), nil
		}),
	}
	cmd.Flags().StringVar(&scope, "scope", "", "scope key to clear (default: this profile's scope)")
	cmd.Flags().BoolVar(&all, "all", false, "clear every scope and the cached auth resolution")
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache clear [--scope KEY] [--all]"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"cleared":["…"],"removed":2}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitConfig},
		Examples: []Example{
			{Why: "the schema changed after a deploy", Cmd: "pay cache clear"},
			{Why: "every scope, plus the cached auth resolution", Cmd: "pay cache clear --all"},
			{Why: "one specific scope from `pay cache ls`", Cmd: "pay cache clear --scope 8c95d0750f63e960"},
			{Why: "clear and immediately re-discover", Cmd: "pay cache clear && pay cache warm"},
		},
		Mistakes: []Mistake{
			{Wrong: "Fearing data loss.",
				Right: "Only discovery data is removed — never documents, never credentials. The next command re-discovers."},
			{Wrong: "Expecting --all to log you out.",
				Right: "--all drops the cached auth RESOLUTION, not the credential. `pay auth logout` removes the credential."},
			{Wrong: "Clearing the cache to fix a permission error.",
				Right: "A 403 is the server's answer, not a stale cache. Check `pay whoami` and `pay access` first."},
		},
		SeeAlso: []string{"pay cache warm", "pay discover --refresh", "pay auth logout"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay cache warm
// ---------------------------------------------------------------------------

func newCacheWarmCmd(rt *Runtime) *cobra.Command {
	var deep bool
	cmd := &cobra.Command{
		Use:   "warm",
		Short: "Run discovery now and persist it",
		Long: `Run discovery now and write it to the cache, so the next command is a
pure local read.

Worth doing once after a deploy or a schema change, and in a container image
build where the first real command would otherwise pay for a cold discovery.`,
		Args: maxArgs(0, "pay cache warm [--deep]"),
		RunE: Handle(rt, "cache warm", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			if err := rt.requireServer(); err != nil {
				return nil, err
			}
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()

			started := rt.Now()
			manifest, err := rt.RunDiscovery(ctx, DiscoverOptions{Deep: deep})
			if err != nil {
				return nil, err
			}
			elapsed := rt.Now().Sub(started)

			scopeKey := ""
			if sc, serr := rt.Scope(); serr == nil {
				scopeKey = sc.Key
			}
			data := map[string]any{
				"scope":              scopeKey,
				"generation":         manifest.Generation,
				"discovery_revision": manifest.Revision(),
				"collections":        len(manifest.Collections),
				"globals":            len(manifest.Globals),
				"limitations":        len(manifest.Limitations),
				"unreachable":        len(manifest.Unreachable),
				"duration_ms":        elapsed.Milliseconds(),
				"persisted":          rt.Cache().Enabled() && !rt.Cfg.NoCache,
				"expires_at":         manifest.ExpiresAt,
			}
			env := output.New("cache warm", output.KindOpResult, jsonValue(data))
			if rt.Cfg.NoCache {
				env.AddWarning(output.Warning{
					Code: cache.WarnCacheWriteFailed,
					Message: "discovery ran but --no-cache kept it out of the cache, " +
						"so the next command will run it again.",
					Hint: "drop --no-cache to make warming durable.",
				})
			}
			return env, nil
		}),
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "also probe every per-collection capability and count documents")
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay cache warm [--deep]"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"scope":"…","collections":49,"globals":3,"duration_ms":420}`},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "discover now, so later commands are offline and fast", Cmd: "pay cache warm"},
			{Why: "also probe per-collection capabilities and count documents", Cmd: "pay cache warm --deep"},
			{Why: "warm a specific profile before a batch of work", Cmd: "pay cache warm --profile staging"},
			{Why: "confirm what it learned", Cmd: "pay cache warm && pay explain"},
		},
		Mistakes: []Mistake{
			{Wrong: "Running --deep on every invocation.",
				Right: "--deep probes every collection and counts documents: many more requests, and the counts are stale the moment they land. Use it once after a schema change."},
			{Wrong: "Expecting warm to repair a broken connection.",
				Right: "It performs the same discovery any command would; if the server is unreachable it fails the same way. `pay doctor` is the diagnostic."},
			{Wrong: "Warming one profile and expecting another to benefit.",
				Right: "Each scope is discovered separately; warm each profile you are about to use."},
		},
		SeeAlso: []string{"pay cache info", "pay discover --refresh", "pay doctor"},
	})
	return cmd
}
