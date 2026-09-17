package cli

import (
	"context"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

func init() { Register(newSkillsCmd) }

// newSkillsCmd builds `pay skills` (§14).
//
// The skill is the piece of PayCLI that an agent reads before it runs anything,
// so the canonical copy lives only inside the binary (//go:embed). There is no
// top-level skills/ directory to drift out of sync with the binary that
// documents it, and `pay skills print` makes one unnecessary.
func newSkillsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "skills",
		GroupID: GroupAdmin,
		Short:   "Install the PayCLI agent skill",
		Long: `Install the PayCLI agent skill into a coding agent's skill directory.

The skill teaches an agent the envelope, the exit-code table, the query DSL and
the handful of Payload behaviours that produce confidently wrong answers when
guessed (draft reads, whole-document validation, locale fallback). It is copied,
never symlinked, and every installed file is hashed into a .pay-skill.json
manifest so a reinstall can tell "you edited this" from "this is out of date".

Default scope is the detected project root; --global installs into your home
directory instead.`,
	}
	cmd.AddCommand(
		newSkillsInstallCmd(rt),
		newSkillsUpdateCmd(rt),
		newSkillsUninstallCmd(rt),
		newSkillsStatusCmd(rt),
		newSkillsListCmd(rt),
		newSkillsPrintCmd(rt),
	)
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay skills install|update|status|uninstall|list|print"},
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitNotFound},
		Examples: []Example{
			{Why: "install into every agent detected in this project", Cmd: "pay skills install"},
			{Why: "install for this user instead of one project", Cmd: "pay skills install --global"},
			{Why: "where is it installed, and has it drifted?", Cmd: "pay skills status"},
			{Why: "refresh after upgrading the binary", Cmd: "pay skills update"},
			{Why: "read a file without installing anything", Cmd: "pay skills print --output raw"},
		},
		Mistakes: []Mistake{
			{Wrong: "Symlinking the skill so it tracks the binary.",
				Right: "Files are COPIED, and each copy is hashed into .pay-skill.json so a reinstall can tell \"you edited this\" from \"this is out of date\". A symlink defeats the drift check."},
			{Wrong: "Editing an installed SKILL.md and then running `pay skills update`.",
				Right: "Your edits are detected and preserved; update refuses to clobber them without --force. Check `pay skills status` first."},
			{Wrong: "Expecting the skill to update itself when PayCLI is upgraded.",
				Right: "The skill is a copy of what the binary embeds at install time. Run `pay skills update` after an upgrade; `pay skills status` reports the version gap."},
			{Wrong: "Committing a top-level skills/ directory to keep it in sync.",
				Right: "`pay skills print --output raw > SKILL.md` regenerates it from the binary on demand, so the checked-in copy cannot silently drift."},
		},
		SeeAlso: []string{"pay skills status", "pay explain", "pay skills list"},
	})
	return cmd
}

// skillsFlags is the shared flag set of install/update/uninstall/status.
type skillsFlags struct {
	global      bool
	scope       string
	dirs        []string
	agents      []string
	force       bool
	withProject bool
	projectOnly bool
	root        string
}

func (f *skillsFlags) bind(cmd *cobra.Command) {
	s := cmd.Flags()
	s.BoolVar(&f.global, "global", false, "install for the current user instead of this project")
	s.StringVar(&f.scope, "scope", "", "project|user (overrides --global and detection)")
	s.StringArrayVar(&f.dirs, "dir", nil, "extra agent skills directory; the skill lands in <dir>/pay (repeatable)")
	s.StringSliceVar(&f.agents, "agent", nil, "agents to target: "+strings.Join(skills.AgentNames(), ",")+", or all")
	s.StringVar(&f.root, "root", "", "project root to install into (default: detected)")
}

