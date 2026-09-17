package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func init() { Register(newConfigCmd) }

func newConfigCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "Inspect and edit PayCLI's own configuration (§4).",
		GroupID: GroupAdmin,
		RunE: Handle(rt, "config", func(_ context.Context, _ *Runtime, _ []string) (*output.Envelope, error) {
			return nil, apierr.New(apierr.CodeInvalidArgs, "pay config needs a subcommand").
				WithHint("pay config explain   # every effective setting and where it came from").
				WithDidYouMean("get", "set", "unset", "list", "paths", "explain")
		}),
	}
	cmd.AddCommand(
		newConfigGetCmd(rt),
		newConfigSetCmd(rt),
		newConfigUnsetCmd(rt),
		newConfigListCmd(rt),
		newConfigPathsCmd(rt),
		newConfigExplainCmd(rt),
	)
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay config get|set|unset|list|paths|explain"},
		Long: "Values resolve in one fixed order (§4.5): flag, then PAY_* environment variable,\n" +
			"then ./pay.toml, then the user config.toml, then the built-in default.\n" +
			"`pay config explain` prints every effective setting together with which of those\n" +
			"five layers produced it — start there when a setting is not what you expected.",
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "where does everything live on this machine?", Cmd: "pay config paths"},
			{Why: "why is --depth 2 when I did not ask for it?", Cmd: "pay config explain --path .depth"},
			{Why: "make table the default output for this user", Cmd: "pay config set defaults.output table"},
			{Why: "point the default profile at a local dev server", Cmd: "pay config set profiles.default.base_url http://localhost:3000"},
		},
		Mistakes: []Mistake{
			{Wrong: "Putting `api_key = \"…\"` in config.toml.",
				Right: "PayCLI refuses to load such a file (config_secret_in_plaintext, exit 9). Use `pay auth login --api-key-stdin`, which writes credentials.json with mode 0600."},
			{Wrong: "Editing config.toml while expecting an env var to lose.",
				Right: "PAY_* beats both config files. Unset the variable or pass the flag, which beats everything."},
			{Wrong: "Assuming `pay config set` edits ./pay.toml.",
				Right: "It edits the user config file. Edit ./pay.toml by hand to pin a project; `pay config paths` prints both locations."},
		},
		SeeAlso: []string{"pay auth status", "pay doctor", "pay explain --section connection"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------------

func newConfigPathsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "paths",
		Short: "Print every directory and file PayCLI reads or writes (§4.1).",
		Args:  maxArgs(0, "pay config paths"),
		RunE: Handle(rt, "config paths", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			data := map[string]any{}
			for k, v := range rt.Paths.Map() {
				data[k] = v
			}
			data["sources"] = rt.Paths.Sources
			data["project_root"] = rt.Project.Dir
			data["project_config"] = rt.Project.ConfigPath
			data["config_file_exists"] = rt.UserFile != nil && rt.UserFile.Exists
			data["project_file_exists"] = rt.ProjectFile != nil && rt.ProjectFile.Exists
			return output.New("config paths", output.KindOpResult, data), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config paths"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"config_dir":"…","cache_dir":"…","state_dir":"…","config_file":"…","credentials_file":"…","audit_file":"…"}`},
		ExitCodes: []int{apierr.ExitOK},
		Examples: []Example{
			{Why: "all of them", Cmd: "pay config paths"},
			{Why: "just the cache directory", Cmd: "pay config paths --path .cache_dir --output id"},
			{Why: "which env var chose each directory", Cmd: "pay config paths --path .sources"},
			{Why: "back up the credential file", Cmd: "cp \"$(pay config paths --path .credentials_file --output id)\" ~/pay-creds.bak"},
		},
		Mistakes: []Mistake{
			{Wrong: "Hardcoding ~/.config/pay.",
				Right: "PAY_HOME, XDG_CONFIG_HOME and the Windows layout all move it; read .config_dir from this command."},
			{Wrong: "Editing credentials.json by hand.",
				Right: "Use `pay auth login` / `pay auth logout`; the file is 0600 and PayCLI refuses to read it otherwise."},
			{Wrong: "Deleting the cache directory to fix a stale schema.",
				Right: "`pay cache clear --all` or `pay discover --refresh` does it without racing a concurrent run."},
		},
		SeeAlso: []string{"pay config explain", "pay cache path"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// explain
// ---------------------------------------------------------------------------

func newConfigExplainCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Print every effective setting with the layer that produced it (§4.5).",
		Args:  maxArgs(0, "pay config explain"),
		RunE: Handle(rt, "config explain", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			settings := rt.Cfg.Explain()
			data := make(map[string]any, len(settings))
			for k, v := range settings {
				data[k] = v
			}
			return output.New("config explain", output.KindOpResult, data), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config explain"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"base_url":{"value":"http://localhost:3900","source":"env:PAY_BASE_URL"},"depth":{"value":0,"source":"default"}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitConfig},
		Examples: []Example{
			{Why: "everything, with provenance", Cmd: "pay config explain"},
			{Why: "where did base_url come from?", Cmd: "pay config explain --path .base_url"},
			{Why: "confirm a profile switch took effect", Cmd: "pay config explain --profile staging --path .base_url.value --output id"},
			{Why: "check the concurrency PayCLI will actually use", Cmd: "pay config explain --path .concurrency"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the API key here.",
				Right: "Secrets never appear in any output. `pay auth status` prints the key fingerprint and its source."},
			{Wrong: "Reading .value without .source and then debugging the wrong layer.",
				Right: "The source string names the exact file or variable; change that one."},
			{Wrong: "Expecting header values to be shown.",
				Right: "All configured header values are treated as secret and masked; only names are printed."},
		},
		SeeAlso: []string{"pay config paths", "pay doctor"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// get / set / unset / list
// ---------------------------------------------------------------------------

func newConfigGetCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Print one effective setting, or one raw key from a config file.",
		Args:  exactArgs(1, "pay config get <key>"),
		RunE: Handle(rt, "config get", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			key := args[0]
			settings := rt.Cfg.Explain()
			if s, ok := settings[key]; ok {
				return output.New("config get", output.KindOpResult, map[string]any{
					"key": key, "value": s.Value, "source": s.Source,
				}), nil
			}
			for _, layer := range []*config.File{rt.ProjectFile, rt.UserFile} {
				if layer == nil || !layer.Exists {
					continue
				}
				doc, err := decodeTOMLFile(layer.Path)
				if err != nil {
					return nil, err
				}
				if v, ok := tomlLookup(doc, strings.Split(key, ".")); ok {
					return output.New("config get", output.KindOpResult, map[string]any{
						"key": key, "value": maskConfigValue(key, v), "source": layer.Source(),
					}), nil
				}
			}
			keys := make([]string, 0, len(settings))
			for k := range settings {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"no setting or config key named %q", key).
				WithHint("pay config explain lists every effective setting; pay config list prints the raw files").
				WithDidYouMean(apierr.DidYouMean(key, keys)...)
		}),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if !rt.completionSetup(cmd) || len(args) > 0 {
				return noSuggestions()
			}
			var out []string
			for k := range rt.Cfg.Explain() {
				if strings.HasPrefix(k, toComplete) {
					out = append(out, k)
				}
			}
			sort.Strings(out)
			return out, cobra.ShellCompDirectiveNoFileComp
		},
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config get <key>"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"key":"limit","value":20,"source":"default"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "the effective page size", Cmd: "pay config get limit"},
			{Why: "the effective base URL only", Cmd: "pay config get base_url --path .value --output id"},
			{Why: "a raw key from the file, not the resolved value", Cmd: "pay config get profiles.default.api_path"},
			{Why: "which layer set the output format", Cmd: "pay config get output --path .source --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Using a TOML path (`defaults.limit`) for a resolved setting.",
				Right: "Resolved settings are flat (`limit`); TOML paths are the fallback for keys PayCLI does not resolve."},
			{Wrong: "Expecting `get` to read credentials.",
				Right: "Use `pay auth status`; the key itself is never printed anywhere."},
			{Wrong: "Scripting against the whole object.",
				Right: "Add `--path .value --output id` to get the bare value on stdout."},
		},
		SeeAlso: []string{"pay config set", "pay config explain"},
	})
	return cmd
}

func newConfigSetCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write one key into the user config file (atomic, mode 0600).",
		Args:  exactArgs(2, "pay config set <key> <value>"),
		RunE: Handle(rt, "config set", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			return rt.editConfig("config set", args[0], args[1], false)
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config set <key> <value>"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"key":"defaults.output","value":"table","path":"/home/u/.config/pay/config.toml","written":true}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation, apierr.ExitConfig, apierr.ExitInternal},
		Examples: []Example{
			{Why: "default every command to table output", Cmd: "pay config set defaults.output table"},
			{Why: "raise the default page size", Cmd: "pay config set defaults.limit 100"},
			{Why: "pin a profile's endpoint", Cmd: "pay config set profiles.staging.base_url https://staging.example.com"},
			{Why: "make staging the default profile", Cmd: "pay config set default_profile staging"},
		},
		Mistakes: []Mistake{
			{Wrong: "`pay config set profiles.default.api_key sk-…`",
				Right: "Refused with config_secret_in_plaintext (exit 9). Run `pay auth login --api-key-stdin` instead."},
			{Wrong: "Quoting a number (`pay config set defaults.limit \"100\"`).",
				Right: "Values are typed the way JSON types them; 100 becomes an integer, \"100\" a string that then fails validation."},
			{Wrong: "Expecting the change to affect a shell that already exported PAY_LIMIT.",
				Right: "Environment variables beat both config files; unset it or use the flag."},
		},
		SeeAlso: []string{"pay config unset", "pay config list", "pay config explain"},
	})
	return cmd
}

func newConfigUnsetCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove one key from the user config file.",
		Args:  exactArgs(1, "pay config unset <key>"),
		RunE: Handle(rt, "config unset", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			return rt.editConfig("config unset", args[0], "", true)
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config unset <key>"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"key":"defaults.output","removed":true,"path":"…"}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation, apierr.ExitInternal},
		Examples: []Example{
			{Why: "go back to the built-in default output", Cmd: "pay config unset defaults.output"},
			{Why: "drop a whole profile", Cmd: "pay config unset profiles.staging"},
			{Why: "stop pinning a locale", Cmd: "pay config unset defaults.locale"},
			{Why: "confirm what is left", Cmd: "pay config list --path .user"},
		},
		Mistakes: []Mistake{
			{Wrong: "Unsetting a key to remove a credential.",
				Right: "Credentials live in credentials.json; use `pay auth logout`."},
			{Wrong: "Expecting an error when the key is absent.",
				Right: "Unset is idempotent and reports removed:false with exit 0."},
			{Wrong: "Unsetting `default_profile` while several profiles exist.",
				Right: "PayCLI then needs --profile on every command; set it again or keep exactly one profile."},
		},
		SeeAlso: []string{"pay config set", "pay config list"},
	})
	return cmd
}

func newConfigListCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print the raw contents of both config files, redacted.",
		Args:  maxArgs(0, "pay config list"),
		RunE: Handle(rt, "config list", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			data := map[string]any{
				"user":     map[string]any{},
				"project":  map[string]any{},
				"profiles": config.ProfileNames(rt.ProjectFile, rt.UserFile),
			}
			for label, layer := range map[string]*config.File{"user": rt.UserFile, "project": rt.ProjectFile} {
				if layer == nil || !layer.Exists {
					continue
				}
				doc, err := decodeTOMLFile(layer.Path)
				if err != nil {
					return nil, err
				}
				data[label] = map[string]any{"path": layer.Path, "contents": maskConfigTree(doc, "")}
			}
			return output.New("config list", output.KindOpResult, data), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay config list"},
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"user":{"path":"…","contents":{…}},"project":{…},"profiles":["default"]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitConfig},
		Examples: []Example{
			{Why: "both files at once", Cmd: "pay config list"},
			{Why: "just the user file", Cmd: "pay config list --path .user.contents"},
			{Why: "profile names for a script", Cmd: "pay config list --path .profiles[]"},
			{Why: "check whether a project pin exists", Cmd: "pay config list --path .project.path --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating this as the effective configuration.",
				Right: "It is the files as written; `pay config explain` applies flags, env and defaults."},
			{Wrong: "Expecting header values.",
				Right: "Every value under a headers table is treated as secret and printed as <redacted>."},
			{Wrong: "Editing the file while a long `--all` run is in flight.",
				Right: "Configuration is read once at start-up; finish the run or restart it."},
		},
		SeeAlso: []string{"pay config explain", "pay config paths"},
	})
	return cmd
}

