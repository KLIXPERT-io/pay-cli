package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
	"github.com/KLIXPERT-io/pay-cli/internal/update"
)

func init() { Register(newDoctorCmd) }

// Check statuses. `unknown` is a first-class answer: a fact PayCLI could not
// establish is never reported as a pass or a failure (§7.6).
const (
	CheckOK      = "ok"
	CheckWarn    = "warn"
	CheckFail    = "fail"
	CheckUnknown = "unknown"
	CheckSkipped = "skipped"
)

// Check is one diagnostic line.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
	Value  any    `json:"value,omitempty"`
}

type doctorReport struct {
	OK      bool    `json:"ok"`
	Checks  []Check `json:"checks"`
	Summary struct {
		OK      int `json:"ok"`
		Warn    int `json:"warn"`
		Fail    int `json:"fail"`
		Unknown int `json:"unknown"`
	} `json:"summary"`
	Connection map[string]any `json:"connection"`
	Cache      map[string]any `json:"cache"`
	Audit      map[string]any `json:"audit"`
	Skill      map[string]any `json:"skill"`
	Update     map[string]any `json:"update"`
}

func newDoctorCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Diagnose this project, this profile and this machine in one JSON document.",
		GroupID: GroupDiscovery,
		Args:    maxArgs(0, "pay doctor"),
		RunE: Handle(rt, "doctor", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			report := rt.diagnose(ctx)
			env := output.New("doctor", output.KindCapabilities, report)
			if !report.OK {
				// doctor itself succeeded: it answered the question. The
				// failing checks are the payload, not the command's status.
				rt.Warnf("doctor_failures",
					"%d check(s) failed and %d need attention; read .data.checks",
					report.Summary.Fail, report.Summary.Warn)
			}
			return env, nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay doctor"},
		Long: "This is the command to run when anything is confusing. It answers, in one call:\n" +
			"can I reach the server, is this actually Payload, does my credential resolve to a\n" +
			"user, which collections accept API keys at all, is GraphQL usable, is the cache\n" +
			"healthy and writable, is the audit log writable, and is a newer release pending.\n" +
			"\n" +
			"doctor exits 0 whenever it produced a report — a failing CHECK is data, not an\n" +
			"error. Branch on .data.ok, not on the exit code.",
		Output:    OutputSpec{Kind: output.KindCapabilities, Skeleton: `{"ok":false,"checks":[{"name":"identity","status":"fail","detail":"…","hint":"…"}],"summary":{"ok":9,"warn":1,"fail":1,"unknown":2}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitConfig},
		Examples: []Example{
			{Why: "everything", Cmd: "pay doctor"},
			{Why: "only the failures", Cmd: "pay doctor --path .checks[]  # then filter on .status"},
			{Why: "a one-word health answer", Cmd: "pay doctor --path .ok --output id"},
			{Why: "diagnose a different profile", Cmd: "pay doctor --profile staging"},
			{Why: "which collections can take an API key at all?", Cmd: "pay doctor --path .connection.api_key_collections"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting a non-zero exit when a check fails.",
				Right: "doctor reports; it does not fail. Read .data.ok and .data.summary.fail."},
			{Wrong: "Running `pay auth login --api-key` on a project with no useAPIKey collection.",
				Right: "doctor's api_key_support check names that case and prints the --jwt command instead."},
			{Wrong: "Ignoring an `unknown` status.",
				Right: "It means PayCLI could not establish the fact — usually a permission or a disabled GraphQL endpoint. The hint says what to grant."},
		},
		SeeAlso: []string{"pay explain", "pay auth test", "pay cache info", "pay discover --refresh"},
	})
	return cmd
}

func (rt *Runtime) diagnose(ctx context.Context) *doctorReport {
	r := &doctorReport{
		Connection: map[string]any{},
		Cache:      map[string]any{},
		Audit:      map[string]any{},
		Skill:      map[string]any{},
		Update:     map[string]any{},
	}
	add := func(c Check) { r.Checks = append(r.Checks, c) }

	// --- configuration ----------------------------------------------------
	r.Connection["profile"] = rt.Cfg.Profile
	r.Connection["base_url"] = rt.Cfg.BaseURL
	r.Connection["config_file"] = rt.Paths.ConfigFile
	if err := rt.Cfg.RequireBaseURL(); err != nil {
		add(Check{Name: "base_url", Status: CheckFail,
			Detail: "no base_url is configured for this profile",
			Hint:   "pay auth login --profile " + rt.Cfg.Profile + " --base-url https://example.com"})
		rt.finishReport(r)
		return r
	}
	add(Check{Name: "base_url", Status: CheckOK,
		Detail: "resolved from " + rt.Cfg.Sources["base_url"], Value: rt.Cfg.BaseURL})

	// --- credential -------------------------------------------------------
	cred, err := rt.Credential(ctx)
	switch {
	case err != nil:
		add(Check{Name: "credential", Status: CheckFail, Detail: err.Error(),
			Hint: apierr.From(err).Hint})
	case cred.Anonymous():
		add(Check{Name: "credential", Status: CheckWarn,
			Detail: "no credential resolved; PayCLI will run anonymously",
			Hint:   "pay auth login --profile " + rt.Cfg.Profile + " --api-key-stdin"})
	default:
		add(Check{Name: "credential", Status: CheckOK,
			Detail: fmt.Sprintf("%s from %s", cred.Mode, cred.Source),
			Value:  map[string]any{"key_fingerprint": cred.Fingerprint, "auth_mode": string(cred.Mode)}})
		r.Connection["key_fingerprint"] = cred.Fingerprint
		r.Connection["key_source"] = cred.Source
	}

	// --- reachability, latency, X-Powered-By ------------------------------
	client, clientErr := rt.Client(ctx)
	if clientErr != nil {
		add(Check{Name: "client", Status: CheckFail, Detail: clientErr.Error()})
		rt.finishReport(r)
		return r
	}
	// Discovery runs FIRST, even though its check is reported after this one:
	// §7.2's api_path ladder lives inside the Discoverer, and the probed
	// prefix only reaches the client when the manifest is adopted. Probing
	// before that would send GET /access to the configured /api on a project
	// whose routes.api is /cms-api, so doctor would print
	// `connection.api_path: /cms-api` and a FAIL reachability on /api/access
	// in the same envelope — a healthy project reported as broken, by the one
	// command an agent runs when it is already confused. The checks are still
	// appended in the §18 order: reachability, then discovery.
	m, discErr := rt.Discovery(ctx)

	// client.APIPath() — not rt.Cfg.APIPath — is the prefix the probe below
	// actually carries: adoption swaps it on the client handle every command
	// already holds, so the client is the authority on where doctor knocked.
	apiPath := client.APIPath()
	start := rt.Now()
	access, accessErr := client.Access(ctx)
	latency := rt.Now().Sub(start)
	r.Connection["access_latency_ms"] = latency.Milliseconds()
	if accessErr != nil {
		add(Check{Name: "reachability", Status: CheckFail,
			Detail: "GET " + apiPath + "/access failed: " + accessErr.Error(),
			Hint:   apierr.From(accessErr).Hint})
	} else {
		add(Check{Name: "reachability", Status: CheckOK,
			Detail: fmt.Sprintf("GET %s/access answered in %d ms with %d collections and %d globals",
				apiPath, latency.Milliseconds(), len(access.Collections), len(access.Globals)),
			Value: latency.Milliseconds()})
		if access.HTTP != nil {
			if powered := access.HTTP.Header.Get(payload.HeaderPoweredBy); powered != "" {
				add(Check{Name: "powered_by", Status: CheckOK,
					Detail: payload.HeaderPoweredBy + ": " + powered, Value: powered})
				r.Connection["powered_by"] = powered
			} else {
				// §7.2: the header is advisory. Its absence behind a proxy that
				// strips it is not evidence of anything.
				add(Check{Name: "powered_by", Status: CheckUnknown,
					Detail: "no " + payload.HeaderPoweredBy + " header; many proxies strip it",
					Hint:   "the /api/access shape is the authoritative Payload check, and it passed"})
			}
		}
	}

	// --- discovery --------------------------------------------------------
	// (already run above, so the reachability probe could use its api_path)
	if discErr != nil {
		add(Check{Name: "discovery", Status: CheckFail, Detail: discErr.Error(),
			Hint: apierr.From(discErr).Hint})
		rt.finishReport(r)
		return r
	}
	add(Check{Name: "discovery", Status: CheckOK,
		Detail: fmt.Sprintf("%d collections, %d globals, revision %s", len(m.Collections), len(m.Globals), m.Revision()),
		Value:  m.Revision()})

	r.Connection["api_path"] = m.Source.APIPath
	r.Connection["api_path_source"] = m.Source.APIPathSource
	r.Connection["graphql_path"] = m.Source.GraphQLPath
	r.Connection["graphql_path_source"] = m.Source.GraphQLPathSource
	r.Connection["discovery_revision"] = m.Revision()
	r.Connection["topology_sha256"] = m.Fingerprint.TopologySHA256
	r.Connection["schema_sha256"] = m.Fingerprint.SchemaSHA256
	r.Connection["payload_version"] = m.Source.PayloadVersion
	r.Connection["payload_version_source"] = m.Source.PayloadVersionSource
	r.Connection["db_adapter"] = m.Source.DBAdapter
	r.Connection["db_adapter_source"] = m.Source.DBAdapterSource

	if m.Source.GraphQLPathSource == discovery.SourceDerivedGraphQLPath {
		add(Check{Name: "graphql_path", Status: CheckOK,
			Detail: "derived from api_path + graphql_route: " + m.Source.GraphQLPath, Value: m.Source.GraphQLPath})
	} else {
		add(Check{Name: "graphql_path", Status: CheckOK,
			Detail: m.Source.GraphQLPath + " (" + m.Source.GraphQLPathSource + ")", Value: m.Source.GraphQLPath})
	}

	switch m.Capabilities.GraphQL.Mode {
	case discovery.GraphQLModeOK:
		add(Check{Name: "graphql", Status: CheckOK,
			Detail: "introspection is available at " + m.Capabilities.GraphQL.Path, Value: m.Capabilities.GraphQL.Mode})
	case discovery.GraphQLModeDisabled, discovery.GraphQLModeRouteMissing:
		add(Check{Name: "graphql", Status: CheckWarn,
			Detail: "GraphQL is not available (" + m.Capabilities.GraphQL.Mode + "); field discovery falls back to REST probes",
			Hint:   m.Capabilities.GraphQL.Hint, Value: m.Capabilities.GraphQL.Mode})
	default:
		add(Check{Name: "graphql", Status: CheckUnknown,
			Detail: "GraphQL mode is " + m.Capabilities.GraphQL.Mode + ": " + m.Capabilities.GraphQL.Detail,
			Hint:   m.Capabilities.GraphQL.Hint, Value: m.Capabilities.GraphQL.Mode})
	}

	if m.Source.PayloadVersion == nil {
		add(Check{Name: "payload_version", Status: CheckUnknown,
			Detail: "the Payload version is not exposed by the API and was not found locally",
			Hint:   "pin it with [profiles." + rt.Cfg.Profile + "] payload_version = \"3.86.0\" if a command needs it"})
	} else {
		add(Check{Name: "payload_version", Status: CheckOK,
			Detail: *m.Source.PayloadVersion + " (" + m.Source.PayloadVersionSource + ")", Value: *m.Source.PayloadVersion})
	}
	if m.Source.DBAdapter == "" || m.Source.DBAdapter == discovery.DBUnknown {
		add(Check{Name: "db_adapter", Status: CheckUnknown,
			Detail: "the database adapter could not be determined; operator pre-checks stay disabled",
			Hint:   "pin it with [profiles." + rt.Cfg.Profile + "] db_adapter = \"postgres\""})
	} else {
		add(Check{Name: "db_adapter", Status: CheckOK,
			Detail: m.Source.DBAdapter + " (" + m.Source.DBAdapterSource + ")", Value: m.Source.DBAdapter})
	}

	// --- auth collections and API-key support (§5.4) ----------------------
	authSlugs, keySlugs, keyUnknown := authCollections(m)
	r.Connection["auth_collections"] = authSlugs
	r.Connection["api_key_collections"] = keySlugs
	r.Connection["auth_collection"] = m.Identity.AuthCollection
	r.Connection["auth_collection_source"] = m.Identity.AuthCollectionSource

	if m.Identity.AuthCollection != nil && *m.Identity.AuthCollection != "" {
		add(Check{Name: "auth_collection", Status: CheckOK,
			Detail: *m.Identity.AuthCollection + " (" + m.Identity.AuthCollectionSource + ")",
			Value:  *m.Identity.AuthCollection})
	} else {
		add(Check{Name: "auth_collection", Status: CheckWarn,
			Detail: "no auth collection was resolved",
			Hint:   "pass --auth-collection SLUG; candidates: " + joinOr(authSlugs, "none found")})
	}
	switch {
	case len(keySlugs) > 0:
		add(Check{Name: "api_key_support", Status: CheckOK,
			Detail: "these collections accept API keys: " + joinOr(keySlugs, ""), Value: keySlugs})
	case keyUnknown:
		add(Check{Name: "api_key_support", Status: CheckUnknown,
			Detail: "PayCLI could not determine whether any collection has useAPIKey (GraphQL was unavailable)",
			Hint:   "if `pay auth login --api-key` fails, use: pay auth login --jwt --email you@example.com --password-stdin"})
	default:
		add(Check{Name: "api_key_support", Status: CheckWarn,
			Detail: "no auth collection on this project has useAPIKey, so an API key can never authenticate here",
			Hint:   "pay auth login --jwt --email you@example.com --password-stdin"})
	}

	// --- identity (§5.4) --------------------------------------------------
	switch {
	case cred == nil || cred.Anonymous():
		add(Check{Name: "identity", Status: CheckSkipped,
			Detail: "anonymous mode does not request /me at all",
			Hint:   "pay auth login --profile " + rt.Cfg.Profile + " --api-key-stdin"})
	case m.Identity.Verified:
		add(Check{Name: "identity", Status: CheckOK,
			Detail: fmt.Sprintf("/%s/me returned user %v (strategy %s)",
				derefString(m.Identity.AuthCollection), m.Identity.UserID, m.Identity.Strategy),
			Value: map[string]any{"user_id": m.Identity.UserID, "can_access_admin": m.Identity.CanAccessAdmin}})
	default:
		hint := "pay auth login --profile " + rt.Cfg.Profile + " --api-key-stdin"
		if len(keySlugs) == 0 && !keyUnknown {
			hint = "pay auth login --jwt --email you@example.com --password-stdin"
		}
		add(Check{Name: "identity", Status: CheckFail,
			Detail: "/me did not return a user; the credential is not valid for this project",
			Hint:   hint})
	}

	// --- cache (§8.7) -----------------------------------------------------
	rt.cacheChecks(r, m, add)

	// --- audit log (§12.7) ------------------------------------------------
	auditor := rt.Audit()
	r.Audit["path"] = auditor.Path()
	r.Audit["enabled"] = auditor.Enabled()
	r.Audit["generations"] = auditor.Generations()
	switch {
	case !auditor.Enabled():
		add(Check{Name: "audit_log", Status: CheckWarn,
			Detail: "the audit log is disabled for this run (--no-audit / PAY_NO_AUDIT)",
			Hint:   "drop --no-audit to record writes"})
	default:
		if err := auditor.Writable(); err != nil {
			add(Check{Name: "audit_log", Status: CheckFail,
				Detail: "the audit log is not writable: " + err.Error(),
				Hint:   "every write would abort with audit_write_failed (exit 1); fix the directory or pass --no-audit"})
		} else {
			add(Check{Name: "audit_log", Status: CheckOK, Detail: auditor.Path() + " is writable"})
		}
	}

	// --- skill (§14) ------------------------------------------------------
	rt.skillChecks(r, m, add)

	// --- self-update (§15) ------------------------------------------------
	rt.updateChecks(r, add)

	rt.finishReport(r)
	return r
}

func (rt *Runtime) cacheChecks(r *doctorReport, m *discovery.Manifest, add func(Check)) {
	store := rt.Cache()
	r.Cache["root"] = store.Root()
	r.Cache["enabled"] = store.Enabled()
	if !store.Enabled() {
		add(Check{Name: "cache", Status: CheckWarn,
			Detail: "the cache is disabled, so every command re-runs discovery",
			Hint:   "drop --no-cache"})
		return
	}
	scopes := store.Scopes()
	var bytes int64
	rows := make([]map[string]any, 0, len(scopes))
	unreadable := 0
	for _, s := range scopes {
		bytes += s.Bytes
		if !s.Readable {
			unreadable++
		}
		row := map[string]any{
			"scope": s.Scope, "base_url": s.BaseURL, "api_path": s.APIPath,
			"collections": s.Collections, "globals": s.Globals, "shards": s.Shards,
			"bytes": s.Bytes, "readable": s.Readable, "generation": s.Generation,
		}
		if s.ConfirmedAt != nil {
			row["age_s"] = int64(rt.Now().Sub(*s.ConfirmedAt) / time.Second)
			row["confirmed_at"] = s.ConfirmedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := rows[i]["scope"].(string)
		b, _ := rows[j]["scope"].(string)
		return a < b
	})
	r.Cache["scopes"] = rows
	r.Cache["bytes"] = bytes

	switch {
	case unreadable > 0:
		add(Check{Name: "cache", Status: CheckWarn,
			Detail: fmt.Sprintf("%d of %d cache scopes are unreadable", unreadable, len(scopes)),
			Hint:   "pay cache clear --all"})
	default:
		add(Check{Name: "cache", Status: CheckOK,
			Detail: fmt.Sprintf("%d scope(s), %d bytes under %s", len(scopes), bytes, store.Root()),
			Value:  bytes})
	}

	// A cache write that keeps failing is the difference between "warm" and
	// "re-paying discovery on every invocation" (§8.7), so it is reported
	// explicitly rather than inferred from latency.
	writeFailing := false
	for _, w := range rt.Warnings() {
		if w.Code == "cache_write_failed" {
			writeFailing = true
		}
	}
	if writeFailing {
		add(Check{Name: "cache_writes", Status: CheckFail,
			Detail: "a cache write failed during this run; discovery will be re-paid on every command",
			Hint:   "check permissions on " + store.Root()})
	} else {
		add(Check{Name: "cache_writes", Status: CheckOK, Detail: "cache writes are succeeding"})
	}

	// Fingerprint drift: the cached topology versus the one just observed.
	if sc, err := rt.Scope(); err == nil {
		if cm, ok, _ := store.ReadManifest(sc); ok {
			switch {
			case cm.Fingerprint.TopologySHA256 == "":
				add(Check{Name: "cache_fingerprint", Status: CheckUnknown,
					Detail: "the cached manifest carries no topology fingerprint"})
			case cm.Fingerprint.TopologySHA256 != m.Fingerprint.TopologySHA256:
				add(Check{Name: "cache_fingerprint", Status: CheckWarn,
					Detail: "the cached scope's topology fingerprint no longer matches the server",
					Hint:   "pay discover --refresh"})
			default:
				add(Check{Name: "cache_fingerprint", Status: CheckOK,
					Detail: "the cached scope matches the server's current topology"})
			}
		}
	}
}

func (rt *Runtime) skillChecks(r *doctorReport, m *discovery.Manifest, add func(Check)) {
	opts := skills.Options{
		Scope:      skills.ScopeProject,
		StartDir:   rt.App.WorkDir,
		Home:       rt.Env.Get("HOME"),
		CLIVersion: buildinfo.Version(),
		Now:        rt.Now(),
	}
	status, err := skills.Status(opts)
	if err != nil || status == nil {
		add(Check{Name: "skill", Status: CheckUnknown,
			Detail: "the agent skill's install state could not be read",
			Hint:   "pay skills status"})
		return
	}
	r.Skill["scope"] = string(status.Scope)
	r.Skill["root"] = status.Root
	installed, stale := 0, 0
	targets := make([]map[string]any, 0, len(status.Targets))
	for _, t := range status.Targets {
		if t.Installed {
			installed++
		}
		if t.Stale || t.ProjectDocStale {
			stale++
		}
		targets = append(targets, map[string]any{
			"agent": t.Agent, "dir": t.Dir, "installed": t.Installed,
			"cli_version": t.CLIVersion, "stale": t.Stale,
			"project_doc_stale":  t.ProjectDocStale,
			"discovery_revision": t.DiscoveryRevision,
		})
	}
	r.Skill["targets"] = targets
	r.Skill["installed"] = installed
	r.Skill["stale"] = stale
	switch {
	case installed == 0:
		add(Check{Name: "skill", Status: CheckWarn,
			Detail: "the pay agent skill is not installed in this project",
			Hint:   "pay skills install --with-project-context"})
	case stale > 0:
		add(Check{Name: "skill", Status: CheckWarn,
			Detail: fmt.Sprintf("%d installed skill target(s) are stale (CLI %s, discovery %s)",
				stale, buildinfo.Version(), m.Revision()),
			Hint: "pay skills update"})
	default:
		add(Check{Name: "skill", Status: CheckOK,
			Detail: fmt.Sprintf("installed and current in %d target(s)", installed)})
	}
}

func (rt *Runtime) updateChecks(r *doctorReport, add func(Check)) {
	state, err := update.LoadState(rt.Paths.UpdateStateFile())
	if err != nil {
		add(Check{Name: "update", Status: CheckUnknown,
			Detail: "the update state file could not be read", Hint: "pay version --check"})
		return
	}
	managed := update.DetectManaged(rt.binaryPath(), rt.updateEnv())
	r.Update["current_version"] = buildinfo.Version()
	r.Update["channel"] = rt.updateChannel()
	r.Update["pending_version"] = state.PendingVersion
	r.Update["verification"] = string(state.Verification)
	r.Update["managed"] = managed.Managed
	r.Update["manager"] = managed.Manager
	if !state.LastCheck.IsZero() {
		r.Update["last_check"] = state.LastCheck.UTC().Format(time.RFC3339)
	}
	switch {
	case !state.HasPending(buildinfo.Version()):
		add(Check{Name: "update", Status: CheckOK,
			Detail: "no newer release is pending (current " + buildinfo.Version() + ")"})
	case state.Verification == update.OutcomeFailed:
		add(Check{Name: "update", Status: CheckFail,
			Detail: "release " + state.PendingVersion + " failed verification and will not be applied",
			Hint:   "pay update-self --check"})
	case managed.Managed:
		add(Check{Name: "update", Status: CheckWarn,
			Detail: "release " + state.PendingVersion + " is available, but this binary is managed by " + managed.Manager,
			Hint:   managed.Hint})
	default:
		add(Check{Name: "update", Status: CheckWarn,
			Detail: "release " + state.PendingVersion + " is available (" + string(state.Verification) + ")",
			Hint:   "pay update-self --apply"})
	}
}

func (rt *Runtime) binaryPath() string {
	if exe, err := update.ResolveBinary(); err == nil {
		return exe
	}
	return ""
}

func (rt *Runtime) finishReport(r *doctorReport) {
	for _, c := range r.Checks {
		switch c.Status {
		case CheckOK:
			r.Summary.OK++
		case CheckWarn:
			r.Summary.Warn++
		case CheckFail:
			r.Summary.Fail++
		case CheckUnknown:
			r.Summary.Unknown++
		}
	}
	r.OK = r.Summary.Fail == 0
	if r.Checks == nil {
		r.Checks = []Check{}
	}
}

// authCollections splits the inventory into "is an auth collection" and "an API
// key can actually authenticate here" (§5.4). The second list is tri-state: a
// nil use_api_key flag means the fact was never learned.
func authCollections(m *discovery.Manifest) (auth []string, apiKey []string, unknown bool) {
	auth, apiKey = []string{}, []string{}
	for _, c := range m.Collections {
		if c.Flags.Auth == nil || !*c.Flags.Auth {
			continue
		}
		auth = append(auth, c.Slug)
		switch {
		case c.Flags.UseAPIKey == nil:
			unknown = true
		case *c.Flags.UseAPIKey:
			apiKey = append(apiKey, c.Slug)
		}
	}
	sort.Strings(auth)
	sort.Strings(apiKey)
	return auth, apiKey, unknown
}

func joinOr(list []string, fallback string) string {
	if len(list) == 0 {
		return fallback
	}
	out := list[0]
	var outSb570 strings.Builder
	for _, s := range list[1:] {
		outSb570.WriteString(", " + s)
	}
	out += outSb570.String()
	return out
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