// options turns the flags plus the runtime into skills.Options.
func (f *skillsFlags) options(rt *Runtime) (skills.Options, error) {
	opts := skills.Options{
		StartDir:    rt.App.WorkDir,
		ProjectRoot: f.root,
		Home:        homeDirOf(rt),
		ExtraDirs:   append([]string{}, f.dirs...),
		Force:       f.force,
		CLIVersion:  buildinfo.Version(),
		Now:         rt.Now(),
	}
	if rt.Cfg != nil {
		opts.DryRun = rt.Cfg.DryRun
		opts.Profile = rt.Cfg.Profile
		opts.BaseURL = rt.Cfg.BaseURL
	}

	switch {
	case f.scope != "":
		switch strings.ToLower(strings.TrimSpace(f.scope)) {
		case string(skills.ScopeProject):
			opts.Scope = skills.ScopeProject
		case string(skills.ScopeUser):
			opts.Scope = skills.ScopeUser
		default:
			return opts, apierr.New(apierr.CodeInvalidOption,
				"%q is not a valid --scope. Valid values: project, user.", f.scope).
				WithDidYouMean(apierr.DidYouMean(f.scope, []string{"project", "user"})...)
		}
	case f.global:
		opts.Scope = skills.ScopeUser
	}

	for _, a := range f.agents {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == "all" {
			opts.AllAgents = true
			continue
		}
		if _, ok := skills.FindAgent(a); !ok {
			return opts, apierr.New(apierr.CodeInvalidOption,
				"%q is not a known agent.", a).
				WithDidYouMean(apierr.DidYouMean(a, skills.AgentNames())...).
				WithHint("Known agents: %s. Use --dir PATH for anything not on the list; "+
					"nothing about the skill is agent-specific.", strings.Join(skills.AgentNames(), ", "))
		}
		opts.Agents = append(opts.Agents, a)
	}
	return opts, nil
}

// homeDirOf resolves the user's home directory from the captured environment
// rather than from the process, so a test can pin it (§3.1).
func homeDirOf(rt *Runtime) string {
	if h, ok := rt.Env.Lookup("HOME"); ok {
		return h
	}
	if h, ok := rt.Env.Lookup("USERPROFILE"); ok {
		return h
	}
	return ""
}

// ---------------------------------------------------------------------------
// pay skills install / update
// ---------------------------------------------------------------------------

func newSkillsInstallCmd(rt *Runtime) *cobra.Command {
	f := &skillsFlags{}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy the skill into every detected agent's skills directory",
		Long: `Copy the skill into every agent whose home directory already exists under
the target scope. An agent named explicitly with --agent, or --agent all, has
its directory created; agents that are skipped are reported with the reason.

Reinstall behaviour: a file whose hash matches the manifest is overwritten
silently, a file identical to the embedded copy is left alone and reported
"unchanged", and a file you edited is SKIPPED with a warning unless --force.`,
		Example: `  pay skills install
  pay skills install --with-project-context
  pay skills install --global --agent claude,codex
  pay skills install --dir ~/.config/myagent/skills`,
		Args: maxArgs(0, "pay skills install [--global] [--agent a,b|all] [--dir PATH] [--force]"),
		RunE: Handle(rt, "skills install", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			return runSkillsInstall(ctx, rt, f, "skills install")
		}),
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&f.force, "force", false, "overwrite files you edited")
	cmd.Flags().BoolVar(&f.withProject, "with-project-context", false,
		"run discovery and write references/PROJECT.md for this project")
	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay skills install [--global|--scope project|user] [--agent A,B|all]",
			"pay skills install [--dir PATH]… [--root PATH] [--force] [--with-project-context]",
		},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"scope":"project","root":"…","targets":[{"agent":"claude","path":"…/skills/pay","written":6}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitNotFound},
		Examples: []Example{
			{Why: "every agent detected in this project", Cmd: "pay skills install"},
			{Why: "for this user, not one project", Cmd: "pay skills install --global"},
			{Why: "one agent only", Cmd: "pay skills install --agent claude"},
			{Why: "an agent PayCLI does not detect", Cmd: "pay skills install --dir ./.myagent/skills"},
			{Why: "include a discovered summary of THIS project", Cmd: "pay skills install --with-project-context"},
			{Why: "re-install over files you edited", Cmd: "pay skills install --force"},
		},
		Mistakes: []Mistake{
			{Wrong: "Re-running install to pick up a new PayCLI version.",
				Right: "`pay skills update` is the upgrade path; install refuses to overwrite edited files, so a plain re-run can silently do nothing."},
			{Wrong: "Passing --dir expecting the files at exactly that path.",
				Right: "The skill lands in <dir>/pay, so each agent's skills directory can hold several skills side by side."},
			{Wrong: "Using --with-project-context offline.",
				Right: "It runs discovery against the server to write references/PROJECT.md; without a reachable base URL it cannot produce that file."},
		},
		SeeAlso: []string{"pay skills status", "pay skills update", "pay skills list"},
	})
	return cmd
}