// editConfig applies one set/unset to the user config file. The whole file is
// decoded, mutated and re-encoded, then re-parsed through config.Parse before
// it is written, so PayCLI can never leave behind a file it would itself refuse
// to load — including one carrying an api_key (§4.2).
func (rt *Runtime) editConfig(command, key, value string, remove bool) (*output.Envelope, error) {
	segments := strings.Split(key, ".")
	for _, seg := range segments {
		if seg == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"%q is not a valid config key: empty path segment", key).
				WithHint("keys are dotted TOML paths, e.g. defaults.output or profiles.default.base_url")
		}
		if strings.EqualFold(seg, "api_key") {
			return nil, apierr.New(apierr.CodeConfigSecretInPlaintext,
				"api_key must never be written to a config file").
				WithHint("pay auth login --profile %s --api-key-stdin", rt.Cfg.Profile)
		}
	}

	path := rt.Paths.ConfigFile
	doc, err := decodeTOMLFile(path)
	if err != nil {
		return nil, err
	}

	changed := false
	if remove {
		changed = tomlDelete(doc, segments)
	} else {
		if err := tomlSet(doc, segments, typedConfigValueFor(segments, value)); err != nil {
			return nil, err
		}
		changed = true
	}

	if changed {
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
			return nil, apierr.Wrap(err, apierr.CodeInternal, "could not encode %s", path)
		}
		if _, err := config.Parse(buf.Bytes(), path, config.KindUser); err != nil {
			return nil, err
		}
		if err := fsatomic.Write(path, buf.Bytes(), 0o600); err != nil {
			return nil, apierr.Wrap(err, apierr.CodeInternal, "could not write %s", path)
		}
	}

	data := map[string]any{"key": key, "path": path}
	if remove {
		data["removed"] = changed
	} else {
		data["value"] = typedConfigValueFor(segments, value)
		data["written"] = changed
	}
	return output.New(command, output.KindOpResult, data), nil
}

func decodeTOMLFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, apierr.Wrap(err, apierr.CodeConfigMissing, "could not read %s", path)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInvalidArgs, "%s is not valid TOML: %s", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func tomlLookup(doc map[string]any, segments []string) (any, bool) {
	cur := any(doc)
	for _, seg := range segments {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func tomlSet(doc map[string]any, segments []string, value any) error {
	cur := doc
	for _, seg := range segments[:len(segments)-1] {
		next, ok := cur[seg]
		if !ok {
			child := map[string]any{}
			cur[seg] = child
			cur = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return apierr.New(apierr.CodeInvalidArgs,
				"cannot set %q: %q already holds a value, not a table",
				strings.Join(segments, "."), seg)
		}
		cur = child
	}
	cur[segments[len(segments)-1]] = value
	return nil
}

func tomlDelete(doc map[string]any, segments []string) bool {
	cur := doc
	for _, seg := range segments[:len(segments)-1] {
		child, ok := cur[seg].(map[string]any)
		if !ok {
			return false
		}
		cur = child
	}
	last := segments[len(segments)-1]
	if _, ok := cur[last]; !ok {
		return false
	}
	delete(cur, last)
	return true
}

// typedConfigValue types a command-line value the way §9.10 types --set: JSON
// first (so true, 42 and ["a","b"] are what they look like), string otherwise.
// configListKeys are the config leaves typed as a TOML array of strings.
// `blocks` is the one map-of-lists: profiles.<p>.blocks.<field> is a list too,
// so the SECOND-to-last segment is checked as well as the last.
var configListKeys = map[string]bool{
	"locales":           true,
	"echo_check_ignore": true,
	"custom_endpoints":  true,
}

// typedConfigValueFor types a value the way typedConfigValue does, except that
// a bare comma list written into a list-typed key becomes a list.
//
// PayCLI PRINTS `pay config set profiles.<p>.locales en,de` as a hint (§7.10
// requires every hint to be literally runnable, and the string is mandated
// verbatim by the spec and by discovery/locales.go), but the generic typer
// produced the string "en,de", which config.Parse then rejected with
// "TOML value has type string; destination has type slice". The advice PayCLI
// gives for pinning locales could not be followed.
func typedConfigValueFor(segments []string, raw string) any {
	if len(segments) == 0 {
		return typedConfigValue(raw)
	}
	last := strings.ToLower(segments[len(segments)-1])
	isList := configListKeys[last]
	if !isList && len(segments) >= 2 {
		isList = strings.EqualFold(segments[len(segments)-2], "blocks")
	}
	trimmed := strings.TrimSpace(raw)
	if !isList || trimmed == "" {
		return typedConfigValue(raw)
	}
	// An explicit JSON/TOML array still wins, so the documented ["en","de"]
	// form keeps working unchanged.
	if trimmed[0] == '[' {
		return typedConfigValue(raw)
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func typedConfigValue(raw string) any {
	trimmed := strings.TrimSpace(raw)
	switch trimmed {
	case "true":
		return true
	case "false":
		return false
	}
	if trimmed != "" && (trimmed[0] == '[' || trimmed[0] == '{' || trimmed[0] == '"' ||
		trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9')) {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err == nil {
			return untypedNumbers(v)
		}
	}
	return raw
}

// untypedNumbers turns json.Number into int64 or float64.
//
// encoding/json's default float64 would make `pay config set defaults.limit 50`
// write `limit = 50.0`, which the TOML loader then rejects as a float where an
// integer belongs — PayCLI would have written a file it refuses to read.
func untypedNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]any:
		for k, child := range t {
			t[k] = untypedNumbers(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = untypedNumbers(child)
		}
		return t
	default:
		return v
	}
}

// maskConfigValue applies §5.3 to a raw file value before it is printed. The
// key is the dotted path the value was read from, so the walk below knows
// whether the value sits under a headers table even when the caller asked for
// a leaf directly.
func maskConfigValue(key string, v any) any {
	last := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		last = key[i+1:]
	}
	if redact.Key(last) {
		return redact.Mask
	}
	return maskConfigTree(v, key)
}

// maskConfigTree applies §5.3 to a decoded config tree before it is printed.
//
// redact.Value only knows key *names*, so a profile header called X-Tenant,
// CF-Access-Client-Id or X-Custom-Thing walks straight through it. §5.3 makes
// every value under a [profiles.X.headers] table a secret whatever it is
// named, so this walk carries the dotted path and masks any value one of whose
// ancestor segments is `headers`. Matching a whole segment (not a substring)
// keeps a key literally named "x-headers-hint" printable; over-masking inside a
// real headers table is the safe direction.
func maskConfigTree(v any, path string) any {
	if underHeaders(path) {
		return redact.Mask
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			if redact.Key(k) {
				out[k] = redact.Mask
				continue
			}
			out[k] = maskConfigTree(child, joinConfigPath(path, k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = maskConfigTree(child, joinConfigPath(path, "[]"))
		}
		return out
	default:
		masked, _ := redact.Value(v)
		return masked
	}
}

// underHeaders reports whether the value at this dotted path is *inside* a
// headers table: every segment but the last is an ancestor.
func underHeaders(path string) bool {
	if path == "" {
		return false
	}
	segments := strings.Split(path, ".")
	for _, seg := range segments[:len(segments)-1] {
		if strings.EqualFold(seg, "headers") {
			return true
		}
	}
	return false
}

func joinConfigPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