func newSkillsUpdateCmd(rt *Runtime) *cobra.Command {
	f := &skillsFlags{}
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Refresh installed skill files to this binary's version",
		Long: `Refresh every installed skill file to the version embedded in this binary.

Files you edited are still skipped unless --force: an update is not permission
to discard your notes. --project-only regenerates references/PROJECT.md from a
fresh discovery and leaves the static files alone when they are already current.`,
		Args: maxArgs(0, "pay skills update [--project-only]"),
		RunE: Handle(rt, "skills update", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			// --project-only implies the project context: regenerating
			// PROJECT.md is the entire point of the flag.
			if f.projectOnly {
				f.withProject = true
			}
			return runSkillsInstall(ctx, rt, f, "skills update")
		}),
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&f.force, "force", false, "overwrite files you edited")
	cmd.Flags().BoolVar(&f.withProject, "with-project-context", true,
		"refresh references/PROJECT.md as well (default true for update)")
	cmd.Flags().BoolVar(&f.projectOnly, "project-only", false,
		"only refresh references/PROJECT.md")
	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay skills update [--global|--scope project|user] [--agent A,B|all]",
			"pay skills update [--force] [--project-only] [--with-project-context=false]",
		},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"scope":"project","targets":[{"agent":"claude","path":"…","updated":6,"skipped":0}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitNotFound},
		Examples: []Example{
			{Why: "after upgrading the binary", Cmd: "pay skills update"},
			{Why: "see what would be stale first", Cmd: "pay skills status"},
			{Why: "re-discover the project summary only", Cmd: "pay skills update --project-only"},
			{Why: "take your edits back out", Cmd: "pay skills update --force"},
			{Why: "the user-level install rather than this project", Cmd: "pay skills update --global"},
		},
		Mistakes: []Mistake{
			{Wrong: "Assuming update silently overwrites everything.",
				Right: "Files you edited are SKIPPED, and reported as skipped. --force is what overwrites them."},
			{Wrong: "Running update where nothing is installed.",
				Right: "It updates existing installs only. Use `pay skills install` for a first install; `pay skills status` shows which is which."},
			{Wrong: "Expecting --project-only to refresh the skill itself.",
				Right: "It refreshes references/PROJECT.md alone — the discovered description of THIS project — and leaves every other file untouched."},
		},
		SeeAlso: []string{"pay skills status", "pay skills install", "pay version"},
	})
	return cmd
}

func runSkillsInstall(ctx context.Context, rt *Runtime, f *skillsFlags, name string) (*output.Envelope, error) {
	opts, err := f.options(rt)
	if err != nil {
		return nil, err
	}

	if f.withProject {
		doc, revision, warn := skillsProjectDoc(ctx, rt, opts)
		if warn != nil {
			rt.Warn(*warn)
		}
		opts.Project = doc
		opts.DiscoveryRevision = revision
	}

	res, err := skills.Install(opts)
	if err != nil {
		return nil, err
	}
	rt.Warn(res.Warnings...)

	env := output.New(name, output.KindOpResult, jsonValue(res))
	if len(res.Targets) == 0 {
		env.AddWarning(output.Warning{
			Code: skills.WarnAgentSkipped,
			Message: "no agent skills directory was found under " + res.Root +
				", so nothing was installed.",
			Hint: "pass --agent all to create them, --dir PATH for an agent PayCLI does not know, " +
				"or --global to install for the current user.",
		})
	}
	return env, nil
}

// skillsProjectDoc renders references/PROJECT.md from a live discovery run.
//
// §14 requires it to degrade gracefully: an unreachable server skips the file
// and warns, rather than failing an install that is otherwise perfectly valid.
func skillsProjectDoc(ctx context.Context, rt *Runtime, opts skills.Options) ([]byte, string, *output.Warning) {
	if err := rt.requireServer(); err != nil {
		return nil, "", &output.Warning{
			Code:    skills.WarnProjectContextUnavailable,
			Message: "references/PROJECT.md was not written: " + err.Error(),
			Hint:    "pay skills update --project-only writes it once a base URL is configured.",
		}
	}
	ctx, cancel := rt.deadlineContext(ctx)
	defer cancel()

	manifest, err := rt.Discovery(ctx)
	if err != nil {
		return nil, "", &output.Warning{
			Code:    skills.WarnProjectContextUnavailable,
			Message: "references/PROJECT.md was not written: " + err.Error(),
			Hint:    "the static skill files were still installed; run `pay skills update --project-only` when the server is reachable.",
		}
	}

	pc := skills.ProjectContext{
		CLIVersion:        opts.CLIVersion,
		GeneratedAt:       rt.Now(),
		Profile:           opts.Profile,
		BaseURL:           opts.BaseURL,
		DiscoveryRevision: manifest.Revision(),
	}
	if rt.Cfg != nil {
		pc.APIPath = rt.Cfg.APIPath
	}
	if v := manifest.Source.PayloadVersion; v != nil {
		pc.PayloadVersion = *v
	}
	pc.AuthMode = manifest.Identity.AuthMode
	if c := manifest.Identity.AuthCollection; c != nil {
		pc.AuthCollection = *c
	}
	pc.Locales = manifest.Capabilities.Localization.Locales
	if d := manifest.Capabilities.Localization.Default; d != nil {
		pc.DefaultLocale = *d
	}

	for _, c := range manifest.Collections {
		pc.Collections = append(pc.Collections, skillsCollectionInfo(ctx, rt, c))
	}
	for _, g := range manifest.Globals {
		pc.Globals = append(pc.Globals, skills.GlobalInfo{
			Slug:     g.Slug,
			Label:    g.Labels.Singular,
			Features: globalFeatures(g),
		})
	}
	sort.Slice(pc.Collections, func(i, j int) bool { return pc.Collections[i].Slug < pc.Collections[j].Slug })
	sort.Slice(pc.Globals, func(i, j int) bool { return pc.Globals[i].Slug < pc.Globals[j].Slug })

	return skills.RenderProject(pc), pc.DiscoveryRevision, nil
}

func skillsCollectionInfo(ctx context.Context, rt *Runtime, c *discovery.Collection) skills.CollectionInfo {
	info := skills.CollectionInfo{
		Slug:      c.Slug,
		Label:     c.Labels.Plural,
		Singular:  c.Labels.Singular,
		IDType:    c.IDType,
		Internal:  c.Internal,
		TotalDocs: -1,
		Ops:       collectionOps(c),
		Features:  collectionFeatures(c),
	}
	if c.Stats.TotalDocs != nil {
		info.TotalDocs = *c.Stats.TotalDocs
	}
	if shard, ok := rt.Shard(ctx, c.Slug, cache.KindCollection); ok {
		info.KeyFields = keyFieldsOf(shard, c.TitleField)
	} else if c.TitleField != nil {
		info.KeyFields = []string{*c.TitleField}
	}
	return info
}

func collectionOps(c *discovery.Collection) []string {
	var ops []string
	if c.Permissions.Create {
		ops = append(ops, "create")
	}
	if c.Permissions.Read {
		ops = append(ops, "read")
	}
	if c.Permissions.Update {
		ops = append(ops, "update")
	}
	if c.Permissions.Delete {
		ops = append(ops, "delete")
	}
	return ops
}

// collectionFeatures lists only capabilities that are known TRUE. A nil flag
// means "never learned" (§7.6's tri-state) and must not be rendered as either
// yes or no, because PROJECT.md is read as fact.
func collectionFeatures(c *discovery.Collection) []string {
	var out []string
	add := func(name string, flag *bool) {
		if flag != nil && *flag {
			out = append(out, name)
		}
	}
	add("upload", c.Flags.Upload)
	add("auth", c.Flags.Auth)
	add("versions", c.Flags.Versions)
	add("drafts", c.Flags.Drafts)
	add("trash", c.Flags.Trash)
	add("folders", c.Flags.Folders)
	add("duplicate", c.Flags.Duplicate)
	add("orderable", c.Flags.Orderable)
	return out
}

func globalFeatures(g *discovery.Global) []string {
	var out []string
	if g.Flags.Versions != nil && *g.Flags.Versions {
		out = append(out, "versions")
	}
	if g.Flags.Drafts != nil && *g.Flags.Drafts {
		out = append(out, "drafts")
	}
	return out
}

// keyFieldsOf picks the handful of fields worth naming in PROJECT.md: the title
// field first (it is what a human recognises the document by), then the
// required top-level scalars, which are what a create call must supply.
func keyFieldsOf(shard *discovery.Shard, title *string) []string {
	const maxKeyFields = 6
	seen := map[string]bool{}
	out := make([]string, 0, maxKeyFields)
	add := func(name string) {
		if name == "" || seen[name] || len(out) >= maxKeyFields {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	if title != nil {
		add(*title)
	}
	for _, p := range shard.RequiredPaths {
		if strings.Contains(p, ".") {
			continue
		}
		add(p)
	}
	return out
}

// ---------------------------------------------------------------------------
// pay skills uninstall / status / list / print
// ---------------------------------------------------------------------------

func newSkillsUninstallCmd(rt *Runtime) *cobra.Command {
	f := &skillsFlags{}
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove skill files PayCLI installed",
		Long: `Remove only the files recorded in .pay-skill.json whose hash still matches,
then prune the directories that are left empty. A file you edited is left in
place: PayCLI never deletes your work.`,
		Args: maxArgs(0, "pay skills uninstall [--global] [--agent a,b|all]"),
		RunE: Handle(rt, "skills uninstall", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			opts, err := f.options(rt)
			if err != nil {
				return nil, err
			}
			res, err := skills.Uninstall(opts)
			if err != nil {
				return nil, err
			}
			rt.Warn(res.Warnings...)
			return output.New("skills uninstall", output.KindOpResult, jsonValue(res)), nil
		}),
	}
	f.bind(cmd)
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay skills uninstall [--global|--scope project|user] [--agent A,B|all] [--dir PATH]…"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"scope":"project","root":"…","targets":[{"agent":"claude","dir":"…","removed":6,"kept":0}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitNotFound},
		Examples: []Example{
			{Why: "remove it from this project", Cmd: "pay skills uninstall"},
			{Why: "remove the user-level install", Cmd: "pay skills uninstall --global"},
			{Why: "one agent only", Cmd: "pay skills uninstall --agent claude"},
			{Why: "confirm it is gone", Cmd: "pay skills uninstall && pay skills status"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the directory itself to disappear.",
				Right: "Only files PayCLI installed (the ones listed in .pay-skill.json) are removed. Anything you added is kept, and so is the directory holding it."},
			{Wrong: "Assuming --global also clears the project install.",
				Right: "Scopes are independent: run it once per scope, or check `pay skills status` for every location."},
			{Wrong: "Using uninstall to resolve an edit conflict.",
				Right: "`pay skills update --force` replaces the files in place; uninstalling first throws away the manifest that knows what PayCLI owns."},
		},
		SeeAlso: []string{"pay skills status", "pay skills install", "pay skills update"},
	})
	return cmd
}

func newSkillsStatusCmd(rt *Runtime) *cobra.Command {
	f := &skillsFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report every install location and its drift",
		Long: `Report every known install location, the CLI version recorded there against
the running binary, per-file hash drift, and whether references/PROJECT.md was
generated from an older discovery than the current cache.`,
		Args: maxArgs(0, "pay skills status [--global]"),
		RunE: Handle(rt, "skills status", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			opts, err := f.options(rt)
			if err != nil {
				return nil, err
			}
			if m, ok := rt.CachedManifest(); ok {
				opts.DiscoveryRevision = m.Revision()
			}
			report, err := skills.Status(opts)
			if err != nil {
				return nil, err
			}
			data := map[string]any{
				"scope":           report.Scope,
				"root":            report.Root,
				"cli_version":     buildinfo.Version(),
				"targets":         report.Targets,
				"installed_count": countInstalled(report),
			}
			env := output.New("skills status", output.KindOpResult, jsonValue(data))
			if stale := staleTargets(report); len(stale) > 0 {
				env.AddWarning(output.Warning{
					Code: "skill_stale",
					Message: "the skill installed at " + strings.Join(stale, ", ") +
						" was written by a different pay version.",
					Paths: stale,
					Hint:  "pay skills update",
				})
			}
			return env, nil
		}),
	}
	f.bind(cmd)
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay skills status [--global|--scope project|user] [--agent A,B|all] [--dir PATH]…"},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"scope":"project","root":"…","cli_version":"…","installed_count":1,"targets":[{"agent":"claude","dir":"…","files":[{"path":"SKILL.md","state":"current|outdated|modified|missing"}]}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation},
		Examples: []Example{
			{Why: "every install location and its drift", Cmd: "pay skills status"},
			{Why: "the user-level install", Cmd: "pay skills status --global"},
			{Why: "just the states, to decide whether to update", Cmd: "pay skills status --path .targets[].files[].state"},
			{Why: "how many locations are installed", Cmd: "pay skills status --path .installed_count --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading \"modified\" as corruption.",
				Right: "It means YOU edited that file. `pay skills update` deliberately preserves it; --force replaces it."},
			{Wrong: "Confusing \"outdated\" with \"modified\".",
				Right: "outdated = the binary has a newer version of an unedited file (run `pay skills update`); modified = your edit is in the way."},
			{Wrong: "Expecting installed_count to count FILES.",
				Right: "It counts install LOCATIONS. The per-file detail is under targets[].files[]."},
		},
		SeeAlso: []string{"pay skills update", "pay skills install", "pay skills list"},
	})
	return cmd
}

func countInstalled(r *skills.StatusReport) int {
	n := 0
	for _, t := range r.Targets {
		if t.Installed {
			n++
		}
	}
	return n
}

func staleTargets(r *skills.StatusReport) []string {
	var out []string
	for _, t := range r.Targets {
		if t.Installed && (t.Stale || t.ProjectDocStale) {
			out = append(out, t.Dir)
		}
	}
	return out
}

func newSkillsListCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the skill files embedded in this binary",
		Args:    maxArgs(0, "pay skills list"),
		RunE: Handle(rt, "skills list", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			files := skills.Files()
			rows := make([]map[string]any, 0, len(files))
			for _, f := range files {
				rows = append(rows, map[string]any{
					"path":   f.Path,
					"bytes":  len(f.Data),
					"sha256": f.SHA256,
				})
			}
			return output.New("skills list", output.KindOpResult, jsonValue(map[string]any{
				"name":        skills.Name,
				"cli_version": buildinfo.Version(),
				"count":       len(rows),
				"files":       rows,
			})), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay skills list"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"name":"pay","cli_version":"…","count":6,"files":[{"name":"SKILL.md","bytes":12345}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal},
		Examples: []Example{
			{Why: "what this binary would install", Cmd: "pay skills list"},
			{Why: "just the file names", Cmd: "pay skills list --path .files[].name"},
			{Why: "as a table", Cmd: "pay skills list --output table"},
			{Why: "then read one of them", Cmd: "pay skills print references/recipes.md --output raw"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading this as what is INSTALLED.",
				Right: "It lists what is EMBEDDED in this binary. `pay skills status` reports what is on disk and whether it matches."},
			{Wrong: "Expecting references/PROJECT.md here.",
				Right: "That file is generated per project by --with-project-context, not embedded, so it never appears in this list."},
			{Wrong: "Assuming the set is stable across versions.",
				Right: "cli_version is in the same envelope precisely because the file set travels with the binary."},
		},
		SeeAlso: []string{"pay skills print", "pay skills status", "pay skills install"},
	})
	return cmd
}

func newSkillsPrintCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "print [NAME]",
		Short: "Print one embedded skill file",
		Long: `Print one embedded skill file. NAME defaults to SKILL.md and may be given
with or without the references/ prefix.

With --output raw the file is written verbatim, which is what makes a top-level
skills/ directory in the repository unnecessary:

  pay skills print --output raw > SKILL.md`,
		Args: maxArgs(1, "pay skills print [NAME]"),
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return skills.Names(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: Handle(rt, "skills print", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			name := skills.SkillFile
			if len(args) == 1 {
				name = args[0]
			}
			data, err := skills.Read(name)
			if err != nil {
				return nil, apierr.From(err).
					WithDidYouMean(apierr.DidYouMean(name, skills.Names())...).
					WithHint("`pay skills list` names every embedded file.")
			}
			resolved := skills.Resolve(name)
			return output.New("skills print", output.KindRaw, jsonValue(map[string]any{
				"path":    resolved,
				"bytes":   len(data),
				"sha256":  skills.Sum(data),
				"content": string(data),
			})).WithRawBody(data, true), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay skills print [NAME]"},
		Args: []ArgSpec{{Name: "NAME", Required: false, Type: "string",
			ValuesFrom: "pay skills list --path .files[].name",
			Example:    "references/recipes.md  (the references/ prefix is optional; defaults to SKILL.md)"}},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"name":"SKILL.md","bytes":12345,"content":"…"}  · --output raw prints the file verbatim`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitNotFound, apierr.ExitValidation},
		Examples: []Example{
			{Why: "the main skill, verbatim", Cmd: "pay skills print --output raw"},
			{Why: "regenerate a checked-in copy from the binary", Cmd: "pay skills print --output raw > SKILL.md"},
			{Why: "one reference file", Cmd: "pay skills print references/recipes.md --output raw"},
			{Why: "the prefix is optional", Cmd: "pay skills print recipes.md --output raw"},
			{Why: "how big is it?", Cmd: "pay skills print --path .bytes --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Redirecting the default JSON output to a .md file.",
				Right: "Without --output raw you get the envelope with the file inside data.content as a JSON string. --output raw is what writes the file itself."},
			{Wrong: "Guessing a file name.",
				Right: "`pay skills list` enumerates exactly what this binary embeds; an unknown name fails with exit 4 rather than printing a near match."},
			{Wrong: "Printing an INSTALLED file with this command.",
				Right: "It prints what the BINARY embeds, never what is on disk. Read the installed copy from the path in `pay skills status`."},
		},
		SeeAlso: []string{"pay skills list", "pay skills install", "pay explain"},
	})
	return cmd
}
